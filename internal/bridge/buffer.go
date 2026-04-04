package bridge

import (
	"sync"
	"sync/atomic"
)

// DefaultBufferCapacity is the maximum number of messages stored in the ring buffer
const DefaultBufferCapacity = 1000

// BufferedMessage represents a message stored in the ring buffer
type BufferedMessage struct {
	ID        int64
	Data      []byte
	Timestamp int64
}

// MessageBuffer is a ring buffer that stores recently sent messages for replay after reconnection.
type MessageBuffer struct {
	messages []BufferedMessage
	cap      int
	head     int   // next write position
	count    int   // current number of messages
	lastAck  int64 // server-acknowledged message ID
	nextID   int64 // next message ID to assign
	mu       sync.Mutex
}

// NewMessageBuffer creates a new ring buffer with the given capacity.
func NewMessageBuffer(capacity int) *MessageBuffer {
	if capacity <= 0 {
		capacity = DefaultBufferCapacity
	}
	return &MessageBuffer{
		messages: make([]BufferedMessage, capacity),
		cap:      capacity,
		nextID:   1,
	}
}

// Push adds a message to the buffer and returns its assigned ID.
// If the buffer is full, the oldest message is overwritten.
func (mb *MessageBuffer) Push(data []byte, timestamp int64) int64 {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	id := atomic.AddInt64(&mb.nextID, 1) - 1

	msg := BufferedMessage{
		ID:        id,
		Data:      make([]byte, len(data)),
		Timestamp: timestamp,
	}
	copy(msg.Data, data)

	mb.messages[mb.head] = msg
	mb.head = (mb.head + 1) % mb.cap
	if mb.count < mb.cap {
		mb.count++
	}

	return id
}

// ReplayAfter returns all messages with ID greater than lastAck, in order.
// Returns an empty slice if no messages need replay.
func (mb *MessageBuffer) ReplayAfter(lastAck int64) []BufferedMessage {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.count == 0 {
		return nil
	}

	// Find the start of valid data in the ring buffer
	start := (mb.head - mb.count + mb.cap) % mb.cap

	var result []BufferedMessage
	for i := 0; i < mb.count; i++ {
		idx := (start + i) % mb.cap
		if mb.messages[idx].ID > lastAck {
			result = append(result, mb.messages[idx])
		}
	}

	return result
}

// SetLastAck updates the server-acknowledged message ID.
func (mb *MessageBuffer) SetLastAck(id int64) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if id > mb.lastAck {
		mb.lastAck = id
	}
}

// LastAck returns the last server-acknowledged message ID.
func (mb *MessageBuffer) LastAck() int64 {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return mb.lastAck
}

// Count returns the current number of messages in the buffer.
func (mb *MessageBuffer) Count() int {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return mb.count
}
