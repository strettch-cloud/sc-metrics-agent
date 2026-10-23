package aggregate

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAggregator(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	assert.NotNil(t, aggregator)
	assert.Implements(t, (*Aggregator)(nil), aggregator)
}

func TestAggregator_Aggregate_EmptyInput(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	result, err := aggregator.Aggregate(nil)
	assert.NoError(t, err)
	assert.Nil(t, result)
	
	result, err = aggregator.Aggregate([]*dto.MetricFamily{})
	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestAggregator_Aggregate_CounterMetric(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	// Create a counter metric family
	counterValue := 42.5
	family := &dto.MetricFamily{
		Name: stringPtr("test_counter"),
		Type: metricTypePtr(dto.MetricType_COUNTER),
		Metric: []*dto.Metric{
			{
				Label: []*dto.LabelPair{
					{Name: stringPtr("job"), Value: stringPtr("test")},
					{Name: stringPtr("instance"), Value: stringPtr("localhost")},
				},
				Counter: &dto.Counter{Value: &counterValue},
			},
		},
	}
	
	result, err := aggregator.Aggregate([]*dto.MetricFamily{family})
	require.NoError(t, err)
	require.Len(t, result, 1)
	
	metric := result[0]
	assert.Equal(t, "test_counter", metric.Name)
	assert.Equal(t, "counter", metric.Type)
	assert.Equal(t, 42.5, metric.Value)
	assert.Equal(t, "test", metric.Labels["job"])
	assert.Equal(t, "localhost", metric.Labels["instance"])
	assert.Greater(t, metric.Timestamp, int64(0))
}

func TestAggregator_Aggregate_GaugeMetric(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	gaugeValue := 123.45
	family := &dto.MetricFamily{
		Name: stringPtr("test_gauge"),
		Type: metricTypePtr(dto.MetricType_GAUGE),
		Metric: []*dto.Metric{
			{
				Label: []*dto.LabelPair{
					{Name: stringPtr("status"), Value: stringPtr("active")},
				},
				Gauge: &dto.Gauge{Value: &gaugeValue},
			},
		},
	}
	
	result, err := aggregator.Aggregate([]*dto.MetricFamily{family})
	require.NoError(t, err)
	require.Len(t, result, 1)
	
	metric := result[0]
	assert.Equal(t, "test_gauge", metric.Name)
	assert.Equal(t, "gauge", metric.Type)
	assert.Equal(t, 123.45, metric.Value)
	assert.Equal(t, "active", metric.Labels["status"])
}

func TestAggregator_Aggregate_HistogramMetric(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	sampleCount := uint64(100)
	sampleSum := 250.5
	bucket1Count := uint64(10)
	bucket2Count := uint64(50)
	bucket3Count := uint64(100)
	bucket1Bound := 0.1
	bucket2Bound := 0.5
	bucket3Bound := 1.0
	
	family := &dto.MetricFamily{
		Name: stringPtr("test_histogram"),
		Type: metricTypePtr(dto.MetricType_HISTOGRAM),
		Metric: []*dto.Metric{
			{
				Label: []*dto.LabelPair{
					{Name: stringPtr("method"), Value: stringPtr("GET")},
				},
				Histogram: &dto.Histogram{
					SampleCount: &sampleCount,
					SampleSum:   &sampleSum,
					Bucket: []*dto.Bucket{
						{CumulativeCount: &bucket1Count, UpperBound: &bucket1Bound},
						{CumulativeCount: &bucket2Count, UpperBound: &bucket2Bound},
						{CumulativeCount: &bucket3Count, UpperBound: &bucket3Bound},
					},
				},
			},
		},
	}
	
	result, err := aggregator.Aggregate([]*dto.MetricFamily{family})
	require.NoError(t, err)
	require.Len(t, result, 5) // 3 buckets + count + sum
	
	// Check buckets
	bucketMetrics := make([]MetricWithValue, 0)
	var countMetric, sumMetric *MetricWithValue
	
	for i := range result {
		switch result[i].Name {
		case "test_histogram_bucket":
			bucketMetrics = append(bucketMetrics, result[i])
		case "test_histogram_count":
			countMetric = &result[i]
		case "test_histogram_sum":
			sumMetric = &result[i]
		}
	}
	
	assert.Len(t, bucketMetrics, 3)
	require.NotNil(t, countMetric)
	require.NotNil(t, sumMetric)
	
	// Verify count and sum
	assert.Equal(t, float64(100), countMetric.Value)
	assert.Equal(t, "counter", countMetric.Type)
	assert.Equal(t, 250.5, sumMetric.Value)
	assert.Equal(t, "counter", sumMetric.Type)
	
	// Verify buckets have le labels
	for _, bucket := range bucketMetrics {
		assert.Contains(t, bucket.Labels, "le")
		assert.Equal(t, "GET", bucket.Labels["method"])
		assert.Equal(t, "counter", bucket.Type)
	}
}

func TestAggregator_Aggregate_SummaryMetric(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	sampleCount := uint64(50)
	sampleSum := 125.25
	quantile50 := 0.5
	quantile95 := 0.95
	quantile99 := 0.99
	value50 := 0.1
	value95 := 0.8
	value99 := 1.2
	
	family := &dto.MetricFamily{
		Name: stringPtr("test_summary"),
		Type: metricTypePtr(dto.MetricType_SUMMARY),
		Metric: []*dto.Metric{
			{
				Label: []*dto.LabelPair{
					{Name: stringPtr("handler"), Value: stringPtr("api")},
				},
				Summary: &dto.Summary{
					SampleCount: &sampleCount,
					SampleSum:   &sampleSum,
					Quantile: []*dto.Quantile{
						{Quantile: &quantile50, Value: &value50},
						{Quantile: &quantile95, Value: &value95},
						{Quantile: &quantile99, Value: &value99},
					},
				},
			},
		},
	}
	
	result, err := aggregator.Aggregate([]*dto.MetricFamily{family})
	require.NoError(t, err)
	require.Len(t, result, 5) // 3 quantiles + count + sum
	
	// Check quantiles
	quantileMetrics := make([]MetricWithValue, 0)
	var countMetric, sumMetric *MetricWithValue
	
	for i := range result {
		switch result[i].Name {
		case "test_summary":
			quantileMetrics = append(quantileMetrics, result[i])
		case "test_summary_count":
			countMetric = &result[i]
		case "test_summary_sum":
			sumMetric = &result[i]
		}
	}
	
	assert.Len(t, quantileMetrics, 3)
	require.NotNil(t, countMetric)
	require.NotNil(t, sumMetric)
	
	// Verify count and sum
	assert.Equal(t, float64(50), countMetric.Value)
	assert.Equal(t, 125.25, sumMetric.Value)
	
	// Verify quantiles have quantile labels
	for _, quantile := range quantileMetrics {
		assert.Contains(t, quantile.Labels, "quantile")
		assert.Equal(t, "api", quantile.Labels["handler"])
		assert.Equal(t, "gauge", quantile.Type)
	}
}

func TestAggregator_Aggregate_UntypedMetric(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	untypedValue := 78.9
	family := &dto.MetricFamily{
		Name: stringPtr("test_untyped"),
		Type: metricTypePtr(dto.MetricType_UNTYPED),
		Metric: []*dto.Metric{
			{
				Untyped: &dto.Untyped{Value: &untypedValue},
			},
		},
	}
	
	result, err := aggregator.Aggregate([]*dto.MetricFamily{family})
	require.NoError(t, err)
	require.Len(t, result, 1)
	
	metric := result[0]
	assert.Equal(t, "test_untyped", metric.Name)
	assert.Equal(t, "untyped", metric.Type)
	assert.Equal(t, 78.9, metric.Value)
}

func TestAggregator_Aggregate_WithTimestamp(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	customTimestamp := int64(1677123456789)
	gaugeValue := 42.0
	family := &dto.MetricFamily{
		Name: stringPtr("test_gauge_with_timestamp"),
		Type: metricTypePtr(dto.MetricType_GAUGE),
		Metric: []*dto.Metric{
			{
				Gauge:       &dto.Gauge{Value: &gaugeValue},
				TimestampMs: &customTimestamp,
			},
		},
	}
	
	result, err := aggregator.Aggregate([]*dto.MetricFamily{family})
	require.NoError(t, err)
	require.Len(t, result, 1)
	
	metric := result[0]
	assert.Equal(t, customTimestamp, metric.Timestamp)
}

func TestAggregator_Aggregate_MultipleMetrics(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	counterValue1 := 10.0
	counterValue2 := 20.0
	gaugeValue := 5.0
	
	families := []*dto.MetricFamily{
		{
			Name: stringPtr("test_counter"),
			Type: metricTypePtr(dto.MetricType_COUNTER),
			Metric: []*dto.Metric{
				{
					Label:   []*dto.LabelPair{{Name: stringPtr("instance"), Value: stringPtr("1")}},
					Counter: &dto.Counter{Value: &counterValue1},
				},
				{
					Label:   []*dto.LabelPair{{Name: stringPtr("instance"), Value: stringPtr("2")}},
					Counter: &dto.Counter{Value: &counterValue2},
				},
			},
		},
		{
			Name: stringPtr("test_gauge"),
			Type: metricTypePtr(dto.MetricType_GAUGE),
			Metric: []*dto.Metric{
				{
					Gauge: &dto.Gauge{Value: &gaugeValue},
				},
			},
		},
	}
	
	result, err := aggregator.Aggregate(families)
	require.NoError(t, err)
	require.Len(t, result, 3)
	
	// Count metrics by name
	nameCount := make(map[string]int)
	for _, metric := range result {
		nameCount[metric.Name]++
	}
	
	assert.Equal(t, 2, nameCount["test_counter"])
	assert.Equal(t, 1, nameCount["test_gauge"])
}

func TestAggregator_Aggregate_NilFamily(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	families := []*dto.MetricFamily{nil}
	_, err := aggregator.Aggregate(families)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "metric family is nil")
}

