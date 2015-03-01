package messaging

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/influxdb/influxdb/raft"
)

const (
	// BroadcastTopicID is the topic used to communicate with all replicas.
	BroadcastTopicID = uint64(0)

	// MaxSegmentSize represents the largest size a segment can be before a
	// new segment is started.
	MaxSegmentSize = 10 * 1024 * 1024 // 10MB
)

// Broker represents distributed messaging system segmented into topics.
// Each topic represents a linear series of events.
type Broker struct {
	mu    sync.RWMutex
	path  string    // data directory
	index uint64    // highest applied index
	log   *raft.Log // internal raft log

	replicas map[uint64]*Replica // replica by id
	topics   map[uint64]*topic   // topics by id

	Logger *log.Logger
}

// NewBroker returns a new instance of a Broker with default values.
func NewBroker() *Broker {
	b := &Broker{
		log:      raft.NewLog(),
		replicas: make(map[uint64]*Replica),
		topics:   make(map[uint64]*topic),
		Logger:   log.New(os.Stderr, "[broker] ", log.LstdFlags),
	}
	b.log.FSM = (*brokerFSM)(b)
	return b
}

// Path returns the path used when opening the broker.
// Returns empty string if the broker is not open.
func (b *Broker) Path() string { return b.path }

// Log returns the underlying raft log.
func (b *Broker) Log() *raft.Log { return b.log }

// metaPath returns the file path to the broker's metadata file.
func (b *Broker) metaPath() string {
	if b.path == "" {
		return ""
	}
	return filepath.Join(b.path, "meta")
}

// Index returns the highest index seen by the broker across all topics.
// Returns 0 if the broker is closed.
func (b *Broker) Index() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.index
}

// opened returns true if the broker is in an open and running state.
func (b *Broker) opened() bool { return b.path != "" }

// SetLogOutput sets writer for all Broker log output.
func (b *Broker) SetLogOutput(w io.Writer) {
	b.Logger = log.New(w, "[broker] ", log.LstdFlags)
	b.log.SetLogOutput(w)
}

// Open initializes the log.
// The broker then must be initialized or join a cluster before it can be used.
func (b *Broker) Open(path string, u *url.URL) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Require a non-blank path.
	if path == "" {
		return ErrPathRequired
	}
	b.path = path

	// Require a non-blank connection address.
	if u == nil {
		return ErrConnectionAddressRequired
	}

	// Read meta data from snapshot.
	if err := b.load(); err != nil {
		_ = b.close()
		return err
	}

	// Open underlying raft log.
	if err := b.log.Open(filepath.Join(path, "raft")); err != nil {
		_ = b.close()
		return fmt.Errorf("raft: %s", err)
	}

	// Copy connection URL.
	b.log.URL = &url.URL{}
	*b.log.URL = *u

	return nil
}

// Close closes the broker and all topics.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.close()
}

func (b *Broker) close() error {
	// Return error if the broker is already closed.
	if !b.opened() {
		return ErrClosed
	}
	b.path = ""

	// Close all topics & replicas.
	b.closeTopics()
	b.closeReplicas()

	// Close raft log.
	_ = b.log.Close()

	return nil
}

// closeTopics closes all topic files and clears the topics map.
func (b *Broker) closeTopics() {
	for _, t := range b.topics {
		_ = t.Close()
	}
	b.topics = make(map[uint64]*topic)
}

// closeReplicas closes all replica writers and clears the replica map.
func (b *Broker) closeReplicas() {
	for _, r := range b.replicas {
		r.closeWriter()
	}
	b.replicas = make(map[uint64]*Replica)
}

// load reads the broker metadata from disk.
func (b *Broker) load() error {
	// Read snapshot header from disk.
	// Ignore if no snapshot exists.
	f, err := os.Open(b.metaPath())
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Read snapshot header from disk.
	hdr := &snapshotHeader{}
	if err := json.NewDecoder(f).Decode(&hdr); err != nil {
		return err
	}

	// Copy topic files from snapshot to local disk.
	for _, st := range hdr.Topics {
		t := b.newTopic(st.ID)

		// Ignore segment data in the snapshot since we have the data locally.
		// Simply opening the topic will automatically build the segments object.
		if err := t.open(); err != nil {
			return fmt.Errorf("open topic: %s", err)
		}
	}

	// Update the replicas.
	for _, sr := range hdr.Replicas {
		// Create replica.
		r := newReplica(b, sr.ID, sr.URL)
		b.replicas[r.id] = r

		// Append replica's topics.
		for _, topicID := range sr.TopicIDs {
			r.topics[topicID] = struct{}{}
		}
	}

	// Read the highest index from each of the topic files.
	if err := b.loadIndex(); err != nil {
		return fmt.Errorf("load index: %s", err)
	}

	return nil
}

