package auth

// WithClock replaces time.Now, for the tests in package auth_test.
var WithClock = withClock

// Waiting returns how many callers are queued behind an exchange.
func (s *TokenSource) Waiting() int { return int(s.waiting.Load()) }
