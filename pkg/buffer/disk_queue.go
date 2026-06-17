// Package buffer provides a bounded on-disk FIFO queue for compressed metric
// batches. Each batch is stored as a single atomic file (write tmp → fsync →
// rename → fsync dir), ensuring safe recovery via directory scan.
package buffer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"
)

const (
	// DefaultMaxBytes caps the buffer at ~10 MB (~16–40 hours at the agent's default cadence).
	DefaultMaxBytes = 10 * 1024 * 1024

	batchExt = ".batch"
	tmpExt   = ".tmp"
	seqWidth = 20 // uint64 max is 20 digits; zero-padding keeps filenames lexicographically ordered
)

// DiskQueue is a thread-safe FIFO of byte blobs persisted as one file per blob.
type DiskQueue struct {
	mu           sync.Mutex
	dir          string
	maxBytes     int
	currentBytes int
	entries      []entry
	nextSeq      uint64
	logger       *zap.Logger
}

// entry tracks a queued blob by sequence number and on-disk size.
// The file path is always derived from seq via pathFor — never stored.
type entry struct {
	seq   uint64
	bytes int
}

// NewDiskQueue opens (or initializes) a disk-backed queue rooted at dir.
// Orphan .tmp files from a crashed write are removed on open.
func NewDiskQueue(dir string, maxBytes int, logger *zap.Logger) (*DiskQueue, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create buffer dir: %w", err)
	}
	q := &DiskQueue{dir: dir, maxBytes: maxBytes, logger: logger}
	return q, q.load()
}

func (q *DiskQueue) load() error {
	files, err := os.ReadDir(q.dir)
	if err != nil {
		return fmt.Errorf("read buffer dir: %w", err)
	}

	var entries []entry
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if strings.HasSuffix(name, tmpExt) {
			// Orphan from a crash before rename — safe to delete.
			_ = os.Remove(filepath.Join(q.dir, name))
			continue
		}
		if !strings.HasSuffix(name, batchExt) {
			continue
		}
		seq, err := strconv.ParseUint(strings.TrimSuffix(name, batchExt), 10, 64)
		if err != nil {
			q.logger.Warn("Skipping malformed buffer file", zap.String("name", name))
			continue
		}
		info, err := f.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", name, err)
		}
		entries = append(entries, entry{seq: seq, bytes: int(info.Size())})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })

	for _, e := range entries {
		q.currentBytes += e.bytes
	}
	q.entries = entries
	if len(entries) > 0 {
		// After sorting, the last entry holds the highest sequence number.
		q.nextSeq = entries[len(entries)-1].seq + 1
	}

	q.logger.Info("Loaded disk buffer",
		zap.String("dir", q.dir),
		zap.Int("batches", len(entries)),
		zap.Int("bytes", q.currentBytes),
	)
	return nil
}

// Push writes blob to disk durably. Returns false if accepting it would exceed
// the byte cap; the blob is dropped wholesale to preserve the older queue.
func (q *DiskQueue) Push(blob []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.currentBytes+len(blob) > q.maxBytes {
		q.logger.Warn("Disk buffer overflow: dropping newest batch",
			zap.Int("incoming_bytes", len(blob)),
			zap.Int("current_bytes", q.currentBytes),
			zap.Int("max_bytes", q.maxBytes),
		)
		return false
	}

	seq := q.nextSeq
	if err := writeAtomic(q.tmpPathFor(seq), q.pathFor(seq), blob); err != nil {
		q.logger.Error("Failed to persist batch; dropping",
			zap.Uint64("seq", seq), zap.Error(err))
		return false
	}

	q.entries = append(q.entries, entry{seq: seq, bytes: len(blob)})
	q.currentBytes += len(blob)
	q.nextSeq++
	return true
}

// Peek returns the oldest blob without removing it.
func (q *DiskQueue) Peek() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.entries) == 0 {
		return nil, false
	}
	data, err := q.readAt(0)
	if err != nil {
		q.logger.Error("Failed to read head batch", zap.Error(err))
		return nil, false
	}
	return data, true
}

// ReadN returns up to n oldest blobs. Successful reads stay in the queue;
// unreadable entries are dropped inline so one bad file can't block the rest.
func (q *DiskQueue) ReadN(n int) [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n <= 0 || len(q.entries) == 0 {
		return nil
	}
	out := make([][]byte, 0, n)
	for i := 0; i < len(q.entries) && len(out) < n; {
		data, err := q.readAt(i)
		if err != nil {
			bad := q.entries[i]
			path := q.pathFor(bad.seq)
			q.logger.Error("Dropping unreadable batch from queue",
				zap.String("path", path), zap.Error(err))
			_ = os.Remove(path)
			q.entries = append(q.entries[:i], q.entries[i+1:]...)
			q.currentBytes -= bad.bytes
			continue // i is unchanged; entries shifted left
		}
		out = append(out, data)
		i++
	}
	return out
}

// Pop removes and returns the oldest blob. ok is false if the queue is empty
// or the read failed; the entry is consumed either way (data lost on read failure).
func (q *DiskQueue) Pop() ([]byte, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.entries) == 0 {
		return nil, false
	}
	head := q.entries[0]
	path := q.pathFor(head.seq)

	data, readErr := q.readAt(0)
	if readErr != nil {
		q.logger.Error("Failed to read popped batch",
			zap.String("path", path), zap.Error(readErr))
	}
	if err := os.Remove(path); err != nil {
		q.logger.Error("Failed to remove popped batch (orphan left on disk)",
			zap.String("path", path), zap.Error(err))
	}

	q.entries = q.entries[1:]
	q.currentBytes -= head.bytes
	return data, readErr == nil
}

// writeAtomic writes data to tmpPath, fsyncs it, renames to finalPath, and
// fsyncs the directory so the rename itself survives a power loss.
func writeAtomic(tmpPath, finalPath string, data []byte) (retErr error) {
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	// On any failure below, remove the partial tmp file. If Rename already
	// moved it to finalPath, Remove returns ENOENT which we ignore.
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, finalPath); err != nil {
		return err
	}
	return syncDir(filepath.Dir(finalPath))
}

// syncDir fsyncs the directory so a rename or unlink is durable across reboot.
// Close errors on a read-only dir handle are uninteresting and ignored.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func (q *DiskQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

func (q *DiskQueue) Bytes() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.currentBytes
}

// basePath is the dir + zero-padded sequence stem for a given seq, no extension.
// pathFor and tmpPathFor both build from this so the format string lives once.
func (q *DiskQueue) basePath(seq uint64) string {
	return filepath.Join(q.dir, fmt.Sprintf("%0*d", seqWidth, seq))
}

func (q *DiskQueue) pathFor(seq uint64) string    { return q.basePath(seq) + batchExt }
func (q *DiskQueue) tmpPathFor(seq uint64) string { return q.basePath(seq) + tmpExt }

// readAt reads the blob at index i in the in-memory entry list.
// Callers must hold mu.
func (q *DiskQueue) readAt(i int) ([]byte, error) {
	return os.ReadFile(q.pathFor(q.entries[i].seq))
}
