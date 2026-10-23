package buffer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestABufferDirThatCantBeCreatedIsAnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	_, err := NewDiskQueue(filepath.Join(file, "buffer"), 100, zap.NewNop())

	assert.ErrorContains(t, err, "create buffer dir")
}

func TestADirectoryInTheBufferIsIgnored(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "1.batch"), 0o700))

	q, err := NewDiskQueue(dir, 100, zap.NewNop())

	require.NoError(t, err)
	assert.Equal(t, 0, q.Len())
}

// A batch that can't be written is dropped, and the queue stays as it was.
// /dev/full fails every write with "no space left on device", as a full disk
// would.
func TestABatchTheDiskCantTakeIsDropped(t *testing.T) {
	for name, breakIt := range map[string]func(q *DiskQueue){
		"the disk is full": func(q *DiskQueue) {
			require.NoError(t, os.Symlink("/dev/full", q.tmpPathFor(q.nextSeq)))
		},
		"the temporary file can't be created": func(q *DiskQueue) {
			require.NoError(t, os.Mkdir(q.tmpPathFor(q.nextSeq), 0o700))
		},
		"the batch can't be renamed into place": func(q *DiskQueue) {
			require.NoError(t, os.MkdirAll(filepath.Join(q.pathFor(q.nextSeq), "inside"), 0o700))
		},
	} {
		t.Run(name, func(t *testing.T) {
			q, _ := newDisk(t, 100)
			require.True(t, q.Push([]byte("kept")))
			breakIt(q)

			assert.False(t, q.Push([]byte("dropped")))

			assert.Equal(t, 1, q.Len())
			assert.Equal(t, len("kept"), q.Bytes())
		})
	}
}

func TestAFullDiskLeavesNoTemporaryFileBehind(t *testing.T) {
	q, _ := newDisk(t, 100)
	temporaryFile := q.tmpPathFor(q.nextSeq)
	require.NoError(t, os.Symlink("/dev/full", temporaryFile))

	q.Push([]byte("dropped"))

	_, err := os.Lstat(temporaryFile)
	assert.True(t, os.IsNotExist(err), "the temporary file is still there: %v", err)
}

// A batch file someone removed can't be sent; popping it reports that and
// moves on to the next batch.
func TestPoppingABatchWhoseFileIsGoneMovesOn(t *testing.T) {
	q, _ := newDisk(t, 100)
	require.True(t, q.Push([]byte("gone")))
	require.True(t, q.Push([]byte("next")))
	require.NoError(t, os.Remove(q.pathFor(q.entries[0].seq)))

	_, ok := q.Pop()
	assert.False(t, ok)

	blob, ok := q.Pop()
	assert.True(t, ok)
	assert.Equal(t, []byte("next"), blob)
}
