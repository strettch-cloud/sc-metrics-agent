package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// Headers
	HeaderAccept       = "Accept"
	HeaderXResourceID  = "X-Resource-ID"
	
	// Values
	AcceptJSON = "application/json"
	
	// A token is replaced this long before its expiresAt.
	TokenRefreshMargin = time.Minute

	// How long a token without a usable expiresAt is kept, unless the server
	// refuses it first.
	TokenCacheLifetime = 30 * time.Minute

	// The metadata service at 169.254.169.254 refuses a request without this header.
	HeaderMetadataFlavor = "Metadata-Flavor"
	MetadataFlavor       = "StrettchCloud"
)

// TokenResponse represents the response from the metadata service
type TokenResponse struct {
	Token       string `json:"token"`
	CloudAPIUrl string `json:"cloudAPIUrl"`
	ExpiresAt   string `json:"expiresAt"`
}

// Client handles communication with the metadata service with token caching
type Client struct {
	endpoint      string
	httpClient    *http.Client
	logger        *zap.Logger
	
	// Token and CloudAPI URL caching
	tokenMu       sync.RWMutex
	cachedToken   string
	cachedAPIURL  string
	refreshAt     time.Time
	tokenLifetime time.Duration
}

// NewClient creates a new metadata service client with token caching
func NewClient(endpoint string, timeout time.Duration, logger *zap.Logger) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger:        logger,
		tokenLifetime: TokenCacheLifetime,
	}
}

// GetAuthToken returns a cached token or fetches a new one if expired
func (c *Client) GetAuthToken(ctx context.Context, vmID string) (string, error) {
	// Check if cached token is still valid
	c.tokenMu.RLock()
	if c.cachedToken != "" && time.Now().Before(c.refreshAt) {
		token := c.cachedToken
		c.tokenMu.RUnlock()
		c.logger.Debug("Using cached auth token",
			zap.Duration("refresh_in", time.Until(c.refreshAt)))
		return token, nil
	}
	c.tokenMu.RUnlock()

	// Need to fetch a new token
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Double-check after acquiring write lock
	if c.cachedToken != "" && time.Now().Before(c.refreshAt) {
		return c.cachedToken, nil
	}

	// Fetch new token and metadata
	tokenResp, err := c.fetchAuthToken(ctx, vmID)
	if err != nil {
		return "", err
	}

	c.cachedToken = tokenResp.Token
	// An answer without the address keeps the one the agent had.
	if tokenResp.CloudAPIUrl != "" {
		c.cachedAPIURL = tokenResp.CloudAPIUrl
	} else {
		c.logger.Warn("CloudAPI URL not available in metadata response")
	}
	c.refreshAt = c.refreshTime(tokenResp.ExpiresAt)

	c.logger.Info("Successfully fetched and cached new auth token",
		zap.Time("refresh_at", c.refreshAt))

	return tokenResp.Token, nil
}

// refreshTime is TokenRefreshMargin before expiresAt, or tokenLifetime from
// now when expiresAt is missing or malformed. A token with less than the
// margin left is kept for half the time it has, so it isn't fetched again on
// every call.
func (c *Client) refreshTime(expiresAt string) time.Time {
	now := time.Now()
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		c.logger.Debug("Token has no usable expiresAt, keeping it for the default lifetime",
			zap.String("expires_at", expiresAt), zap.Duration("lifetime", c.tokenLifetime))
		return now.Add(c.tokenLifetime)
	}
	if refresh := expiry.Add(-TokenRefreshMargin); refresh.After(now) {
		return refresh
	}
	return now.Add(expiry.Sub(now) / 2)
}

// RefreshAt is when the cached token is due to be replaced.
func (c *Client) RefreshAt() time.Time {
	c.tokenMu.RLock()
	defer c.tokenMu.RUnlock()
	return c.refreshAt
}

// fetchAuthToken fetches a new authentication token and metadata
func (c *Client) fetchAuthToken(ctx context.Context, vmID string) (*TokenResponse, error) {
	c.logger.Debug("Fetching new auth token from metadata service", zap.String("endpoint", c.endpoint))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set(HeaderAccept, AcceptJSON)
	req.Header.Set(HeaderXResourceID, vmID)
	req.Header.Set(HeaderMetadataFlavor, MetadataFlavor)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("Failed to fetch auth token", zap.Error(err))
		return nil, fmt.Errorf("failed to fetch auth token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		c.logger.Error("Metadata service returned error",
			zap.Int("status_code", resp.StatusCode),
			zap.String("response", string(body)))
		return nil, fmt.Errorf("metadata service returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal token response: %w", err)
	}

	if tokenResp.Token == "" {
		return nil, fmt.Errorf("received empty token from metadata service")
	}

	c.logger.Debug("Successfully fetched auth token and metadata")
	return &tokenResp, nil
}

// InvalidateCache forces the next GetAuthToken call to fetch a new token.
// The cached token and URL are kept in case that fetch fails.
func (c *Client) InvalidateCache() {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	c.refreshAt = time.Time{}
	c.logger.Debug("Token cache invalidated")
}

// GetCloudAPIURL returns the cached CloudAPI URL, fetching a token first if
// the cached one is due.
func (c *Client) GetCloudAPIURL(ctx context.Context, vmID string) (string, error) {
	_, err := c.GetAuthToken(ctx, vmID)
	if err != nil {
		return "", fmt.Errorf("failed to get CloudAPI URL: %w", err)
	}
	return c.cachedCloudAPIURL()
}

// cachedCloudAPIURL returns the cached CloudAPI URL without fetching a token.
func (c *Client) cachedCloudAPIURL() (string, error) {
	c.tokenMu.RLock()
	url := c.cachedAPIURL
	c.tokenMu.RUnlock()

	if url == "" {
		return "", fmt.Errorf("CloudAPI URL not available in metadata response")
	}

	return url, nil
}