// loadIndex reads through all topics to find the highest known index.
func (b *Broker) loadIndex() error {
	for _, t := range b.topics {
		if err := t.loadIndex(); err != nil {
			return fmt.Errorf("topic(%d): %s", t.id, err)
		} else if t.index > b.index {
			b.index = t.index
		}
	}
	return nil
}

// save persists the broker metadata to disk.
func (b *Broker) save() error {
	if b.path == "" {
		return ErrClosed
	}

	// Calculate header under lock.
	hdr, err := b.createSnapshotHeader()
	if err != nil {
		return fmt.Errorf("create snapshot: %s", err)
	}

	// Write snapshot to disk.
	f, err := os.Create(b.metaPath())
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Write snapshot to disk.
	if err := json.NewEncoder(f).Encode(&hdr); err != nil {
		return err
	}

	return nil
}

// mustSave persists the broker metadata to disk. Panic on error.
func (b *Broker) mustSave() {
	if err := b.save(); err != nil && err != ErrClosed {
		panic(err.Error())
	}
}

// createSnapshotHeader creates a snapshot header.
func (b *Broker) createSnapshotHeader() (*snapshotHeader, error) {
	// Create parent header.
	sh := &snapshotHeader{}

	// Append topics.
	for _, t := range b.topics {
		// Create snapshot topic.
		st := &snapshotTopic{ID: t.id}

		// Add segments to topic.
		for _, s := range t.segments {
			// Retrieve current segment file size from disk.
			var size int64
			fi, err := os.Stat(s.path)
			if os.IsNotExist(err) {
				size = 0
			} else if err == nil {
				size = fi.Size()
			} else {
				return nil, fmt.Errorf("stat segment: %s", err)
			}

			// Append segment.
			st.Segments = append(st.Segments, &snapshotTopicSegment{
				Index: s.index,
				Size:  size,
				path:  s.path,
			})

			// Bump the snapshot header max index.
			if s.index > sh.Index {
				sh.Index = s.index
			}
		}

		// Append topic to the snapshot.
		sh.Topics = append(sh.Topics, st)
	}

	// Append replicas and the current index for each topic.
	for _, r := range b.replicas {
		sr := &snapshotReplica{ID: r.id, URL: r.URL.String()}

		for topicID := range r.topics {
			sr.TopicIDs = append(sr.TopicIDs, topicID)
		}

		sh.Replicas = append(sh.Replicas, sr)
	}

	return sh, nil
}

// URL returns the connection url for the broker.
func (b *Broker) URL() *url.URL {
	return b.log.URL
}

// LeaderURL returns the connection url for the leader broker.
func (b *Broker) LeaderURL() *url.URL {
	_, u := b.log.Leader()
	return u
}

// IsLeader returns true if the broker is the current leader.
func (b *Broker) IsLeader() bool { return b.log.State() == raft.Leader }

// Initialize creates a new cluster.
func (b *Broker) Initialize() error {
	if err := b.log.Initialize(); err != nil {
		return fmt.Errorf("raft: %s", err)
	}
	return nil
}

// Join joins an existing cluster.
func (b *Broker) Join(u *url.URL) error {
	if err := b.log.Join(u); err != nil {
		return fmt.Errorf("raft: %s", err)
	}
	return nil
}

// Publish writes a message.
// Returns the index of the message. Otherwise returns an error.
func (b *Broker) Publish(m *Message) (uint64, error) {
	buf, _ := m.MarshalBinary()
	return b.log.Apply(buf)
}

// PublishSync writes a message and waits until the change is applied.
func (b *Broker) PublishSync(m *Message) error {
	// Publish message.
	index, err := b.Publish(m)
	if err != nil {
		return err
	}

	// Wait for message to apply.
	if err := b.Sync(index); err != nil {
		return err
	}

	return nil
}

// Sync pauses until the given index has been applied.
func (b *Broker) Sync(index uint64) error { return b.log.Wait(index) }

// Replica returns a replica by id.
func (b *Broker) Replica(id uint64) *Replica {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.replicas[id]
}

// Replicas returns a list of the replicas in the system
func (b *Broker) Replicas() []*Replica {
	b.mu.RLock()
	defer b.mu.RUnlock()
	a := make([]*Replica, 0, len(b.replicas))
	for _, r := range b.replicas {
		a = append(a, r)
	}
	sort.Sort(replicas(a))
	return a
}

// minReplicaTopicIndex returns the lowest index replicated for all replicas
// subscribed to a given topic. Requires a lock.
func (b *Broker) minReplicaTopicIndex(topicID uint64) uint64 {
	var index uint64
	var updated bool

	for _, r := range b.replicas {
		// Ignore replicas that are not subscribed.
		if _, ok := r.topics[topicID]; !ok {
			continue
		}

		// Move the index down if unset or a lowest index is found.
		if !updated || r.index < index {
			index = r.index
			updated = true
		}
	}

	return index
}

