package mcpserver

import (
	"strings"
	"testing"
)

// The guidance given to agents must not point at anything the server does not
// have: the key can only be replaced by the user, on the server.
func TestInstructions_SayOnlyTheUserCanReplaceTheKey(t *testing.T) {
	t.Parallel()
	got := newHarness(t).connect(t).InitializeResult().Instructions
	if !strings.Contains(got, "only the user can replace it") || strings.Contains(got, "/auth") {
		t.Errorf("instructions = %q", got)
	}
}

func TestNew_RefusesExposedServerWithoutToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	base := Config{API: h.srv.api}
	for _, addr := range []string{"0.0.0.0:8808", ":8808", "192.168.1.5:8808", "example.com:80", "[::]:8808"} {
		cfg := base
		cfg.Addr = addr
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "token") {
			t.Errorf("Addr %q without token: err = %v, want a refusal", addr, err)
		}
		cfg.Token = "tok"
		if _, err := New(cfg); err != nil {
			t.Errorf("Addr %q with token: %v", addr, err)
		}
	}
	for _, addr := range []string{"", "127.0.0.1:8808", "localhost:8808", "[::1]:8808"} {
		cfg := base
		cfg.Addr = addr
		if _, err := New(cfg); err != nil {
			t.Errorf("loopback %q should be allowed without a token: %v", addr, err)
		}
	}
	if _, err := New(Config{}); err == nil {
		t.Error("a missing API must be an error")
	}
}
