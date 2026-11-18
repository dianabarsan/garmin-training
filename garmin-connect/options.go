package garminconnect

import (
	"log/slog"
	"net/http"
	"time"
)

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the underlying HTTP client. The client is copied, and
// the copy's Transport is wrapped with retry handling and the per-attempt
// timeout; set WithRetry(0, ...) to disable retries. A Timeout set on hc still
// applies, to the whole request including retries.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithBaseURL overrides the API base URL, for tests and sandboxes. It must use
// https, except for a loopback host such as a local test server; otherwise
// every request fails with ErrInsecureBaseURL. It does not change where the
// token source exchanges the key: configure that on the token source (for
// auth.TokenSource, with auth.WithBaseURL).
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

// WithRetry configures retry behaviour for transient failures (timeouts,
// dropped connections, 429 and most 5xx statuses). maxRetries == 0 disables
// retries. A Retry-After longer than maxBackoff is not waited out: the 429 is
// returned instead.
func WithRetry(maxRetries int, minBackoff, maxBackoff time.Duration) Option {
	return func(c *Client) {
		c.retries = maxRetries
		c.minBackoff = minBackoff
		c.maxBackoff = maxBackoff
	}
}

// WithAttemptTimeout bounds each attempt of a request: how long to wait for
// the response headers, and then how long reading the body may go without
// receiving any data. A timed-out attempt is retried, up to the retry count
// (see WithRetry), except for DownloadActivity once it has started writing the
// file: a download that stalls part way fails with ErrAttemptTimeout. Zero
// disables it. The default is DefaultAttemptTimeout.
func WithAttemptTimeout(d time.Duration) Option {
	return func(c *Client) { c.attemptTimeout = d }
}

// WithMaxResponseBytes caps the size of a JSON response; a larger one fails
// with ErrResponseTooLarge instead of being read into memory. Zero or a
// negative value removes the cap. The default is DefaultMaxResponseBytes.
// DownloadActivity streams to a writer and is not capped.
func WithMaxResponseBytes(n int64) Option {
	return func(c *Client) { c.maxResponse = n }
}
