package relay

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Clients such as Paseo probe a provider by starting it, sending initialize and
// session/new to learn its modes and models, and killing it. Against the
// kernel each probe is a full start (Python runtime, harness, websocket). With
// a Cache the supervisor answers those two requests from the kernel's last
// real answers without starting anything, handing out a placeholder session
// id; the kernel session is created when the client first uses that id.

// Cache keeps the kernel's last initialize and session/new results.
type Cache interface {
	// Load returns the cached results; either may be nil.
	Load() (initialize, sessionNew json.RawMessage)
	SaveInitialize(result json.RawMessage)
	// SaveSessionNew stores a session/new result without its sessionId.
	SaveSessionNew(result json.RawMessage)
}

// CacheLocker is implemented by caches that can serialise filling an empty
// cache across processes.
type CacheLocker interface {
	LockFill(timeout time.Duration) (release func(), err error)
}

// SessionStore persists placeholder -> kernel session ids, so a client that
// reconnects (e.g. after a daemon restart) can still session/load a
// placeholder it was given.
type SessionStore interface {
	Lookup(placeholder string) (string, bool)
	Store(placeholder, real string)
}

// lazySession is a session handed out from the cache.
type lazySession struct {
	params json.RawMessage // session/new params as forwarded (after policy)
	real   string          // kernel session id once created
}

// newPlaceholder returns a random UUIDv4-shaped id.
func newPlaceholder() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sessionIDOf returns params.sessionId of a message, or "".
func sessionIDOf(params json.RawMessage) string {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(params, &p)
	return p.SessionID
}

// withSessionID returns line with params.sessionId replaced. A line that does
// not decode is returned unchanged.
func withSessionID(line []byte, id string) []byte {
	var msg map[string]json.RawMessage
	if json.Unmarshal(line, &msg) != nil {
		return line
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(msg["params"], &params) != nil {
		return line
	}
	params["sessionId"], _ = json.Marshal(id)
	msg["params"], _ = json.Marshal(params)
	out, err := json.Marshal(msg)
	if err != nil {
		return line
	}
	return out
}

// withResultSessionID returns a session/new result with sessionId set.
func withResultSessionID(result json.RawMessage, id string) json.RawMessage {
	var r map[string]json.RawMessage
	if json.Unmarshal(result, &r) != nil {
		r = map[string]json.RawMessage{}
	}
	r["sessionId"], _ = json.Marshal(id)
	out, _ := json.Marshal(r)
	return out
}

// FileCache is a Cache in one JSON file. Entries expire after TTL and are
// ignored when Key (e.g. the kernel binary's identity) changes.
type FileCache struct {
	Path string
	Key  string
	TTL  time.Duration
}

type fileCacheData struct {
	Key        string          `json:"key"`
	SavedAt    time.Time       `json:"savedAt"`
	Initialize json.RawMessage `json:"initialize,omitempty"`
	SessionNew json.RawMessage `json:"sessionNew,omitempty"`
}

func (c *FileCache) read() fileCacheData {
	var d fileCacheData
	b, err := os.ReadFile(c.Path)
	if err != nil || json.Unmarshal(b, &d) != nil || d.Key != c.Key {
		return fileCacheData{Key: c.Key}
	}
	if c.TTL > 0 && time.Since(d.SavedAt) > c.TTL {
		return fileCacheData{Key: c.Key}
	}
	return d
}

func (c *FileCache) write(d fileCacheData) {
	d.Key, d.SavedAt = c.Key, time.Now()
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	_ = writeFileAtomic(c.Path, b)
}

// Load implements Cache.
func (c *FileCache) Load() (json.RawMessage, json.RawMessage) {
	d := c.read()
	return d.Initialize, d.SessionNew
}

// SaveInitialize implements Cache.
func (c *FileCache) SaveInitialize(result json.RawMessage) {
	d := c.read()
	d.Initialize = result
	c.write(d)
}

// SaveSessionNew implements Cache.
func (c *FileCache) SaveSessionNew(result json.RawMessage) {
	var r map[string]json.RawMessage
	if json.Unmarshal(result, &r) != nil {
		return
	}
	delete(r, "sessionId")
	stripped, _ := json.Marshal(r)
	d := c.read()
	d.SessionNew = stripped
	c.write(d)
}

// FileSessionStore keeps one small file per placeholder in Dir.
type FileSessionStore struct {
	Dir string
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Lookup implements SessionStore.
func (s *FileSessionStore) Lookup(placeholder string) (string, bool) {
	if !safeID.MatchString(placeholder) {
		return "", false
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, placeholder))
	if err != nil || !safeID.Match(b) {
		return "", false
	}
	return string(b), true
}

// Store implements SessionStore.
func (s *FileSessionStore) Store(placeholder, real string) {
	if !safeID.MatchString(placeholder) || !safeID.MatchString(real) {
		return
	}
	_ = writeFileAtomic(filepath.Join(s.Dir, placeholder), []byte(real))
}

func dirOf(path string) string { return filepath.Dir(path) }

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}
