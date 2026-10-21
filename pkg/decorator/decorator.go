package decorator

import (
	"fmt"

	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
)

// MetricDecorator defines the interface for decorating metrics
type MetricDecorator interface {
	Decorate(families []*dto.MetricFamily) ([]*dto.MetricFamily, error)
}

// metricDecorator implements MetricDecorator interface
type metricDecorator struct {
	vmID   string
	labels map[string]string
	logger *zap.Logger
}

// NewMetricDecorator creates a new metric decorator
func NewMetricDecorator(vmID string, labels map[string]string, logger *zap.Logger) MetricDecorator {
	return &metricDecorator{
		vmID:   vmID,
		labels: labels,
		logger: logger,
	}
}

// Decorate adds VM ID and custom labels to all metrics
func (md *metricDecorator) Decorate(families []*dto.MetricFamily) ([]*dto.MetricFamily, error) {
	if len(families) == 0 {
		return families, nil
	}

	md.logger.Debug("Decorating metric families", zap.Int("families", len(families)))

	decoratedFamilies := make([]*dto.MetricFamily, 0, len(families))

	for _, family := range families {
		decoratedFamily, err := md.decorateFamily(family)
		if err != nil {
			md.logger.Error("Failed to decorate metric family", 
				zap.Error(err), 
				zap.String("family", family.GetName()))
			return nil, fmt.Errorf("failed to decorate family %s: %w", family.GetName(), err)
		}
		decoratedFamilies = append(decoratedFamilies, decoratedFamily)
	}

	md.logger.Debug("Successfully decorated all metric families", 
		zap.Int("decorated_families", len(decoratedFamilies)))
	return decoratedFamilies, nil
}

// decorateFamily adds labels to all metrics in a metric family
func (md *metricDecorator) decorateFamily(family *dto.MetricFamily) (*dto.MetricFamily, error) {
	if family == nil {
		return nil, fmt.Errorf("metric family is nil")
	}

	// Create a copy of the family to avoid modifying the original
	decoratedFamily := &dto.MetricFamily{
		Name:   family.Name,
		Help:   family.Help,
		Type:   family.Type,
		Metric: make([]*dto.Metric, 0, len(family.Metric)),
	}

	// Process each metric in the family
	for _, metric := range family.Metric {
		decoratedMetric, err := md.decorateMetric(metric)
		if err != nil {
			return nil, fmt.Errorf("failed to decorate metric: %w", err)
		}
		decoratedFamily.Metric = append(decoratedFamily.Metric, decoratedMetric)
	}

	return decoratedFamily, nil
}

// decorateMetric adds labels to a single metric
func (md *metricDecorator) decorateMetric(metric *dto.Metric) (*dto.Metric, error) {
	if metric == nil {
		return nil, fmt.Errorf("metric is nil")
	}

	// Create a copy of the metric
	decoratedMetric := &dto.Metric{
		Label:       make([]*dto.LabelPair, 0, len(metric.Label)+len(md.labels)+1),
		Gauge:       metric.Gauge,
		Counter:     metric.Counter,
		Summary:     metric.Summary,
		Untyped:     metric.Untyped,
		Histogram:   metric.Histogram,
		TimestampMs: metric.TimestampMs,
	}

	// Copy existing labels
	for _, label := range metric.Label {
		decoratedMetric.Label = append(decoratedMetric.Label, &dto.LabelPair{
			Name:  label.Name,
			Value: label.Value,
		})
	}

	// Add VM ID label
	vmIDLabel := &dto.LabelPair{
		Name:  stringPtr("vm_id"),
		Value: stringPtr(md.vmID),
	}
	decoratedMetric.Label = append(decoratedMetric.Label, vmIDLabel)

	// Add custom labels
	for key, value := range md.labels {
		customLabel := &dto.LabelPair{
			Name:  stringPtr(key),
			Value: stringPtr(value),
		}
		decoratedMetric.Label = append(decoratedMetric.Label, customLabel)
	}

	return decoratedMetric, nil
}

// stringPtr returns a pointer to a string
func stringPtr(s string) *string {
	return &s
}
