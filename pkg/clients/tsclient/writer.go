package tsclient

import (
	"context"
	"fmt"

	"go.uber.org/zap"
)

// AuxiliaryWriter handles the non-buffered signals: heartbeats and close.
type AuxiliaryWriter interface {
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
