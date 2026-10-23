package collector

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// duplicateSeriesCollector sends the same series twice, which Gather rejects.
type duplicateSeriesCollector struct {
	desc *prometheus.Desc
}

func (c duplicateSeriesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c duplicateSeriesCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1)
}

func TestCollectKeepsTheOtherMetricsWhenOneCollectorFails(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(duplicateSeriesCollector{
		desc: prometheus.NewDesc("node_filesystem_size_bytes", "Filesystem size in bytes.", nil, nil),
	})
	load := prometheus.NewGauge(prometheus.GaugeOpts{Name: "node_load1", Help: "1m load average."})
	load.Set(0.5)
	registry.MustRegister(load)
	systemCollector := &SystemCollector{registry: registry, logger: zaptest.NewLogger(t)}

	families, err := systemCollector.Collect(t.Context())

	require.NoError(t, err)
	var names []string
	for _, family := range families {
		names = append(names, family.GetName())
	}
	assert.Contains(t, names, "node_load1")
}

// invalidMetricCollector stands in for a collector whose every series fails.
type invalidMetricCollector struct {
	desc *prometheus.Desc
}

func (c invalidMetricCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.desc
}

func (c invalidMetricCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(c.desc, errors.New("cannot read /proc"))
}

func TestCollectFailsWhenNothingCouldBeGathered(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(invalidMetricCollector{
		desc: prometheus.NewDesc("node_load1", "1m load average.", nil, nil),
	})
	systemCollector := &SystemCollector{registry: registry, logger: zaptest.NewLogger(t)}

	families, err := systemCollector.Collect(t.Context())

	assert.Error(t, err)
	assert.Empty(t, families)
}