// initializes a new topic object. Requires lock.
func (b *Broker) newTopic(id uint64) *topic {
	t := &topic{
		id:       id,
		path:     filepath.Join(b.path, strconv.FormatUint(uint64(id), 10)),
		replicas: make(map[uint64]*Replica),
	}
	b.topics[t.id] = t
	return t
}

// creates and opens a topic if it doesn't already exist. Requires lock.
func (b *Broker) createTopicIfNotExists(id uint64) (*topic, error) {
	if t := b.topics[id]; t != nil {
		return t, nil
	}

	// Create topic and save metadata.
	t := b.newTopic(id)
	if err := b.save(); err != nil {
		return nil, fmt.Errorf("save: %s", err)
	}

	// Open topic.
	if err := t.open(); err != nil {
		return nil, fmt.Errorf("open topic: %s", err)
	}

	return t, nil
}

func (b *Broker) mustCreateTopicIfNotExists(id uint64) *topic {
	t, err := b.createTopicIfNotExists(id)
	if err != nil {
		panic(err.Error())
	}
	return t
}

// CreateReplica creates a new named replica.
func (b *Broker) CreateReplica(id uint64, connectURL *url.URL) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Ensure replica doesn't already exist.
	s := b.replicas[id]
	if s != nil {
		return ErrReplicaExists
	}

	// Add command to create replica.
	return b.PublishSync(&Message{
		Type: CreateReplicaMessageType,
		Data: mustMarshalJSON(&CreateReplicaCommand{ID: id, URL: connectURL.String()}),
	})
}

func (b *Broker) mustApplyCreateReplica(m *Message) {
	var c CreateReplicaCommand
	mustUnmarshalJSON(m.Data, &c)

	// Create replica.
	r := newReplica(b, c.ID, c.URL)

	// Automatically subscribe to the config topic.
	b.createTopicIfNotExists(BroadcastTopicID)
	r.topics[BroadcastTopicID] = struct{}{}

	// Add replica to the broker.
	b.replicas[c.ID] = r

	b.mustSave()
}

// DeleteReplica deletes an existing replica by id.
func (b *Broker) DeleteReplica(id uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Ensure replica exists.
	if s := b.replicas[id]; s == nil {
		return ErrReplicaNotFound
	}

	// Issue command to remove replica.
	return b.PublishSync(&Message{
		Type: DeleteReplicaMessageType,
		Data: mustMarshalJSON(&DeleteReplicaCommand{ID: id}),
	})
}

func (b *Broker) mustApplyDeleteReplica(m *Message) {
	var c DeleteReplicaCommand
	mustUnmarshalJSON(m.Data, &c)

	// Find replica.
	r := b.replicas[c.ID]
	if r == nil {
		return
	}

	// Remove replica from all subscribed topics.
	for topicID := range r.topics {
		if t := b.topics[topicID]; t != nil {
			delete(t.replicas, r.id)
		}
	}
	r.topics = make(map[uint64]struct{})

	// Close replica's writer.
	r.closeWriter()

	// Remove replica from broker.
	delete(b.replicas, c.ID)

	b.mustSave()
}

// Subscribe adds a subscription to a topic from a replica.
func (b *Broker) Subscribe(replicaID, topicID uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// TODO: Allow non-zero starting index.

	// Ensure replica & topic exist.
	if b.replicas[replicaID] == nil {
		return ErrReplicaNotFound
	}

	// Issue command to subscribe to topic.
	return b.PublishSync(&Message{
		Type: SubscribeMessageType,
		Data: mustMarshalJSON(&SubscribeCommand{ReplicaID: replicaID, TopicID: topicID}),
	})
}

func (b *Broker) mustApplySubscribe(m *Message) {
	var c SubscribeCommand
	mustUnmarshalJSON(m.Data, &c)

	// Retrieve replica.
	r := b.replicas[c.ReplicaID]
	if r == nil {
		return
	}

	// Save current index on topic.
	t := b.mustCreateTopicIfNotExists(c.TopicID)

	// Ensure topic is not already subscribed to.
	if _, ok := r.topics[c.TopicID]; ok {
		b.Logger.Printf("already subscribed to topic: replica=%d, topic=%d", r.id, c.TopicID)
		return
	}

	// Add subscription to replica.
	r.topics[c.TopicID] = struct{}{}
	t.replicas[c.ReplicaID] = r

	// Catch up replica.
	_ = t.writeTo(r)

	b.mustSave()
}

