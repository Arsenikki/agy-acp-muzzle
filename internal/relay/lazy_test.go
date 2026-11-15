package relay

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestFileCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cache.json")
	c := &FileCache{Path: path, Key: "k1", TTL: time.Hour}
	if i, n := c.Load(); i != nil || n != nil {
		t.Fatal("empty cache returned data")
	}
	c.SaveInitialize(json.RawMessage(`{"a":1}`))
	c.SaveSessionNew(json.RawMessage(`{"sessionId":"s1","models":{"x":1}}`))
	i, n := c.Load()
	if string(i) != `{"a":1}` || strings.Contains(string(n), "sessionId") || !strings.Contains(string(n), "models") {
		t.Fatalf("got %s / %s", i, n)
	}
	if i, _ := (&FileCache{Path: path, Key: "k2", TTL: time.Hour}).Load(); i != nil {
		t.Fatal("cache with another key was used")
	}
	if i, _ := (&FileCache{Path: path, Key: "k1", TTL: time.Nanosecond}).Load(); i != nil {
		t.Fatal("expired cache was used")
	}
}

func TestFileSessionStore(t *testing.T) {
	s := &FileSessionStore{Dir: filepath.Join(t.TempDir(), "sessions")}
	s.Store("11111111-2222-4333-8444-555555555555", "real-1")
	if r, ok := s.Lookup("11111111-2222-4333-8444-555555555555"); !ok || r != "real-1" {
		t.Fatalf("lookup = %q %v", r, ok)
	}
	if _, ok := s.Lookup("../etc/passwd"); ok {
		t.Fatal("unsafe id accepted")
	}
	s.Store("../x", "y")
	if _, ok := s.Lookup("missing"); ok {
		t.Fatal("missing id found")
	}
}

func TestNewPlaceholderIsUUIDv4(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		p := newPlaceholder()
		if !re.MatchString(p) || seen[p] {
			t.Fatalf("bad or repeated placeholder %q", p)
		}
		seen[p] = true
	}
}

func TestWithSessionID(t *testing.T) {
	got := withSessionID([]byte(`{"jsonrpc":"2.0","method":"x","params":{"sessionId":"a","k":1}}`), "b")
	if sessionIDOf(mustParams(t, got)) != "b" || !strings.Contains(string(got), `"k":1`) {
		t.Fatalf("got %s", got)
	}
	if string(withSessionID([]byte("junk"), "b")) != "junk" {
		t.Fatal("junk line changed")
	}
}

func mustParams(t *testing.T, line []byte) json.RawMessage {
	t.Helper()
	var m struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	return m.Params
}
