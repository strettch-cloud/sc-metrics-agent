package decorator

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func pointerTo[T any](value T) *T {
	return &value
}

func gaugeMetric(value float64, labels ...*dto.LabelPair) *dto.Metric {
	return &dto.Metric{
		Label:       labels,
		Gauge:       &dto.Gauge{Value: pointerTo(value)},
		TimestampMs: pointerTo(int64(1700000000000)),
	}
}

func labelPair(name, value string) *dto.LabelPair {
	return &dto.LabelPair{Name: pointerTo(name), Value: pointerTo(value)}
}

// labelStrings lists a metric's labels as name=value. A list rather than a
// map, so a label that appears twice shows up twice.
func labelStrings(metric *dto.Metric) []string {
	var labels []string
	for _, label := range metric.Label {
		labels = append(labels, label.GetName()+"="+label.GetValue())
	}
	return labels
}

func TestDecorateAddsTheVMIDAndConfiguredLabelsToEveryMetric(t *testing.T) {
	families := []*dto.MetricFamily{
		{
			Name: pointerTo("node_cpu_usage"),
			Help: pointerTo("Share of time the CPU was busy"),
			Type: pointerTo(dto.MetricType_GAUGE),
			Metric: []*dto.Metric{
				gaugeMetric(0.25, labelPair("cpu", "0")),
				gaugeMetric(0.75, labelPair("cpu", "1")),
			},
		},
		{
			Name:   pointerTo("node_load1"),
			Type:   pointerTo(dto.MetricType_GAUGE),
			Metric: []*dto.Metric{gaugeMetric(1.5)},
		},
	}
	metricDecorator := NewMetricDecorator("vm-1", map[string]string{"environment": "test", "team": "storage"}, zap.NewNop())

	decorated, err := metricDecorator.Decorate(families)

	require.NoError(t, err)
	require.Len(t, decorated, 2)
	assert.ElementsMatch(t, []string{"cpu=0", "vm_id=vm-1", "environment=test", "team=storage"}, labelStrings(decorated[0].Metric[0]))
	assert.ElementsMatch(t, []string{"cpu=1", "vm_id=vm-1", "environment=test", "team=storage"}, labelStrings(decorated[0].Metric[1]))
	assert.ElementsMatch(t, []string{"vm_id=vm-1", "environment=test", "team=storage"}, labelStrings(decorated[1].Metric[0]))

	assert.Equal(t, "node_cpu_usage", decorated[0].GetName())
	assert.Equal(t, "Share of time the CPU was busy", decorated[0].GetHelp())
	assert.Equal(t, dto.MetricType_GAUGE, decorated[0].GetType())
	assert.Equal(t, 0.75, decorated[0].Metric[1].GetGauge().GetValue())
	assert.Equal(t, int64(1700000000000), decorated[0].Metric[1].GetTimestampMs())

	assert.Equal(t, []string{"cpu=0"}, labelStrings(families[0].Metric[0]), "the families passed in must not change")
}

func TestDecorateReturnsNothingWhenGivenNothing(t *testing.T) {
	decorated, err := NewMetricDecorator("vm-1", nil, zap.NewNop()).Decorate(nil)

	require.NoError(t, err)
	assert.Empty(t, decorated)
}

func TestDecorateRejectsAMissingFamilyOrMetric(t *testing.T) {
	tests := map[string][]*dto.MetricFamily{
		"missing family": {nil},
		"missing metric": {{Name: pointerTo("node_load1"), Metric: []*dto.Metric{nil}}},
	}
	for name, families := range tests {
		t.Run(name, func(t *testing.T) {
			decorated, err := NewMetricDecorator("vm-1", nil, zap.NewNop()).Decorate(families)

			assert.Error(t, err)
			assert.Nil(t, decorated)
		})
	}
}
