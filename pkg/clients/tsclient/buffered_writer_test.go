package tsclient

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/strettch/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch/sc-metrics-agent/pkg/buffer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeSender records every envelope and replays a programmed sequence of results.
type fakeSender struct {
	endpointAvailable bool
	results           []sendResult
	envelopes         [][][]byte
}

type sendResult struct {
	status int
	err    error
}

func (f *fakeSender) CompressMetrics(metrics []aggregate.MetricWithValue) ([]byte, error) {
	// Encode the first metric's name so tests can assert on FIFO order
	// without depending on snappy/JSON output.
	if len(metrics) == 0 {
		return nil, errors.New("no metrics")
	}
	return []byte(metrics[0].Name), nil
}

func (f *fakeSender) SendCompressedBatches(_ context.Context, batches [][]byte, _ string) (*Response, error) {
	// Snapshot the envelope so later queue mutations don't change what the
	// test sees.
	snapshot := make([][]byte, len(batches))
	for i, b := range batches {
		snapshot[i] = append([]byte(nil), b...)
	}
	f.envelopes = append(f.envelopes, snapshot)

	if len(f.results) == 0 {
		return &Response{StatusCode: 202}, nil
	}
	r := f.results[0]
	f.results = f.results[1:]
	if r.err != nil {
		return nil, r.err
	}
	return &Response{StatusCode: r.status}, nil
}

func (f *fakeSender) IngestEndpointAvailable() bool { return f.endpointAvailable }

// lastEnvelope returns the most recently sent envelope as a slice of strings
// for easy comparison with expected payload names.
func (f *fakeSender) lastEnvelope() []string {
	if len(f.envelopes) == 0 {
		return nil
	}
	env := f.envelopes[len(f.envelopes)-1]
	out := make([]string, len(env))
	for i, b := range env {
		out[i] = string(b)
	}
	return out
}

func newTestWriter(t *testing.T, queueBytes int, sender *fakeSender) *BufferedMetricWriter {
	t.Helper()
	q, err := buffer.NewDiskQueue(t.TempDir(), queueBytes, zap.NewNop())
	require.NoError(t, err)
	return &BufferedMetricWriter{
		sender: sender,
		queue:  q,
		aux:    nil, // unused in these tests
		logger: zap.NewNop(),
	}
}

func batch(name string) []aggregate.MetricWithValue {
	return []aggregate.MetricWithValue{{Name: name}}
}

func TestBufferedWriter_HealthyPath_SendsImmediately(t *testing.T) {
	sender := &fakeSender{endpointAvailable: true}
	w := newTestWriter(t, 1024, sender)

	require.NoError(t, w.WriteMetrics(context.Background(), batch("a"), "token"))

	assert.Equal(t, 0, w.BufferedBatches())
	require.Len(t, sender.envelopes, 1)
	assert.Equal(t, []string{"a"}, sender.lastEnvelope())
}

func TestBufferedWriter_NoAuth_BuffersAndSkipsDrain(t *testing.T) {
	sender := &fakeSender{endpointAvailable: true}
	w := newTestWriter(t, 1024, sender)

	require.NoError(t, w.WriteMetrics(context.Background(), batch("a"), ""))

	assert.Equal(t, 1, w.BufferedBatches())
	assert.Empty(t, sender.envelopes, "drain must be skipped when auth is missing")
}

func TestBufferedWriter_NoEndpoint_BuffersAndSkipsDrain(t *testing.T) {
	sender := &fakeSender{endpointAvailable: false}
	w := newTestWriter(t, 1024, sender)

	require.NoError(t, w.WriteMetrics(context.Background(), batch("a"), "token"))

	assert.Equal(t, 1, w.BufferedBatches())
	assert.Empty(t, sender.envelopes, "drain must be skipped when endpoint is unresolved")
}