func TestAggregator_Aggregate_UnsupportedType(t *testing.T) {
	logger := zap.NewNop()
	aggregator := NewAggregator(logger)
	
	// Create an invalid metric type
	family := &dto.MetricFamily{
		Name: stringPtr("test_invalid"),
		Type: metricTypePtr(dto.MetricType(999)), // Invalid type
		Metric: []*dto.Metric{
			{
				Gauge: &dto.Gauge{Value: floatPtr(1.0)},
			},
		},
	}
	
	_, err := aggregator.Aggregate([]*dto.MetricFamily{family})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported metric type")
}

func TestSortMetrics(t *testing.T) {
	metrics := []MetricWithValue{
		{Name: "metric_z", Labels: map[string]string{"instance": "2"}},
		{Name: "metric_a", Labels: map[string]string{"instance": "1"}},
		{Name: "metric_z", Labels: map[string]string{"instance": "1"}},
		{Name: "metric_a", Labels: map[string]string{"instance": "2"}},
	}
	
	SortMetrics(metrics)
	
	// Should be sorted by name first, then by label fingerprint
	assert.Equal(t, "metric_a", metrics[0].Name)
	assert.Equal(t, "metric_a", metrics[1].Name)
	assert.Equal(t, "metric_z", metrics[2].Name)
	assert.Equal(t, "metric_z", metrics[3].Name)
	
	// Within same name, should be sorted by labels
	assert.Equal(t, "1", metrics[0].Labels["instance"])
	assert.Equal(t, "2", metrics[1].Labels["instance"])
	assert.Equal(t, "1", metrics[2].Labels["instance"])
	assert.Equal(t, "2", metrics[3].Labels["instance"])
}

