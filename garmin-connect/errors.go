package garminconnect

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	// ErrRateLimited is returned for HTTP 429 responses after retries are exhausted.
	ErrRateLimited = errors.New("garmin: rate limited")
	// ErrNoTokenSource is returned when a request needs credentials but the
	// client was built without WithTokenSource.
	ErrNoTokenSource = errors.New("garmin: no token source configured")
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
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	return nil
}