func TestBufferedWriter_DrainsUpToN_AsOneEnvelopeOldestFirst(t *testing.T) {
	sender := &fakeSender{endpointAvailable: true}
	w := newTestWriter(t, 4096, sender)

	// Build a backlog of 55 batches while unhealthy.
	for i := 0; i < 55; i++ {
		require.NoError(t, w.WriteMetrics(context.Background(), batch(string(rune('A'+i%26))+strconv.Itoa(i)), ""))
	}
	require.Equal(t, 55, w.BufferedBatches())

	// One healthy tick pushes one more (queue = 56) and drains exactly 50
	// in a single envelope, oldest first.
	require.NoError(t, w.WriteMetrics(context.Background(), batch("Z999"), "token"))

	assert.Equal(t, 6, w.BufferedBatches(), "should leave 56 - 50 = 6 buffered")
	require.Len(t, sender.envelopes, 1, "drain must produce exactly one envelope")
	require.Len(t, sender.envelopes[0], 50)
	// First entry of the envelope must be the oldest blob ("A0").
	assert.Equal(t, "A0", string(sender.envelopes[0][0]))
}

func TestBufferedWriter_EnvelopeRejected_NoBatchesPopped(t *testing.T) {
	sender := &fakeSender{
		endpointAvailable: true,
		results:           []sendResult{{err: errors.New("connection refused")}},
	}
	w := newTestWriter(t, 1024, sender)

	for _, name := range []string{"a", "b", "c", "d"} {
		require.NoError(t, w.WriteMetrics(context.Background(), batch(name), ""))
	}

	// Healthy tick: push "e", drain attempts envelope [a..e], send fails →
	// nothing is popped.
	require.NoError(t, w.WriteMetrics(context.Background(), batch("e"), "token"))

	assert.Equal(t, 5, w.BufferedBatches(), "failed envelope must leave queue intact")
	require.Len(t, sender.envelopes, 1)
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, sender.lastEnvelope())

	// Next tick succeeds → all 5 popped.
	sender.results = []sendResult{{status: 202}}
	require.NoError(t, w.WriteMetrics(context.Background(), nil, "token"))

	assert.Equal(t, 0, w.BufferedBatches())
	require.Len(t, sender.envelopes, 2)
	assert.Equal(t, []string{"a", "b", "c", "d", "e"}, sender.lastEnvelope())
}

func TestBufferedWriter_EnvelopeRejected_5xx_NoBatchesPopped(t *testing.T) {
	sender := &fakeSender{
		endpointAvailable: true,
		results:           []sendResult{{status: 503}},
	}
	w := newTestWriter(t, 1024, sender)

	for _, name := range []string{"a", "b", "c"} {
		require.NoError(t, w.WriteMetrics(context.Background(), batch(name), ""))
	}
	require.NoError(t, w.WriteMetrics(context.Background(), batch("d"), "token"))

	assert.Equal(t, 4, w.BufferedBatches(), "503 must leave queue intact")
	require.Len(t, sender.envelopes, 1)
	assert.Equal(t, []string{"a", "b", "c", "d"}, sender.lastEnvelope())
}

func TestBufferedWriter_Overflow_DropsNewest(t *testing.T) {
	sender := &fakeSender{endpointAvailable: false}
	// Cap of 2 bytes holds at most two single-char batches.
	w := newTestWriter(t, 2, sender)

	require.NoError(t, w.WriteMetrics(context.Background(), batch("a"), ""))
	require.NoError(t, w.WriteMetrics(context.Background(), batch("b"), ""))
	require.NoError(t, w.WriteMetrics(context.Background(), batch("c"), ""))

	// c was dropped; a,b remain in FIFO order.
	assert.Equal(t, 2, w.BufferedBatches())

	sender.endpointAvailable = true
	require.NoError(t, w.WriteMetrics(context.Background(), nil, "token"))

	require.Len(t, sender.envelopes, 1)
	assert.Equal(t, []string{"a", "b"}, sender.lastEnvelope())
}

func TestBufferedWriter_EmptyMetrics_StillDrainsBacklog(t *testing.T) {
	sender := &fakeSender{endpointAvailable: true}
	w := newTestWriter(t, 1024, sender)

	require.NoError(t, w.WriteMetrics(context.Background(), batch("a"), ""))
	require.NoError(t, w.WriteMetrics(context.Background(), batch("b"), ""))

	// "No metrics this tick" should still drain the existing backlog.
	require.NoError(t, w.WriteMetrics(context.Background(), nil, "token"))

	assert.Equal(t, 0, w.BufferedBatches())
	require.Len(t, sender.envelopes, 1)
	assert.Equal(t, []string{"a", "b"}, sender.lastEnvelope())
}

