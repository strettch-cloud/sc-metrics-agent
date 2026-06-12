package tsclient

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"go.uber.org/zap"
)

// AuxiliaryWriter handles the non-buffered signals: diagnostics, heartbeats, close.
type AuxiliaryWriter interface {
	WriteDiagnostics(ctx context.Context, agentID string, status string, lastError string, collectorStatus map[string]bool, authToken string) error
	SendHeartbeat(ctx context.Context, authToken string, version string) error
	Close() error
}

type auxWriter struct {
	client *Client
	logger *zap.Logger
}

// NewAuxiliaryWriter is used by NewBufferedMetricWriter for the aux channel.
func NewAuxiliaryWriter(client *Client, logger *zap.Logger) AuxiliaryWriter {
	return &auxWriter{
		client: client,
		logger: logger,
	}
}

func (a *auxWriter) WriteDiagnostics(ctx context.Context, agentID string, status string, lastError string, collectorStatus map[string]bool, authToken string) error {
	a.logger.Debug("Writing diagnostics to ingestor", zap.String("agent_id", agentID))

	diagnostics := DiagnosticPayload{
		AgentID:         agentID,
		Timestamp:       time.Now().UnixMilli(),
		Status:          status,
		LastError:       lastError,
		CollectorStatus: collectorStatus,
		Metadata: map[string]interface{}{
			"version":    "1.0",
			"go_version": runtime.Version(),
		},
	}

	response, err := a.client.SendDiagnostics(ctx, diagnostics, authToken)
	if err != nil {
		a.logger.Error("Failed to send diagnostics", zap.Error(err))
		return fmt.Errorf("failed to send diagnostics: %w", err)
	}

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		a.logger.Info("Successfully sent diagnostics",
			zap.Int("status_code", response.StatusCode),
			zap.String("agent_id", agentID))
		return nil
	}

	errorMsg := fmt.Sprintf("ingestor returned status %d", response.StatusCode)
	if len(response.Body) > 0 {
		errorMsg += fmt.Sprintf(": %s", string(response.Body))
	}
	a.logger.Warn("Ingestor returned error status for diagnostics",
		zap.Int("status_code", response.StatusCode),
		zap.String("response_body", string(response.Body)))
	return fmt.Errorf("failed to write diagnostics: %s", errorMsg)
}

func (a *auxWriter) SendHeartbeat(ctx context.Context, authToken string, version string) error {
	a.logger.Debug("Sending heartbeat")

	response, err := a.client.SendHeartbeat(ctx, authToken, version)
	if err != nil {
		a.logger.Error("Failed to send heartbeat", zap.Error(err))
		return fmt.Errorf("failed to send heartbeat: %w", err)
	}

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		a.logger.Info("Successfully sent heartbeat", zap.Int("status_code", response.StatusCode))
		return nil
	}

	errorMsg := fmt.Sprintf("heartbeat returned status %d", response.StatusCode)
	if len(response.Body) > 0 {
		errorMsg += fmt.Sprintf(": %s", string(response.Body))
	}
	a.logger.Error("Heartbeat failed", zap.String("error", errorMsg))
	return fmt.Errorf("%s", errorMsg)
}

func (a *auxWriter) Close() error {
	a.logger.Debug("Closing auxiliary writer")
	return a.client.Close()
}