// Unsubscribe removes a subscription for a topic from a replica.
func (b *Broker) Unsubscribe(replicaID, topicID uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Ensure replica & topic exist.
	if b.replicas[replicaID] == nil {
		return ErrReplicaNotFound
	}

	// Issue command to unsubscribe from topic.
	return b.PublishSync(&Message{
		Type: UnsubscribeMessageType,
		Data: mustMarshalJSON(&UnsubscribeCommand{ReplicaID: replicaID, TopicID: topicID}),
	})
}

func (b *Broker) mustApplyUnsubscribe(m *Message) {
	var c UnsubscribeCommand
	mustUnmarshalJSON(m.Data, &c)

	// Remove topic from replica.
	if r := b.replicas[c.ReplicaID]; r != nil {
		delete(r.topics, c.TopicID)
	}

	// Remove replica from topic.
	if t := b.topics[c.TopicID]; t != nil {
		delete(t.replicas, c.ReplicaID)
	}

	b.mustSave()
}

// ReplicaIndex returns the highest received index of a replica.
func (b *Broker) ReplicaIndex(id uint64) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Ensure replica exists.
	r := b.replicas[id]
	if r == nil {
		return 0, ErrReplicaNotFound
	}
	return r.index, nil
}

// Heartbeat records a heartbeat from a replica.
// The heartbeat is transient and only stored on the leader. It is used for
// truncating the broker log segments but truncation can only occur if the
// broker has current heartbeats from all replicas.
func (b *Broker) Heartbeat(id, index uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Ignore if the broker is not the leader.
	if !b.IsLeader() {
		return raft.ErrNotLeader
	}

	// Find replica.
	r := b.replicas[id]
	if r == nil {
		return ErrReplicaNotFound
	}

	// Update its highest index received.
	r.index = index
	return nil
}

// Truncate removes log segments that have been replicated to all subscribed replicas.
func (b *Broker) Truncate() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Loop over every topic.
	for _, t := range b.topics {
		// Determine the highest index replicated to all subscribed replicas.
		minReplicaTopicIndex := b.minReplicaTopicIndex(t.id)

		// Loop over segments and close as needed.
		newSegments := make(segments, 0, len(t.segments))
		for i, s := range t.segments {
			// Find the next segment so we can find the upper index bound.
			var next *segment
			if i < len(t.segments)-1 {
				next = t.segments[i+1]
			}

			// Ignore the last segment or if the next index is less than
			// the highest index replicated across all replicas.
			if next == nil || minReplicaTopicIndex < next.index {
				newSegments = append(newSegments, s)
				continue
			}

			// Remove the segment if the replicated index has moved pasted
			// all the entries inside this segment.
			s.close()
			if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove segment: topic=%d, segment=%d, err=%s", t.id, s.index, err)
			}
		}
	}

	return nil
}

// brokerFSM implements the raft.FSM interface for the broker.
// This is implemented as a separate type because it is not meant to be exported.
type brokerFSM Broker

// MustApply executes a raft log entry against the broker.
// Non-repeatable errors such as system or disk errors must panic.
func (fsm *brokerFSM) MustApply(e *raft.LogEntry) {
	b := (*Broker)(fsm)

	// Create a message with the same index as Raft.
	m := &Message{}

	// Decode commands into messages.
	// Convert internal raft entries to no-ops to move the index forward.
	if e.Type == raft.LogEntryCommand {
		// Decode the message from the raft log.
		err := m.UnmarshalBinary(e.Data)
		assert(err == nil, "message unmarshal: %s", err)

		// Update the broker configuration.
		switch m.Type {
		case CreateReplicaMessageType:
			b.mustApplyCreateReplica(m)
		case DeleteReplicaMessageType:
			b.mustApplyDeleteReplica(m)
		case SubscribeMessageType:
			b.mustApplySubscribe(m)
		case UnsubscribeMessageType:
			b.mustApplyUnsubscribe(m)
		}
	} else {
		// Internal raft commands should be broadcast out as no-ops.
		m.TopicID = BroadcastTopicID
		m.Type = InternalMessageType
	}

	// Set the raft index.
	m.Index = e.Index

	// Write to the topic.
	t := b.mustCreateTopicIfNotExists(m.TopicID)
	if err := t.encode(m); err != nil {
		panic("encode: " + err.Error())
	}

	// Save highest applied index.
	b.index = e.Index
}

// Index returns the highest index that the broker has seen.
func (fsm *brokerFSM) Index() (uint64, error) {
	b := (*Broker)(fsm)
	return b.index, nil
}

