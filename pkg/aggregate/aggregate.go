package aggregate

import (
	"fmt"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

// MetricWithValue represents an aggregated metric in internal format
type MetricWithValue struct {
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	Value     float64           `json:"value"`
	Timestamp int64             `json:"timestamp"`
	Type      string            `json:"type"`
}

// Aggregator defines the interface for metric aggregation
type Aggregator interface {
	Aggregate(families []*dto.MetricFamily) ([]MetricWithValue, error)
}

// aggregator implements the Aggregator interface
type aggregator struct {
	logger *zap.Logger
}

// NewAggregator creates a new metric aggregator
func NewAggregator(logger *zap.Logger) Aggregator {
	return &aggregator{
		logger: logger,
	}
}

// Aggregate converts Prometheus metric families to internal format
func (a *aggregator) Aggregate(families []*dto.MetricFamily) ([]MetricWithValue, error) {
	if len(families) == 0 {
		return nil, nil
	}

	a.logger.Debug("Starting metric aggregation", zap.Int("families", len(families)))

	var metrics []MetricWithValue
	timestamp := time.Now().UnixMilli()

	for _, family := range families {
		familyMetrics, err := a.processFamily(family, timestamp)
		if err != nil {
			a.logger.Error("Failed to process metric family", zap.Error(err), zap.String("family", family.GetName()))
			return nil, fmt.Errorf("failed to process family %s: %w", family.GetName(), err)
		}
		metrics = append(metrics, familyMetrics...)
	}

	a.logger.Debug("Aggregation completed", zap.Int("aggregated_metrics", len(metrics)))
	return metrics, nil
}

// processFamily converts a single metric family to internal format
func (a *aggregator) processFamily(family *dto.MetricFamily, timestamp int64) ([]MetricWithValue, error) {
	if family == nil {
		return nil, fmt.Errorf("metric family is nil")
	}

	familyName := family.GetName()
	familyType := family.GetType().String()
	
	var metrics []MetricWithValue

	for _, metric := range family.Metric {
		metricValues, err := a.processMetric(familyName, familyType, metric, timestamp)
		if err != nil {
			return nil, fmt.Errorf("failed to process metric in family %s: %w", familyName, err)
		}
		metrics = append(metrics, metricValues...)
	}

	return metrics, nil
}

// processMetric converts a single Prometheus metric to internal format
func (a *aggregator) processMetric(familyName, familyType string, metric *dto.Metric, timestamp int64) ([]MetricWithValue, error) {
	// Extract labels
	labels := make(map[string]string)
	for _, labelPair := range metric.Label {
		labels[labelPair.GetName()] = labelPair.GetValue()
	}

	// Use metric timestamp if available, otherwise use provided timestamp
	metricTimestamp := timestamp
	if metric.TimestampMs != nil {
		metricTimestamp = metric.GetTimestampMs()
	}

	var metrics []MetricWithValue

	// Process based on metric type
	switch familyType {
	case "COUNTER":
		if metric.Counter != nil {
			metrics = append(metrics, MetricWithValue{
				Name:      familyName,
				Labels:    labels,
				Value:     metric.Counter.GetValue(),
				Timestamp: metricTimestamp,
				Type:      "counter",
			})
		}

	case "GAUGE":
		if metric.Gauge != nil {
			metrics = append(metrics, MetricWithValue{
				Name:      familyName,
				Labels:    labels,
				Value:     metric.Gauge.GetValue(),
				Timestamp: metricTimestamp,
				Type:      "gauge",
			})
		}

	case "HISTOGRAM":
		if metric.Histogram != nil {
			// Process histogram buckets
			for _, bucket := range metric.Histogram.Bucket {
				bucketLabels := copyLabels(labels)
				bucketLabels["le"] = fmt.Sprintf("%g", bucket.GetUpperBound())
				
				metrics = append(metrics, MetricWithValue{
					Name:      familyName + "_bucket",
					Labels:    bucketLabels,
					Value:     float64(bucket.GetCumulativeCount()),
					Timestamp: metricTimestamp,
					Type:      "counter",
				})
			}

			// Add count and sum
			metrics = append(metrics, MetricWithValue{
				Name:      familyName + "_count",
				Labels:    labels,
				Value:     float64(metric.Histogram.GetSampleCount()),
				Timestamp: metricTimestamp,
				Type:      "counter",
			})

			metrics = append(metrics, MetricWithValue{
				Name:      familyName + "_sum",
				Labels:    labels,
				Value:     metric.Histogram.GetSampleSum(),
				Timestamp: metricTimestamp,
				Type:      "counter",
			})
		}

	case "SUMMARY":
		if metric.Summary != nil {
			// Process quantiles
			for _, quantile := range metric.Summary.Quantile {
				quantileLabels := copyLabels(labels)
				quantileLabels["quantile"] = fmt.Sprintf("%g", quantile.GetQuantile())
				
				metrics = append(metrics, MetricWithValue{
					Name:      familyName,
					Labels:    quantileLabels,
					Value:     quantile.GetValue(),
					Timestamp: metricTimestamp,
					Type:      "gauge",
				})
			}

			// Add count and sum
			metrics = append(metrics, MetricWithValue{
				Name:      familyName + "_count",
				Labels:    labels,
				Value:     float64(metric.Summary.GetSampleCount()),
				Timestamp: metricTimestamp,
				Type:      "counter",
			})

			metrics = append(metrics, MetricWithValue{
				Name:      familyName + "_sum",
				Labels:    labels,
				Value:     metric.Summary.GetSampleSum(),
				Timestamp: metricTimestamp,
				Type:      "counter",
			})
		}

	case "UNTYPED":
		if metric.Untyped != nil {
			metrics = append(metrics, MetricWithValue{
				Name:      familyName,
				Labels:    labels,
				Value:     metric.Untyped.GetValue(),
				Timestamp: metricTimestamp,
				Type:      "untyped",
			})
		}

	default:
		return nil, fmt.Errorf("unsupported metric type: %s", familyType)
	}

	return metrics, nil
}

// copyLabels creates a deep copy of labels map
func copyLabels(labels map[string]string) map[string]string {
	copy := make(map[string]string, len(labels))
	for k, v := range labels {
		copy[k] = v
	}
	return copy
}

// SortMetrics sorts metrics by name and then by label fingerprint for consistent ordering
func SortMetrics(metrics []MetricWithValue) {
	sort.Slice(metrics, func(i, j int) bool {
		if metrics[i].Name != metrics[j].Name {
			return metrics[i].Name < metrics[j].Name
		}
		return labelFingerprint(metrics[i].Labels) < labelFingerprint(metrics[j].Labels)
	})
}

// labelFingerprint creates a deterministic string representation of labels
func labelFingerprint(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}

	// Sort keys for deterministic output
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}

	return strings.Join(parts, ",")
}

type TimeRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

