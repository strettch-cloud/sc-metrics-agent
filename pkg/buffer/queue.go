// Package buffer provides a bounded in-memory FIFO queue for compressed metric
// batches.
package buffer

import (
	"sync"

	"go.uber.org/zap"
)

// ~10 MB buffer capacity (~16–40 hours of retention).
const DefaultMaxBytes = 10 * 1024 * 1024

// A thread-safe FIFO queue with a byte size limit. When full, new data is dropped to preserve older entries.
type Queue struct {
	mu           sync.Mutex
	items        [][]byte
	currentBytes int
	maxBytes     int
	logger       *zap.Logger
}

// NewQueue creates a queue with the given byte cap. A non-positive cap falls
// back to DefaultMaxBytes.
func NewQueue(maxBytes int, logger *zap.Logger) *Queue {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Queue{
		maxBytes: maxBytes,
		logger:   logger,
	}
}

// Push appends blob to the back of the queue. If accepting it would exceed the
// byte cap the blob is dropped and Push returns false; the queue is unchanged.
func (q *Queue) Push(blob []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.currentBytes+len(blob) > q.maxBytes {
		q.logger.Warn("buffer overflow: dropping newest batch",
			zap.Int("incoming_bytes", len(blob)),
			zap.Int("current_bytes", q.currentBytes),
			zap.Int("max_bytes", q.maxBytes),
		)
		return false
	}

	q.items = append(q.items, blob)
	q.currentBytes += len(blob)
	return true
}

// Peek returns the oldest item without removing it.
// The returned slice is read-only.
func (q *Queue) Peek() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return nil, false
	}
	return q.items[0], true
}

// PeekN returns up to n oldest FIFO items without removing them. The result
// shares underlying storage with the queue; callers must not mutate it.
func (q *Queue) PeekN(n int) [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()

	if n <= 0 || len(q.items) == 0 {
		return nil
	}
	if n > len(q.items) {
		n = len(q.items)
	}
	out := make([][]byte, n)
	copy(out, q.items[:n])
	return out
}

// Pop removes and returns the oldest blob.
func (q *Queue) Pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.items) == 0 {
		return nil, false
	}

	blob := q.items[0]
	// Release the underlying array slot so the GC can reclaim the blob.
	q.items[0] = nil
	q.items = q.items[1:]
	q.currentBytes -= len(blob)
	return blob, true
}

// Len returns the number of buffered blobs.
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Bytes returns the total bytes currently buffered.
func (q *Queue) Bytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.currentBytes
}
