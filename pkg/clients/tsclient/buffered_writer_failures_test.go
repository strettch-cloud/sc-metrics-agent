package tsclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/buffer"
)

func TestABatchThatCantBeCompressedIsAnErrorAndQueuesNothing(t *testing.T) {
	sender := &fakeSender{endpointAvailable: true, compressErr: errors.New("json: unsupported value: NaN")}
	w := newTestWriter(t, 1024, sender)

	err := w.WriteMetrics(context.Background(), batch("a"), "token")

	assert.ErrorContains(t, err, "failed to compress metrics")
	assert.Equal(t, 0, w.BufferedBatches())
}

func TestATickWithNothingToSendOrDrainSendsNothing(t *testing.T) {
	sender := &fakeSender{endpointAvailable: true}
	w := newTestWriter(t, 1024, sender)

	require.NoError(t, w.WriteMetrics(context.Background(), nil, "token"))

	assert.Empty(t, sender.envelopes)
}

// Shutting down cancels the context: the batch stays in the buffer for the
// next start instead of being sent half way through a stop.
func TestABatchWrittenAsTheAgentStopsStaysBuffered(t *testing.T) {
	sender := &fakeSender{endpointAvailable: true}
	w := newTestWriter(t, 1024, sender)
	stopping, stop := context.WithCancel(context.Background())
	stop()

	require.NoError(t, w.WriteMetrics(stopping, batch("a"), "token"))

	assert.Equal(t, 1, w.BufferedBatches())
	assert.Empty(t, sender.envelopes)
}

// Buffered batches whose files are gone are dropped, and nothing is sent for
// them.
func TestBufferedBatchesWhoseFilesAreGoneSendNothing(t *testing.T) {
	dir := t.TempDir()
	queue, err := buffer.NewDiskQueue(dir, 1024, zap.NewNop())
	require.NoError(t, err)
	sender := &fakeSender{endpointAvailable: false}
	w := &BufferedMetricWriter{sender: sender, queue: queue, logger: zap.NewNop()}
	require.NoError(t, w.WriteMetrics(context.Background(), batch("a"), "token"))
	batchFiles, err := filepath.Glob(filepath.Join(dir, "*.batch"))
	require.NoError(t, err)
	for _, batchFile := range batchFiles {
		require.NoError(t, os.Remove(batchFile))
	}
	sender.endpointAvailable = true

	require.NoError(t, w.WriteMetrics(context.Background(), nil, "token"))

	assert.Empty(t, sender.envelopes)
	assert.Equal(t, 0, w.BufferedBatches())
}

// While batches go one at a time, a send with no answer or a stopped agent
// ends the round, and the batches not yet accepted stay saved.
func TestSendingBatchesAloneStopsOnAFailedSendOrShutdown(t *testing.T) {
	original := splitSendInterval
	splitSendInterval = time.Millisecond
	t.Cleanup(func() { splitSendInterval = original })

	t.Run("a send that fails", func(t *testing.T) {
		sender := &fakeSender{endpointAvailable: true, results: []sendResult{
			{status: 400}, {status: 202}, {err: errors.New("connection refused")},
		}}
		w := newTestWriter(t, 1024, sender)
		require.NoError(t, w.WriteMetrics(context.Background(), batch("a"), ""))
		require.NoError(t, w.WriteMetrics(context.Background(), batch("b"), ""))

		require.NoError(t, w.WriteMetrics(context.Background(), batch("c"), "token"))

		assert.Len(t, sender.envelopes, 3)
		assert.Equal(t, 2, w.BufferedBatches())
	})

	t.Run("a stopped agent", func(t *testing.T) {
		splitSendInterval = time.Hour
		stopping, stop := context.WithCancel(context.Background())
		sender := &fakeSender{endpointAvailable: true, results: []sendResult{{status: 400}}}
		w := newTestWriter(t, 1024, sender)
		require.NoError(t, w.WriteMetrics(stopping, batch("a"), ""))
		time.AfterFunc(50*time.Millisecond, stop)

		require.NoError(t, w.WriteMetrics(stopping, batch("b"), "token"))

		assert.Len(t, sender.envelopes, 1)
		assert.Equal(t, 2, w.BufferedBatches())
	})
}
