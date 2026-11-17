package mcpserver

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testMasterKey(t *testing.T) []byte {
	t.Helper()
	k, err := ParseMasterKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func openStore(t *testing.T, path string, key []byte) *UserStore {
	t.Helper()
	s, err := OpenUserStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseMasterKey(t *testing.T) {
	t.Parallel()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	tests := []struct {
		name, in string
		ok       bool
	}{
		{"hex", hex.EncodeToString(raw), true},
		{"hex with newline", hex.EncodeToString(raw) + "\n", true},
		{"base64", base64.StdEncoding.EncodeToString(raw), true},
		{"base64 no padding", base64.RawStdEncoding.EncodeToString(raw), true},
		{"base64url", base64.URLEncoding.EncodeToString(raw), true},
		{"empty", "", false},
		{"too short hex", "abcd", false},
		{"31 bytes", hex.EncodeToString(raw[:31]), false},
		{"33 bytes", hex.EncodeToString(append(raw, 1)), false},
		{"garbage", "not a key at all!!", false},
	}
	for _, tc := range tests {
		got, err := ParseMasterKey(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if tc.ok && string(got) != string(raw) {
			t.Errorf("%s: wrong bytes", tc.name)
		}
	}
	if _, err := OpenUserStore(filepath.Join(t.TempDir(), "u.json"), raw[:16]); err == nil {
		t.Error("a 16-byte key must be refused")
	}
}

func TestUserStore_AddAndLookup(t *testing.T) {
	t.Parallel()
	s := openStore(t, filepath.Join(t.TempDir(), "u.json"), testMasterKey(t))
	u, token, err := s.Add("  Alice  ", " key-for-alice \n")
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "Alice" || !strings.HasPrefix(u.ID, "u_") || u.CreatedAt.IsZero() {
		t.Errorf("user = %+v", u)
	}
	if !strings.HasPrefix(token, tokenPrefix) || len(token) != len(tokenPrefix)+64 {
		t.Errorf("token %q should be %q plus 64 hex characters", token, tokenPrefix)
	}
	if got, ok := s.Lookup(token); !ok || got.ID != u.ID {
		t.Errorf("Lookup(token) = %+v %v", got, ok)
	}
	for _, bad := range []string{"", "gmcp_", token + "x", token[:len(token)-1], strings.ToUpper(token), "Bearer " + token} {
		if _, ok := s.Lookup(bad); ok {
			t.Errorf("Lookup(%q) must fail", bad)
		}
	}
	if key, err := s.APIKey(u.ID); err != nil || key != "key-for-alice" {
		t.Errorf("APIKey = %q, %v (whitespace is trimmed)", key, err)
	}
	// Two users never share a token.
	_, token2, _ := s.Add("Bob", "k2")
	if token == token2 {
		t.Error("tokens collided")
	}
}

func TestUserStore_NameRules(t *testing.T) {
	t.Parallel()
	s := openStore(t, filepath.Join(t.TempDir(), "u.json"), testMasterKey(t))
	for _, ok := range []string{"a", "Alice", "alice.smith", "bob_2", "carol@example.com", "Dan-the-man", "Zoë", "李雷", "a b", strings.Repeat("x", 64)} {
		if err := s.CheckName(ok); err != nil {
			t.Errorf("CheckName(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", "   ", " ", "-alice", ".alice", "_x", "ali/ce", "ali\nce", "ali\"ce", `a\b`, "a;b", "<script>", strings.Repeat("x", 65), "tab\there"} {
		if err := s.CheckName(bad); err != ErrInvalidName {
			t.Errorf("CheckName(%q) = %v, want ErrInvalidName", bad, err)
		}
	}
	if _, _, err := s.Add("Alice", "k"); err != nil {
		t.Fatal(err)
	}
	for _, dup := range []string{"Alice", "alice", "ALICE", "  alice  "} {
		if _, _, err := s.Add(dup, "k"); err != ErrUserExists {
			t.Errorf("Add(%q) = %v, want ErrUserExists", dup, err)
		}
		if err := s.CheckName(dup); err != ErrUserExists {
			t.Errorf("CheckName(%q) = %v, want ErrUserExists", dup, err)
		}
	}
	if _, _, err := s.Add("Fine", "   "); err != ErrNoKey {
		t.Errorf("empty key: %v", err)
	}
}

// The file is what an attacker who copies the disk gets: it must hold neither
// the tokens, nor the Garmin keys, nor anything that would let them log in.
func TestUserStore_FileHoldsNoSecrets(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "users.json")
	s := openStore(t, path, testMasterKey(t))
	const garminKey = "GARMIN-KEY-PLAINTEXT-MUST-NOT-APPEAR"
	_, token, err := s.Add("Alice", garminKey)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{garminKey, token, strings.TrimPrefix(token, tokenPrefix), "GARMIN-KEY"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the users file contains %q:\n%s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), hashToken(token)) {
		t.Error("the token should be stored as its SHA-256 hash")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", info.Mode().Perm())
	}
	// A stolen copy does not authenticate: the hash is not the token.
	if _, ok := s.Lookup(hashToken(token)); ok {
		t.Error("the stored hash must not be usable as a token")
	}
	var f usersFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != usersFileVersion || len(f.Users) != 1 {
		t.Errorf("file = %s (%v)", raw, err)
	}
}

func TestUserStore_SurvivesAReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "users.json")
	key := testMasterKey(t)
	s := openStore(t, path, key)
	a, ta, _ := s.Add("Alice", "key-a")
	b, tb, _ := s.Add("Bob", "key-b")

	s2 := openStore(t, path, key)
	for _, c := range []struct {
		u     User
		token string
		key   string
	}{{a, ta, "key-a"}, {b, tb, "key-b"}} {
		got, ok := s2.Lookup(c.token)
		if !ok || got.ID != c.u.ID || got.Name != c.u.Name || !got.CreatedAt.Equal(c.u.CreatedAt) {
			t.Errorf("%s: lookup after reopen = %+v %v", c.u.Name, got, ok)
		}
		if k, err := s2.APIKey(c.u.ID); err != nil || k != c.key {
			t.Errorf("%s: key after reopen = %q %v", c.u.Name, k, err)
		}
	}
	if names := fmt.Sprint(s2.List()); !strings.Contains(names, "Alice") || strings.Index(names, "Alice") > strings.Index(names, "Bob") {
		t.Errorf("List should be ordered by name: %s", names)
	}
}

func TestUserStore_RefusesTheWrongMasterKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "users.json")
	s := openStore(t, path, testMasterKey(t))
	_, _, _ = s.Add("Alice", "key-a")

	other, _ := ParseMasterKey(strings.Repeat("cd", 32))
	_, err := OpenUserStore(path, other)
	if err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("err = %v, want a clear master-key error at open time", err)
	}
	// An empty store has nothing to decrypt, so any key opens it.
	if _, err := OpenUserStore(filepath.Join(t.TempDir(), "new.json"), other); err != nil {
		t.Errorf("a new store: %v", err)
	}
}

