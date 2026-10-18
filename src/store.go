package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type JobOptions struct {
	OutMode  string   `json:"outMode"` // "here" | "dir"
	OutDir   string   `json:"outDir"`
	Smart    bool     `json:"smart"`
	After    string   `json:"after"` // "keep" | "move" | "delete"
	MoveTo   string   `json:"moveTo"`
	SkipDone bool     `json:"skipDone"`
	Extra    []string `json:"passwords,omitempty"` // one-off passwords for this job
}

type WatchSettings struct {
	Enabled     bool       `json:"enabled"`
	Dirs        []string   `json:"dirs"`
	IntervalMin int        `json:"intervalMin"`
	StableMin   int        `json:"stableMin"`
	Opts        JobOptions `json:"opts"`
}

type Settings struct {
	PassHash  string           `json:"passHash"`
	PassSalt  string           `json:"passSalt"`
	Passwords []string         `json:"passwords"`
	Defaults  JobOptions       `json:"defaults"`
	Watch     WatchSettings    `json:"watch"`
	Sessions  map[string]int64 `json:"sessions"`
}

type Store struct {
	mu   sync.Mutex
	dir  string
	S    Settings
	done map[string]int64
}

func defaultSettings() Settings {
	d := JobOptions{OutMode: "here", Smart: true, After: "keep", SkipDone: true}
	return Settings{
		Defaults: d,
		Watch:    WatchSettings{IntervalMin: 10, StableMin: 3, Opts: d},
		Sessions: map[string]int64{},
	}
}

func openStore(dir string) *Store {
	os.MkdirAll(dir, 0o755)
	st := &Store{dir: dir, S: defaultSettings(), done: map[string]int64{}}
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		json.Unmarshal(b, &st.S)
	}
	if st.S.Sessions == nil {
		st.S.Sessions = map[string]int64{}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "done.json")); err == nil {
		json.Unmarshal(b, &st.done)
	}
	return st
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (st *Store) saveLocked() error { return writeJSON(filepath.Join(st.dir, "settings.json"), st.S) }

func (st *Store) Update(fn func(s *Settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	fn(&st.S)
	return st.saveLocked()
}

func (st *Store) Get() Settings {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.S
	s.Passwords = append([]string(nil), st.S.Passwords...)
	s.Watch.Dirs = append([]string(nil), st.S.Watch.Dirs...)
	return s
}

// --- done records ---

func (st *Store) IsDone(key string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.done[key]
	return ok
}

func (st *Store) MarkDone(key string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.done[key] = time.Now().Unix()
	// keep the record bounded
	if len(st.done) > 20000 {
		var oldestK string
		var oldest int64 = 1 << 62
		for k, v := range st.done {
			if v < oldest {
				oldest, oldestK = v, k
			}
		}
		delete(st.done, oldestK)
	}
	writeJSON(filepath.Join(st.dir, "done.json"), st.done)
}

func (st *Store) ClearDone() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.done = map[string]int64{}
	writeJSON(filepath.Join(st.dir, "done.json"), st.done)
}

// --- access password & sessions ---

func hashPass(pw, salt string) string {
	k, _ := pbkdf2.Key(sha256.New, pw, []byte(salt), 120000, 32)
	return hex.EncodeToString(k)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (st *Store) NeedSetup() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.S.PassHash == ""
}

func (st *Store) SetPassword(pw string) error {
	return st.Update(func(s *Settings) {
		s.PassSalt = randHex(16)
		s.PassHash = hashPass(pw, s.PassSalt)
		s.Sessions = map[string]int64{}
	})
}

func (st *Store) CheckPassword(pw string) bool {
	s := st.Get()
	if s.PassHash == "" {
		return false
	}
	h := hashPass(pw, s.PassSalt)
	return subtle.ConstantTimeCompare([]byte(h), []byte(s.PassHash)) == 1
}

const sessionTTL = 30 * 24 * time.Hour

func (st *Store) NewSession() string {
	tok := randHex(24)
	st.Update(func(s *Settings) {
		now := time.Now().Unix()
		for k, exp := range s.Sessions {
			if exp < now {
				delete(s.Sessions, k)
			}
		}
		s.Sessions[tok] = time.Now().Add(sessionTTL).Unix()
	})
	return tok
}

func (st *Store) ValidSession(tok string) bool {
	if tok == "" {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	exp, ok := st.S.Sessions[tok]
	return ok && exp > time.Now().Unix()
}

func (st *Store) DropSession(tok string) {
	st.Update(func(s *Settings) { delete(s.Sessions, tok) })
}
