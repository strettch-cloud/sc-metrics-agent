package buffer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestQueue_PushPop_FIFO(t *testing.T) {
	q := NewQueue(100, zap.NewNop())

	require.True(t, q.Push([]byte("a")))
	require.True(t, q.Push([]byte("bb")))
	require.True(t, q.Push([]byte("ccc")))

	assert.Equal(t, 3, q.Len())
	assert.Equal(t, 6, q.Bytes())

	blob, ok := q.Pop()
	require.True(t, ok)
	assert.Equal(t, []byte("a"), blob)

	blob, ok = q.Pop()
	require.True(t, ok)
	assert.Equal(t, []byte("bb"), blob)

	blob, ok = q.Pop()
	require.True(t, ok)
	assert.Equal(t, []byte("ccc"), blob)

	_, ok = q.Pop()
	assert.False(t, ok)
	assert.Equal(t, 0, q.Len())
	assert.Equal(t, 0, q.Bytes())
}

func TestQueue_Peek_DoesNotRemove(t *testing.T) {
	q := NewQueue(100, zap.NewNop())
	require.True(t, q.Push([]byte("first")))
	require.True(t, q.Push([]byte("second")))

	blob, ok := q.Peek()
	require.True(t, ok)
	assert.Equal(t, []byte("first"), blob)
	assert.Equal(t, 2, q.Len())

	// Peeking again returns the same blob.
	blob, ok = q.Peek()
	require.True(t, ok)
	assert.Equal(t, []byte("first"), blob)
}

func TestQueue_Peek_Empty(t *testing.T) {
	q := NewQueue(100, zap.NewNop())
	_, ok := q.Peek()
	assert.False(t, ok)
}

func TestQueue_ByteCap_Enforced(t *testing.T) {
	q := NewQueue(10, zap.NewNop())

	require.True(t, q.Push(make([]byte, 4)))
	require.True(t, q.Push(make([]byte, 4)))
	assert.Equal(t, 8, q.Bytes())

	// A 3-byte push would put us at 11 > 10 — rejected.
	assert.False(t, q.Push(make([]byte, 3)))
	assert.Equal(t, 8, q.Bytes())
	assert.Equal(t, 2, q.Len())

	// A 2-byte push fits exactly at the cap.
	require.True(t, q.Push(make([]byte, 2)))
	assert.Equal(t, 10, q.Bytes())
	assert.Equal(t, 3, q.Len())
}

func TestQueue_Overflow_DropsNewest_KeepsOldest(t *testing.T) {
	q := NewQueue(10, zap.NewNop())

	require.True(t, q.Push([]byte("oldest")))    // 6 bytes
	require.True(t, q.Push([]byte("mid")))       // +3 = 9 bytes
	assert.False(t, q.Push([]byte("dropped")))   // would be 16 — dropped

	// The oldest blob is still there in FIFO order.
	blob, ok := q.Pop()
	require.True(t, ok)
	assert.Equal(t, []byte("oldest"), blob)

	blob, ok = q.Pop()
	require.True(t, ok)
	assert.Equal(t, []byte("mid"), blob)

	_, ok = q.Pop()
	assert.False(t, ok)
}

func TestQueue_PopReleasesBytes(t *testing.T) {
	q := NewQueue(10, zap.NewNop())
	require.True(t, q.Push(make([]byte, 8)))
	assert.False(t, q.Push(make([]byte, 4))) // would overflow

	// After popping, capacity frees up and the next push fits.
	_, ok := q.Pop()
	require.True(t, ok)
	assert.Equal(t, 0, q.Bytes())

	require.True(t, q.Push(make([]byte, 4)))
}

func TestQueue_DefaultCap(t *testing.T) {
	q := NewQueue(0, zap.NewNop())
	assert.Equal(t, DefaultMaxBytes, q.maxBytes)
}

func TestQueue_PeekN(t *testing.T) {
	q := NewQueue(100, zap.NewNop())
	for _, name := range []string{"a", "b", "c", "d"} {
		require.True(t, q.Push([]byte(name)))
	}

	// PeekN(0) / negative returns nil; queue untouched.
	assert.Nil(t, q.PeekN(0))
	assert.Nil(t, q.PeekN(-1))
	assert.Equal(t, 4, q.Len())

	// PeekN(2) returns the two oldest in FIFO order without removing.
	got := q.PeekN(2)
	require.Len(t, got, 2)
	assert.Equal(t, []byte("a"), got[0])
	assert.Equal(t, []byte("b"), got[1])
	assert.Equal(t, 4, q.Len())

	// PeekN(n) where n > Len returns everything available.
	got = q.PeekN(10)
	require.Len(t, got, 4)
	assert.Equal(t, []byte("a"), got[0])
	assert.Equal(t, []byte("d"), got[3])

	// Empty queue returns nil.
	emptyQ := NewQueue(100, zap.NewNop())
	assert.Nil(t, emptyQ.PeekN(5))
}
