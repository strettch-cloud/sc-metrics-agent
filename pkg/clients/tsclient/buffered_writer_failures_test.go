package tsclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
