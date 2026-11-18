package garminconnect

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dianabarsan/garmin-training/garmin-connect/internal/transport"
)

// Sentinel errors for common failure classes. Use errors.Is to test for them;
// an *APIError wraps the matching sentinel when one applies.
var (
	// ErrUnauthorized is returned for HTTP 401 responses.
	ErrUnauthorized = errors.New("garmin: unauthorized")
	// ErrForbidden is returned for HTTP 403 responses, e.g. a missing user permission.
	ErrForbidden = errors.New("garmin: forbidden")
	// ErrNotFound is returned for HTTP 404 responses.
	ErrNotFound = errors.New("garmin: not found")
	// ErrBadRequest is returned for HTTP 400 responses, typically a parameter
	// Garmin does not accept (an unknown sport, a range it will not serve).
	ErrBadRequest = errors.New("garmin: bad request")
	// ErrRateLimited is returned for HTTP 429 responses after retries are
	// exhausted, or at once when Garmin's Retry-After asks for a longer wait
	// than the maximum backoff (see WithRetry).
	ErrRateLimited = errors.New("garmin: rate limited")
	// ErrServer is returned for HTTP 5xx responses after retries are exhausted.
	ErrServer = errors.New("garmin: server error")
	// ErrNoTokenSource is returned when a request needs credentials but the
	// client was built without WithTokenSource.
	ErrNoTokenSource = errors.New("garmin: no token source configured")
	// ErrResponseTooLarge is returned when a response exceeds the limit set
	// with WithMaxResponseBytes.
	ErrResponseTooLarge = errors.New("garmin: response too large")
	// ErrEmptyResponse is returned when Garmin answers with no body where one
	// is required, such as a download that has no content.
	ErrEmptyResponse = errors.New("garmin: empty response")
	// ErrInsecureBaseURL is returned (wrapped) by every request when
	// WithBaseURL names a plain-http host other than localhost.
	ErrInsecureBaseURL = transport.ErrInsecureURL
	// ErrAttemptTimeout is returned (wrapped) when Garmin did not answer within
	// the attempt timeout on the last attempt (see WithAttemptTimeout).
	ErrAttemptTimeout = transport.ErrAttemptTimeout
)

// maxErrorBody bounds how much of an error response is kept in APIError.Body.
const maxErrorBody = 4 << 10

// APIError describes a non-2xx response from the Garmin API.
type APIError struct {
	StatusCode int
	Method     string
	URL        string
	// Body is the (truncated) response body, useful for diagnostics.
	Body string
}

// Error implements error.
func (e *APIError) Error() string {
	msg := fmt.Sprintf("garmin: %s %s: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	if b := strings.TrimSpace(e.Body); b != "" {
		msg += ": " + b
	}
	return msg
}

// Unwrap lets errors.Is match the package sentinels.
func (e *APIError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusBadRequest:
		return ErrBadRequest
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	if e.StatusCode >= 500 && e.StatusCode <= 599 {
		return ErrServer
	}
	return nil
}