// Snapshot streams the current state of the broker and returns the index.
func (fsm *brokerFSM) Snapshot(w io.Writer) (uint64, error) {
	b := (*Broker)(fsm)

	// TODO: Prevent truncation during snapshot.

	// Calculate header under lock.
	b.mu.RLock()
	hdr, err := b.createSnapshotHeader()
	b.mu.RUnlock()
	if err != nil {
		return 0, fmt.Errorf("create snapshot: %s", err)
	}

	// Encode snapshot header.
	buf, err := json.Marshal(&hdr)
	if err != nil {
		return 0, fmt.Errorf("encode snapshot header: %s", err)
	}

	// Write header frame.
	if err := binary.Write(w, binary.BigEndian, uint32(len(buf))); err != nil {
		return 0, fmt.Errorf("write header size: %s", err)
	}
	if _, err := w.Write(buf); err != nil {
		return 0, fmt.Errorf("write header: %s", err)
	}

	// Stream each topic sequentially.
	for _, t := range hdr.Topics {
		for _, s := range t.Segments {
			if _, err := copyFileN(w, s.path, s.Size); err != nil {
				return 0, err
			}
		}
	}

	// Return the snapshot and its last applied index.
	return hdr.Index, nil
}

// Restore reads the broker state.
func (fsm *brokerFSM) Restore(r io.Reader) error {
	b := (*Broker)(fsm)

	b.mu.Lock()
	defer b.mu.Unlock()

	// Read header frame.
	var sz uint32
	if err := binary.Read(r, binary.BigEndian, &sz); err != nil {
		return fmt.Errorf("read header size: %s", err)
	}
	buf := make([]byte, sz)
	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("read header: %s", err)
	}

	// Decode header.
	sh := &snapshotHeader{}
	if err := json.Unmarshal(buf, &sh); err != nil {
		return fmt.Errorf("decode header: %s", err)
	}

	// Close any topics and replicas which might be open and clear them out.
	b.closeTopics()
	b.closeReplicas()

	// Copy topic files from snapshot to local disk.
	for _, st := range sh.Topics {
		t := b.newTopic(st.ID)

		// Remove existing file if it exists.
		if err := os.RemoveAll(t.path); err != nil && !os.IsNotExist(err) {
			return err
		}

		// Copy data from snapshot into segment files.
		// We don't instantiate the segments because that will be done
		// automatically when calling open() on the topic.
		for _, ss := range st.Segments {
			if err := func() error {
				// Create a new file with the starting index.
				f, err := os.Open(t.segmentPath(ss.Index))
				if err != nil {
					return fmt.Errorf("open segment: %s", err)
				}
				defer func() { _ = f.Close() }()

				// Copy from stream into file.
				if _, err := io.CopyN(f, r, ss.Size); err != nil {
					return fmt.Errorf("copy segment: %s", err)
				}

				return nil
			}(); err != nil {
				return err
			}
		}

		// Open new empty topic file.
		if err := t.open(); err != nil {
			return fmt.Errorf("open topic: %s", err)
		}
	}

	// Update the replicas.
	for _, sr := range sh.Replicas {
		// Create replica.
		r := newReplica(b, sr.ID, sr.URL)
		b.replicas[r.id] = r

		// Append replica's topics.
		for _, topicID := range sr.TopicIDs {
			r.topics[topicID] = struct{}{}
		}
	}

	return nil
}

// copyFileN copies n bytes from a path to a writer.
func copyFileN(w io.Writer, path string, n int64) (int64, error) {
	// Open file for reading.
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	// Copy file up to n bytes.
	return io.CopyN(w, f, n)
}

// snapshotHeader represents the header of a snapshot.
type snapshotHeader struct {
	Replicas []*snapshotReplica `json:"replicas"`
	Topics   []*snapshotTopic   `json:"topics"`
	Index    uint64             `json:"index"`
}

type snapshotReplica struct {
	ID       uint64   `json:"id"`
	TopicIDs []uint64 `json:"topicIDs"`
	URL      string   `json:"url"`
}

type snapshotTopic struct {
	ID       uint64                  `json:"id"`
	Segments []*snapshotTopicSegment `json:"segments"`
}

type snapshotTopicSegment struct {
	Index uint64 `json:"index"`
	Size  int64  `json:"size"`

	path string
}

// topic represents a single named queue of messages.
// Each topic is identified by a unique path.
//
// Topics write their entries to segmented log files which contain a
// contiguous range of entries. These segments are periodically dropped
// as data is replicated the replicas and the replicas heartbeat back
// a confirmation of receipt.
type topic struct {
	id       uint64   // unique identifier
	index    uint64   // highest index written
	path     string   // on-disk path
	segments segments // list of available segments

	replicas map[uint64]*Replica // replicas subscribed to topic
}

// segmentPath returns the path to a segment starting with a given log index.
func (t *topic) segmentPath(index uint64) string {
	path := t.path
	if path == "" {
		return ""
	}
	return filepath.Join(path, strconv.FormatUint(index, 10))
}

