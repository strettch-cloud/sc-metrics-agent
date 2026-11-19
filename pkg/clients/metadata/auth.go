package metadata

import (
	"context"
	"sync"
	"time"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
	"go.uber.org/zap"
)

// refreshRetryInterval is how long the refresh loop waits after a failed
// refresh. A variable only so tests can shorten it before NewAuthManager.
var refreshRetryInterval = 30 * time.Second

// AuthManager keeps a token from the metadata service, replacing it before it
// expires and as soon as the server refuses it.
type AuthManager struct {
	client *Client
	vmID   string
	logger *zap.Logger
	stopCh chan struct{}
	// retryInterval is refreshRetryInterval when the manager was made.
	retryInterval time.Duration

	// fetchMu lets one fetch run at a time, so a token refused by both the
	// ingest and the heartbeat is replaced once.
	fetchMu sync.Mutex

	// mutex guards the two fields below: the refresh loop writes them while
	// the pipeline, the heartbeat and the ingest client read them.
	mutex        sync.RWMutex
	currentToken string
	cloudAPIURL  string
}

// NewAuthManager creates a new auth manager.
func NewAuthManager(cfg *config.Config, logger *zap.Logger) *AuthManager {
	return &AuthManager{
		client: NewClient(cfg.MetadataServiceEndpoint, cfg.HTTPTimeout, logger),
		vmID:   cfg.VMID,
		logger: logger,
		stopCh: make(chan struct{}),

		retryInterval: refreshRetryInterval,
	}
}

// fetchAndStoreToken fetches a token and CloudAPI URL and stores them
// internally. On failure the token and URL the manager had are kept.
func (am *AuthManager) fetchAndStoreToken(ctx context.Context, forceFetch bool) error {
	am.fetchMu.Lock()
	defer am.fetchMu.Unlock()
	return am.fetchAndStoreTokenLocked(ctx, forceFetch)
}

func (am *AuthManager) fetchAndStoreTokenLocked(ctx context.Context, forceFetch bool) error {
	if forceFetch {
		am.logger.Debug("Forcing token refresh")
		am.client.InvalidateCache()
	}

	token, err := am.client.GetAuthToken(ctx, am.vmID)
	if err != nil {
		return err
	}
	am.mutex.Lock()
	am.currentToken = token
	am.mutex.Unlock()

	cloudAPIURL, err := am.client.GetCloudAPIURL(ctx, am.vmID)
	if err != nil {
		return err
	}
	am.mutex.Lock()
	am.cloudAPIURL = cloudAPIURL
	am.mutex.Unlock()

	am.logger.Debug("Token and CloudAPI URL stored successfully")
	return nil
}

// EnsureValidToken fetches a new token if the current one is due, and stores
// it internally. It makes no request while the current token is good.
func (am *AuthManager) EnsureValidToken(ctx context.Context) error {
	return am.fetchAndStoreToken(ctx, false)
}

// RenewRefusedToken fetches a new token at once after the server refused
// refused. It does nothing if that token was already replaced.
func (am *AuthManager) RenewRefusedToken(ctx context.Context, refused string) error {
	am.fetchMu.Lock()
	defer am.fetchMu.Unlock()
	if am.GetCurrentToken() != refused {
		return nil
	}
	am.logger.Warn("Token refused, fetching a new one")
	return am.fetchAndStoreTokenLocked(ctx, true)
}

// StartRefresh replaces the token when it is due, without waiting for a round.
func (am *AuthManager) StartRefresh(ctx context.Context) {
	go func() {
		for {
			wait := time.Until(am.client.RefreshAt())
			if wait <= 0 {
				wait = am.retryInterval
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
				if err := am.EnsureValidToken(ctx); err != nil {
					am.logger.Error("Background token refresh failed", zap.Error(err))
				}
			case <-ctx.Done():
				timer.Stop()
				am.logger.Info("Token refresh stopped due to context cancel")
				return
			case <-am.stopCh:
				timer.Stop()
				am.logger.Info("Token refresh stopped")
				return
			}
		}
	}()
}

// refresh invalidates the cache and fetches a new token.
func (am *AuthManager) refresh(ctx context.Context) error {
	return am.fetchAndStoreToken(ctx, true)
}

// GetCurrentToken returns the current authentication token.
func (am *AuthManager) GetCurrentToken() string {
	am.mutex.RLock()
	defer am.mutex.RUnlock()
	return am.currentToken
}

// GetCloudAPIURL returns the cached CloudAPI URL
func (am *AuthManager) GetCloudAPIURL() string {
	am.mutex.RLock()
	defer am.mutex.RUnlock()
	return am.cloudAPIURL
}

// Close stops the refresh loop.
func (am *AuthManager) Close() {
	close(am.stopCh)
}
