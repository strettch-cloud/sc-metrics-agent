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
	
	// Token cache lifetime
	TokenCacheLifetime = 30 * time.Minute
)

// TokenResponse represents the response from the metadata service
type TokenResponse struct {
	Token       string `json:"token"`
	CloudAPIUrl string `json:"cloudAPIUrl"`
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
	tokenExpiry   time.Time
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
	if c.cachedToken != "" && time.Now().Before(c.tokenExpiry) {
		token := c.cachedToken
		c.tokenMu.RUnlock()
		c.logger.Debug("Using cached auth token", 
			zap.Duration("remaining_lifetime", time.Until(c.tokenExpiry)))
		return token, nil
	}
	c.tokenMu.RUnlock()

	// Need to fetch a new token
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Double-check after acquiring write lock
	if c.cachedToken != "" && time.Now().Before(c.tokenExpiry) {
		return c.cachedToken, nil
	}

	// Fetch new token and metadata
	tokenResp, err := c.fetchAuthToken(ctx, vmID)
	if err != nil {
		return "", err
	}

	// Cache the token and CloudAPI URL
	c.cachedToken = tokenResp.Token
	c.cachedAPIURL = tokenResp.CloudAPIUrl
	c.tokenExpiry = time.Now().Add(c.tokenLifetime)
	
	c.logger.Info("Successfully fetched and cached new auth token",
		zap.Time("expires_at", c.tokenExpiry),
		zap.Duration("lifetime", c.tokenLifetime))

	return tokenResp.Token, nil
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

// InvalidateCache forces the next GetAuthToken call to fetch a new token
func (c *Client) InvalidateCache() {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	
	c.cachedToken = ""
	c.cachedAPIURL = ""
	c.tokenExpiry = time.Time{}
	c.logger.Debug("Token cache invalidated")
}

// GetCloudAPIURL returns the cached CloudAPI URL
func (c *Client) GetCloudAPIURL(ctx context.Context, vmID string) (string, error) {
	c.tokenMu.RLock()
	if c.cachedAPIURL != "" && time.Now().Before(c.tokenExpiry) {
		url := c.cachedAPIURL
		c.tokenMu.RUnlock()
		c.logger.Debug("Using cached CloudAPI URL", zap.String("url", url))
		return url, nil
	}
	c.tokenMu.RUnlock()

	// Need to fetch new metadata to get the URL
	_, err := c.GetAuthToken(ctx, vmID)
	if err != nil {
		return "", fmt.Errorf("failed to get CloudAPI URL: %w", err)
	}

	c.tokenMu.RLock()
	url := c.cachedAPIURL
	c.tokenMu.RUnlock()

	if url == "" {
		return "", fmt.Errorf("CloudAPI URL not available in metadata response")
	}

	return url, nil
}