// open opens a topic for writing.
func (t *topic) open() error {
	assert(len(t.segments) == 0, "topic already open: %d", t.id)

	// Ensure the parent directory exists.
	if err := os.MkdirAll(t.path, 0700); err != nil {
		return err
	}

	// Read available segments.
	if err := t.loadSegments(); err != nil {
		return fmt.Errorf("read segments: %s", err)
	}

	return nil
}

// loadSegments reads all available segments for the topic.
// At least one segment will always exist.
func (t *topic) loadSegments() error {
	// Open handle to directory.
	f, err := os.Open(t.path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Read directory items.
	fis, err := f.Readdir(0)
	if err != nil {
		return err
	}

	// Create a segment for each file with a numeric name.
	var a segments
	for _, fi := range fis {
		index, err := strconv.ParseUint(fi.Name(), 10, 64)
		if err != nil {
			continue
		}
		a = append(a, &segment{
			index: index,
			path:  t.segmentPath(index),
			size:  fi.Size(),
		})
	}
	sort.Sort(a)

	// Create a first segment if one doesn't exist.
	if len(a) == 0 {
		a = segments{&segment{index: 0, path: t.segmentPath(0), size: 0}}
	}

	t.segments = a

	return nil
}

// close closes the underlying file.
func (t *topic) Close() error {
	for _, s := range t.segments {
		_ = s.close()
	}
	return nil
}

// loadIndex reads the highest available index for a topic from disk.
func (t *topic) loadIndex() error {
	// Open topic file for reading.
	f, err := os.Open(t.segments.last().path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Read all messages.
	dec := NewMessageDecoder(bufio.NewReader(f))
	for {
		// Decode message.
		var m Message
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("decode: %s", err)
		}

		// Update the topic's highest index.
		t.index = m.Index
	}
}

// writeTo writes the topic to a replica. Only writes messages after replica index.
// Returns an error if the starting index is unavailable.
func (t *topic) writeTo(r *Replica) error {
	// TODO: If index is too old then return an error.

	// Retrieve the replica's highest received index.
	index := r.index

	// Loop over each segment and write if it contains entries after index.
	segments := t.segments
	for i, s := range segments {
		// Determine the maximum index in the range.
		var next *segment
		if i < len(segments)-1 {
			next = segments[i+1]
		}

		// If the index is after the end of the segment then ignore.
		if next != nil && index >= next.index {
			continue
		}

		// Otherwise write segment to replica.
		if err := t.writeSegmentTo(r, index, s); err != nil {
			return fmt.Errorf("write segment(%d/%d): %s", t.id, s.index, err)
		}
	}

	return nil
}

func (t *topic) writeSegmentTo(r *Replica, index uint64, segment *segment) error {
	// Open segment for reading.
	// If it doesn't exist then just exit immediately.
	f, err := os.Open(segment.path)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Stream out all messages until EOF.
	dec := NewMessageDecoder(bufio.NewReader(f))
	for {
		// Decode message.
		var m Message
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("decode: %s", err)
		}

		// Ignore message if it's on or before high water mark.
		if m.Index <= index {
			continue
		}

		// Write message out to stream.
		_, err := m.WriteTo(r)
		if err != nil {
			return fmt.Errorf("write to: %s", err)
		}
	}

	return nil
}

// encode writes a message to the end of the topic.
func (t *topic) encode(m *Message) error {
	// Ensure message is in-order.
	assert(m.Index > t.index, "topic message out of order: %d -> %d", t.index, m.Index)

	// Retrieve the last segment.
	s := t.segments.last()

	// Close the segment if it's too large.
	if s.size > MaxSegmentSize {
		s.close()
		s = nil
	}

	// Create and append a new segment if we don't have one.
	if s == nil {
		t.segments = append(t.segments, &segment{index: m.Index, path: t.segmentPath(m.Index)})
	}
	if s.file == nil {
		if err := s.open(); err != nil {
			return fmt.Errorf("open segment: %s", err)
		}
	}

	// Encode message.
	b := make([]byte, messageHeaderSize+len(m.Data))
	copy(b, m.marshalHeader())
	copy(b[messageHeaderSize:], m.Data)

	// Write to segment.
	if _, err := s.file.Write(b); err != nil {
		return fmt.Errorf("write segment: %s", err)
	}

	// Move up high water mark on the topic.
	t.index = m.Index

	// Write message out to all replicas.
	for _, r := range t.replicas {
		_, _ = r.Write(b)
	}

	return nil
}

// segment represents a contiguous section of a topic log.
type segment struct {
	index uint64 // starting index of the segment and name
	path  string // path to the segment file.
	size  int64  // total size of the segment file, in bytes.

	file *os.File // handle for writing, only open for last segment
}

