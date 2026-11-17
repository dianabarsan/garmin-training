package garminconnect

import (
	"log/slog"
	"net/http"
	"time"
)

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the underlying HTTP client. Its Transport is wrapped
// with retry handling; set WithRetry(0, ...) to disable that.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithBaseURL overrides the API base URL (useful for tests and sandboxes).
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = u }
}

// WithTokenSource sets where bearer tokens come from.
func WithTokenSource(ts TokenSource) Option {
	return func(c *Client) { c.tokens = ts }
}

// WithLogger sets the logger used for debug output. Defaults to a discard logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithRetry configures retry behaviour for transient failures (network errors,
// 429 and 5xx). maxRetries == 0 disables retries.
func WithRetry(maxRetries int, minBackoff, maxBackoff time.Duration) Option {
	return func(c *Client) {
		c.retries = maxRetries
		c.minBackoff = minBackoff
		c.maxBackoff = maxBackoff
	}
}