func TestLabelFingerprint(t *testing.T) {
	labels1 := map[string]string{"a": "1", "b": "2"}
	labels2 := map[string]string{"b": "2", "a": "1"} // Same labels, different order
	labels3 := map[string]string{"a": "1", "c": "3"}
	
	fp1 := labelFingerprint(labels1)
	fp2 := labelFingerprint(labels2)
	fp3 := labelFingerprint(labels3)
	
	// Same labels should produce same fingerprint regardless of order
	assert.Equal(t, fp1, fp2)
	assert.NotEqual(t, fp1, fp3)
	
	// Empty labels
	emptyFp := labelFingerprint(map[string]string{})
	assert.Equal(t, "", emptyFp)
}

func TestCopyLabels(t *testing.T) {
	original := map[string]string{
		"key1": "value1",
		"key2": "value2",
	}
	
	copied := copyLabels(original)
	
	// Should be equal but different instances
	assert.Equal(t, original, copied)
	assert.NotSame(t, &original, &copied)
	
	// Modifying copy shouldn't affect original
	copied["key3"] = "value3"
	assert.NotContains(t, original, "key3")
	assert.Contains(t, copied, "key3")
}

// Helper functions for tests
func stringPtr(s string) *string {
	return &s
}

func floatPtr(f float64) *float64 {
	return &f
}

func metricTypePtr(t dto.MetricType) *dto.MetricType {
	return &t
}