// open opens the file handle for append.
func (s *segment) open() error {
	f, err := os.OpenFile(s.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	s.file = f
	return nil
}

// close closes the segment's writing file handle.
func (s *segment) close() error {
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// segments represents a list of segments sorted by index.
type segments []*segment

// last returns the last segment in the slice.
// Returns nil if there are no elements.
func (a segments) last() *segment {
	if len(a) == 0 {
		return nil
	}
	return a[len(a)-1]
}

func (a segments) Len() int           { return len(a) }
func (a segments) Less(i, j int) bool { return a[i].index < a[j].index }
func (a segments) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// replicas represents a sortable list of replicas.
type replicas []*Replica

func (a replicas) Len() int           { return len(a) }
func (a replicas) Less(i, j int) bool { return a[i].id < a[j].id }
func (a replicas) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// Replica represents a collection of subscriptions to topics on the broker.
// The replica maintains the highest index read for each topic so that the
// broker can use this high water mark for trimming the topic logs.
type Replica struct {
	URL *url.URL

	id     uint64
	broker *Broker
	index  uint64 // highest index replicated to the replica.

	writer io.Writer     // currently attached writer
	done   chan struct{} // notify when current writer is removed

	topics map[uint64]struct{} // set of subscribed topics.
}

// newReplica returns a new Replica instance associated with a broker.
func newReplica(b *Broker, id uint64, urlstr string) *Replica {
	// get the url of the replica
	u, err := url.Parse(urlstr)
	if err != nil {
		panic(err.Error())
	}

	return &Replica{
		URL:    u,
		broker: b,
		id:     id,
		topics: make(map[uint64]struct{}),
	}
}

// closeWriter removes the writer on the replica and closes the notify channel.
func (r *Replica) closeWriter() {
	if r.writer != nil {
		r.writer = nil
		close(r.done)
		r.done = nil
	}
}

// Topics returns a list of topic names that the replica is subscribed to.
func (r *Replica) Topics() []uint64 {
	a := make([]uint64, 0, len(r.topics))
	for topicID := range r.topics {
		a = append(a, topicID)
	}
	sort.Sort(uint64Slice(a))
	return a
}

// Write writes a byte slice to the underlying writer.
// If no writer is available then ErrReplicaUnavailable is returned.
func (r *Replica) Write(p []byte) (int, error) {
	// Check if there's a replica available.
	if r.writer == nil {
		return 0, errReplicaUnavailable
	}

	// If an error occurs on the write then remove the writer.
	n, err := r.writer.Write(p)
	if err != nil {
		r.closeWriter()
		return n, errReplicaUnavailable
	}

	// If the writer has a flush method then call it.
	if w, ok := r.writer.(flusher); ok {
		w.Flush()
	}

	return n, nil
}

// WriteTo begins writing messages to a named stream.
// Only one writer is allowed on a stream at a time.
func (r *Replica) WriteTo(w io.Writer) (int64, error) {
	// Close previous writer, if set.
	r.closeWriter()

	// Set a new writer on the replica.
	r.writer = w
	done := make(chan struct{})
	r.done = done

	// Create a topic list with the "config" topic first.
	// Configuration changes need to be propagated to make sure topics exist.
	ids := make([]uint64, 0, len(r.topics))
	for topicID := range r.topics {
		ids = append(ids, topicID)
	}
	sort.Sort(uint64Slice(ids))

	// Catch up and attach replica to all subscribed topics.
	for _, topicID := range ids {
		// Find topic.
		t := r.broker.topics[topicID]
		assert(t != nil, "topic missing: %s", topicID)

		// Write topic messages to replica.
		if err := t.writeTo(r); err != nil {
			r.closeWriter()
			return 0, fmt.Errorf("add stream writer: %s", err)
		}

		// Attach replica to topic to tail new messages.
		t.replicas[r.id] = r
	}

	// Wait for writer to close and then return.
	<-done
	return 0, nil
}

// CreateReplica creates a new replica.
type CreateReplicaCommand struct {
	ID  uint64 `json:"id"`
	URL string `json:"url"`
}

// DeleteReplicaCommand removes a replica.
type DeleteReplicaCommand struct {
	ID uint64 `json:"id"`
}

// SubscribeCommand subscribes a replica to a new topic.
type SubscribeCommand struct {
	ReplicaID uint64 `json:"replicaID"` // replica id
	TopicID   uint64 `json:"topicID"`   // topic id
}

// UnsubscribeCommand removes a subscription for a topic from a replica.
type UnsubscribeCommand struct {
	ReplicaID uint64 `json:"replicaID"` // replica id
	TopicID   uint64 `json:"topicID"`   // topic id
}

// MessageType represents the type of message.
type MessageType uint16

const (
	BrokerMessageType = 0x8000
)

const (
	InternalMessageType = BrokerMessageType | MessageType(0x00)

	CreateReplicaMessageType = BrokerMessageType | MessageType(0x10)
	DeleteReplicaMessageType = BrokerMessageType | MessageType(0x11)

	SubscribeMessageType   = BrokerMessageType | MessageType(0x20)
	UnsubscribeMessageType = BrokerMessageType | MessageType(0x21)
)

// The size of the encoded message header, in bytes.
const messageHeaderSize = 2 + 8 + 8 + 4

// Message represents a single item in a topic.
type Message struct {
	Type    MessageType
	TopicID uint64
	Index   uint64
	Data    []byte
}

// WriteTo encodes and writes the message to a writer. Implements io.WriterTo.
func (m *Message) WriteTo(w io.Writer) (n int64, err error) {
	if n, err := w.Write(m.marshalHeader()); err != nil {
		return int64(n), err
	}
	if n, err := w.Write(m.Data); err != nil {
		return int64(messageHeaderSize + n), err
	}
	return int64(messageHeaderSize + len(m.Data)), nil
}

// MarshalBinary returns a binary representation of the message.
// This implements encoding.BinaryMarshaler. An error cannot be returned.
func (m *Message) MarshalBinary() ([]byte, error) {
	b := make([]byte, messageHeaderSize+len(m.Data))
	copy(b, m.marshalHeader())
	copy(b[messageHeaderSize:], m.Data)
	return b, nil
}

// UnmarshalBinary reads a message from a binary encoded slice.
// This implements encoding.BinaryUnmarshaler.
func (m *Message) UnmarshalBinary(b []byte) error {
	m.unmarshalHeader(b)
	if len(b[messageHeaderSize:]) < len(m.Data) {
		return fmt.Errorf("message data too short: %d < %d", len(b[messageHeaderSize:]), len(m.Data))
	}
	copy(m.Data, b[messageHeaderSize:])
	return nil
}

// marshalHeader returns a byte slice with the message header.
func (m *Message) marshalHeader() []byte {
	b := make([]byte, messageHeaderSize)
	binary.BigEndian.PutUint16(b[0:2], uint16(m.Type))
	binary.BigEndian.PutUint64(b[2:10], m.TopicID)
	binary.BigEndian.PutUint64(b[10:18], m.Index)
	binary.BigEndian.PutUint32(b[18:22], uint32(len(m.Data)))
	return b
}

// unmarshalHeader reads message header data from binary encoded slice.
// The data field is appropriately sized but is not filled.
func (m *Message) unmarshalHeader(b []byte) {
	m.Type = MessageType(binary.BigEndian.Uint16(b[0:2]))
	m.TopicID = binary.BigEndian.Uint64(b[2:10])
	m.Index = binary.BigEndian.Uint64(b[10:18])
	m.Data = make([]byte, binary.BigEndian.Uint32(b[18:22]))
}

// MessageDecoder decodes messages from a reader.
type MessageDecoder struct {
	r io.Reader
}

// NewMessageDecoder returns a new instance of the MessageDecoder.
func NewMessageDecoder(r io.Reader) *MessageDecoder {
	return &MessageDecoder{r: r}
}

// Decode reads a message from the decoder's reader.
func (dec *MessageDecoder) Decode(m *Message) error {
	// Read header bytes.
	var b [messageHeaderSize]byte
	if _, err := io.ReadFull(dec.r, b[:]); err != nil {
		return err
	}
	m.unmarshalHeader(b[:])

	// Read data.
	if _, err := io.ReadFull(dec.r, m.Data); err != nil {
		return err
	}

	return nil
}

type flusher interface {
	Flush()
}

// uint64Slice attaches the methods of Interface to []int, sorting in increasing order.
type uint64Slice []uint64

func (p uint64Slice) Len() int           { return len(p) }
func (p uint64Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p uint64Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// mustMarshalJSON encodes a value to JSON.
// This will panic if an error occurs. This should only be used internally when
// an invalid marshal will cause corruption and a panic is appropriate.
func mustMarshalJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("marshal: " + err.Error())
	}
	return b
}

// mustUnmarshalJSON decodes a value from JSON.
// This will panic if an error occurs. This should only be used internally when
// an invalid unmarshal will cause corruption and a panic is appropriate.
func mustUnmarshalJSON(b []byte, v interface{}) {
	if err := json.Unmarshal(b, v); err != nil {
		panic("unmarshal: " + err.Error())
	}
}

// assert will panic with a given formatted message if the given condition is false.
func assert(condition bool, msg string, v ...interface{}) {
	if !condition {
		panic(fmt.Sprintf("assert failed: "+msg, v...))
	}
}

func warn(v ...interface{})              { fmt.Fprintln(os.Stderr, v...) }
func warnf(msg string, v ...interface{}) { fmt.Fprintf(os.Stderr, msg+"\n", v...) }
