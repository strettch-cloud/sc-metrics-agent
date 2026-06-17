package buffer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newDisk(t *testing.T, maxBytes int) (*DiskQueue, string) {
	t.Helper()
	dir := t.TempDir()
	q, err := NewDiskQueue(dir, maxBytes, zap.NewNop())
	require.NoError(t, err)
	return q, dir
}

func TestDiskQueue_PushPop_FIFO(t *testing.T) {
	q, _ := newDisk(t, 100)

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

func TestDiskQueue_Peek_DoesNotRemove(t *testing.T) {
	q, _ := newDisk(t, 100)
	require.True(t, q.Push([]byte("first")))
	require.True(t, q.Push([]byte("second")))

	blob, ok := q.Peek()
	require.True(t, ok)
	assert.Equal(t, []byte("first"), blob)
	assert.Equal(t, 2, q.Len())

	blob, ok = q.Peek()
	require.True(t, ok)
	assert.Equal(t, []byte("first"), blob)
}

func TestDiskQueue_ReadN(t *testing.T) {
	q, _ := newDisk(t, 100)
	for _, s := range []string{"a", "b", "c", "d"} {
		require.True(t, q.Push([]byte(s)))
	}

	assert.Nil(t, q.ReadN(0))
	assert.Nil(t, q.ReadN(-1))

	got := q.ReadN(2)
	require.Len(t, got, 2)
	assert.Equal(t, []byte("a"), got[0])
	assert.Equal(t, []byte("b"), got[1])
	assert.Equal(t, 4, q.Len())

	got = q.ReadN(10)
	require.Len(t, got, 4)
	assert.Equal(t, []byte("d"), got[3])
}

func TestDiskQueue_ByteCap_DropsNewest(t *testing.T) {
	q, _ := newDisk(t, 10)

	require.True(t, q.Push([]byte("oldest"))) // 6 bytes
	require.True(t, q.Push([]byte("mid")))    // +3 = 9
	assert.False(t, q.Push([]byte("dropped"))) // would be 16 → dropped

	assert.Equal(t, 2, q.Len())
	assert.Equal(t, 9, q.Bytes())

	blob, _ := q.Pop()
	assert.Equal(t, []byte("oldest"), blob)
	blob, _ = q.Pop()
	assert.Equal(t, []byte("mid"), blob)
}

func TestDiskQueue_PopReleasesBytes(t *testing.T) {
	q, _ := newDisk(t, 10)
	require.True(t, q.Push(make([]byte, 8)))
	assert.False(t, q.Push(make([]byte, 4)))

	_, ok := q.Pop()
	require.True(t, ok)
	assert.Equal(t, 0, q.Bytes())

	require.True(t, q.Push(make([]byte, 4)))
}

func TestDiskQueue_RestartRecovery_PreservesFIFOAndBytes(t *testing.T) {
	dir := t.TempDir()

	q1, err := NewDiskQueue(dir, 1024, zap.NewNop())
	require.NoError(t, err)
	for _, s := range []string{"a", "bb", "ccc"} {
		require.True(t, q1.Push([]byte(s)))
	}
	require.Equal(t, 3, q1.Len())
	require.Equal(t, 6, q1.Bytes())

	// Simulate restart: re-open the same directory.
	q2, err := NewDiskQueue(dir, 1024, zap.NewNop())
	require.NoError(t, err)

	assert.Equal(t, 3, q2.Len())
	assert.Equal(t, 6, q2.Bytes())

	blob, ok := q2.Pop()
	require.True(t, ok)
	assert.Equal(t, []byte("a"), blob, "FIFO order must survive restart")
	blob, _ = q2.Pop()
	assert.Equal(t, []byte("bb"), blob)
	blob, _ = q2.Pop()
	assert.Equal(t, []byte("ccc"), blob)
}

func TestDiskQueue_RestartContinuesSequence(t *testing.T) {
	dir := t.TempDir()

	q1, err := NewDiskQueue(dir, 1024, zap.NewNop())
	require.NoError(t, err)
	require.True(t, q1.Push([]byte("one")))
	require.True(t, q1.Push([]byte("two")))

	q2, err := NewDiskQueue(dir, 1024, zap.NewNop())
	require.NoError(t, err)
	require.True(t, q2.Push([]byte("three")))

	// FIFO order from disk + the new push.
	for _, want := range []string{"one", "two", "three"} {
		blob, ok := q2.Pop()
		require.True(t, ok)
		assert.Equal(t, want, string(blob))
	}
}

func TestDiskQueue_OrphanTmpFiles_CleanedOnOpen(t *testing.T) {
	dir := t.TempDir()

	// Simulate a crash mid-write by manually dropping a .tmp file.
	orphan := filepath.Join(dir, "00000000000000000005.tmp")
	require.NoError(t, os.WriteFile(orphan, []byte("partial"), 0o640))

	// A legitimate batch from a previous run.
	valid := filepath.Join(dir, "00000000000000000003.batch")
	require.NoError(t, os.WriteFile(valid, []byte("good"), 0o640))

	q, err := NewDiskQueue(dir, 1024, zap.NewNop())
	require.NoError(t, err)

	assert.Equal(t, 1, q.Len(), "orphan .tmp must be ignored")
	_, statErr := os.Stat(orphan)
	assert.True(t, os.IsNotExist(statErr), "orphan .tmp must be deleted on open")

	blob, ok := q.Pop()
	require.True(t, ok)
	assert.Equal(t, []byte("good"), blob)
}

func TestDiskQueue_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("notes"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "garbage.batch"), []byte("bad"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "00000000000000000007.batch"), []byte("real"), 0o640))

	q, err := NewDiskQueue(dir, 1024, zap.NewNop())
	require.NoError(t, err)

	assert.Equal(t, 1, q.Len())
	blob, _ := q.Pop()
	assert.Equal(t, []byte("real"), blob)
}

func TestDiskQueue_DefaultCap(t *testing.T) {
	dir := t.TempDir()
	q, err := NewDiskQueue(dir, 0, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxBytes, q.maxBytes)
}

func TestDiskQueue_CreatesDirectoryIfMissing(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "a", "b", "c")
	q, err := NewDiskQueue(nested, 1024, zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, q)
	info, err := os.Stat(nested)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// TestDiskQueue_ReadN_SkipsUnreadableMiddleEntry exercises the self-healing
// path: when a file in the middle of the queue becomes unreadable, ReadN
// must drop it inline so the rest of the queue keeps draining.
func TestDiskQueue_ReadN_SkipsUnreadableMiddleEntry(t *testing.T) {
	q, dir := newDisk(t, 1024)

	for _, s := range []string{"a", "b", "c", "d"} {
		require.True(t, q.Push([]byte(s)))
	}
	require.Equal(t, 4, q.Len())

	// Corrupt the second file ("b" at seq 1) by removing it directly.
	require.NoError(t, os.Remove(filepath.Join(dir, "00000000000000000001.batch")))

	got := q.ReadN(10)
	// Expect: "a" succeeds, "b" fails and is dropped, "c" and "d" succeed.
	require.Len(t, got, 3)
	assert.Equal(t, []byte("a"), got[0])
	assert.Equal(t, []byte("c"), got[1])
	assert.Equal(t, []byte("d"), got[2])

	// Bad entry is gone from in-memory state; bytes accounting reflects it.
	assert.Equal(t, 3, q.Len())
	assert.Equal(t, 3, q.Bytes(), "currentBytes must shrink by the dropped entry's bytes")
}