// Moving one user's encrypted key onto another user's record must be detected,
// because each ciphertext is bound to its owner's id.
func TestUserStore_DetectsSwappedCiphertexts(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "users.json")
	key := testMasterKey(t)
	s := openStore(t, path, key)
	_, _, _ = s.Add("Alice", "key-a")
	_, _, _ = s.Add("Bob", "key-b")

	raw, _ := os.ReadFile(path)
	var f usersFile
	_ = json.Unmarshal(raw, &f)
	f.Users[0].KeyEnc, f.Users[1].KeyEnc = f.Users[1].KeyEnc, f.Users[0].KeyEnc
	swapped, _ := json.Marshal(f)
	if err := os.WriteFile(path, swapped, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUserStore(path, key); err == nil {
		t.Error("swapped ciphertexts were accepted: a user could be given someone else's Garmin key")
	}
}

func TestUserStore_RejectsDamagedFiles(t *testing.T) {
	t.Parallel()
	key := testMasterKey(t)
	good := func() usersFile {
		p := filepath.Join(t.TempDir(), "u.json")
		s := openStore(t, p, key)
		_, _, _ = s.Add("Alice", "k")
		raw, _ := os.ReadFile(p)
		var f usersFile
		_ = json.Unmarshal(raw, &f)
		return f
	}
	write := func(v any) string {
		p := filepath.Join(t.TempDir(), "u.json")
		b, _ := json.Marshal(v)
		_ = os.WriteFile(p, b, 0o600)
		return p
	}
	cases := map[string]string{}
	f := good()
	f.Version = 99
	cases["unknown version"] = write(f)
	f = good()
	f.Users = append(f.Users, f.Users[0]) // same id and name twice
	cases["duplicate entries"] = write(f)
	f = good()
	f.Users[0].TokenHash = ""
	cases["empty token hash"] = write(f)
	f = good()
	f.Users[0].KeyEnc = "!!not base64!!"
	cases["bad ciphertext"] = write(f)
	bad := filepath.Join(t.TempDir(), "u.json")
	_ = os.WriteFile(bad, []byte("{not json"), 0o600)
	cases["not json"] = bad
	for name, p := range cases {
		if _, err := OpenUserStore(p, key); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestUserStore_RotateSetKeyRemove(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "users.json")
	key := testMasterKey(t)
	s := openStore(t, path, key)
	u, old, _ := s.Add("Alice", "key-1")

	fresh, err := s.RotateToken(u.ID)
	if err != nil || fresh == old || !strings.HasPrefix(fresh, tokenPrefix) {
		t.Fatalf("RotateToken = %q, %v", fresh, err)
	}
	if _, ok := s.Lookup(old); ok {
		t.Error("the old token still works after rotation")
	}
	if got, ok := s.Lookup(fresh); !ok || got.ID != u.ID {
		t.Error("the new token does not work")
	}

	if err := s.SetAPIKey(u.ID, " key-2 "); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.APIKey(u.ID); k != "key-2" {
		t.Errorf("key after SetAPIKey = %q", k)
	}
	if err := s.SetAPIKey(u.ID, "  "); err != ErrNoKey {
		t.Errorf("empty key: %v", err)
	}

	// Everything above was written to disk.
	s2 := openStore(t, path, key)
	if _, ok := s2.Lookup(old); ok {
		t.Error("the old token came back after a reopen")
	}
	if k, _ := s2.APIKey(u.ID); k != "key-2" {
		t.Errorf("key after reopen = %q", k)
	}

	if err := s.Remove(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(fresh); ok {
		t.Error("a removed user's token still works")
	}
	if _, err := s.APIKey(u.ID); err != ErrUserNotFound {
		t.Errorf("APIKey of a removed user: %v", err)
	}
	if s3 := openStore(t, path, key); len(s3.List()) != 0 {
		t.Error("the removal was not persisted")
	}
	missing := map[string]error{
		"Remove": s.Remove("u_nope"),
		"SetKey": s.SetAPIKey("u_nope", "k"),
	}
	_, missing["Get"] = s.Get("u_nope")
	_, missing["Rotate"] = s.RotateToken("u_nope")
	_, missing["APIKey"] = s.APIKey("u_nope")
	for name, err := range missing {
		if err != ErrUserNotFound {
			t.Errorf("%s of an unknown user: %v", name, err)
		}
	}
	// A name freed by removal can be registered again, by a new identity.
	if u2, _, err := s.Add("Alice", "key-3"); err != nil || u2.ID == u.ID {
		t.Errorf("re-adding a removed name: %+v %v", u2, err)
	}
}

// A change that cannot be written must not take effect, or the running server
// would hold users the file does not.
func TestUserStore_FailedWriteChangesNothing(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	s := openStore(t, filepath.Join(base, "state", "users.json"), testMasterKey(t)) // opens: nothing exists yet
	if _, _, err := s.Add("Alice", "k"); err != nil {
		t.Fatal(err)
	}
	// Now make the store's directory impossible to use: a file takes its place.
	if err := os.RemoveAll(filepath.Join(base, "state")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "state"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Add("Bob", "k"); err == nil {
		t.Fatal("Add should fail when the file cannot be written")
	}
	names := fmt.Sprint(s.List())
	if strings.Contains(names, "Bob") || !strings.Contains(names, "Alice") {
		t.Errorf("a failed Add must leave the store exactly as it was, got %s", names)
	}
	if _, err := s.RotateToken(s.List()[0].ID); err == nil {
		t.Error("RotateToken should fail too")
	}
}

func TestUserStore_ConcurrentUse(t *testing.T) {
	t.Parallel()
	s := openStore(t, filepath.Join(t.TempDir(), "users.json"), testMasterKey(t))
	var wg sync.WaitGroup
	tokens := make([]string, 20)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, tok, err := s.Add(fmt.Sprintf("user%02d", i), fmt.Sprintf("key-%d", i))
			if err != nil {
				t.Error(err)
				return
			}
			tokens[i] = tok
			s.Lookup(tok)
			s.List()
		}()
	}
	wg.Wait()
	if n := len(s.List()); n != 20 {
		t.Fatalf("%d users, want 20", n)
	}
	for i, tok := range tokens {
		if u, ok := s.Lookup(tok); !ok || u.Name != fmt.Sprintf("user%02d", i) {
			t.Errorf("token %d resolves to %+v %v", i, u, ok)
		}
	}
}
