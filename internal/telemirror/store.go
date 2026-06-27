package telemirror

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Store persists the user-added channel list. Defaults are pinned and
// always returned at the front of List() regardless of the file content.
type Store struct {
	path       string
	titlesPath string
	mu         sync.Mutex
	titles     map[string]string // in-memory cache, nil until first load
}

func NewStore(dataDir string) *Store {
	return &Store{
		path:       filepath.Join(dataDir, "telemirror_channels.json"),
		titlesPath: filepath.Join(dataDir, "telemirror_titles.json"),
	}
}

type subsFile struct {
	Channels []string `json:"channels"`
}

func (s *Store) loadLocked() []string {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	var f subsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil
	}
	return f.Channels
}

func (s *Store) saveLocked(chs []string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(subsFile{Channels: chs}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0600)
}

// List returns the full channel list with defaults pinned to the front.
func (s *Store) List() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.loadLocked()
	seen := make(map[string]bool, len(DefaultChannels)+len(user))
	out := make([]string, 0, len(DefaultChannels)+len(user))
	for _, d := range DefaultChannels {
		seen[strings.ToLower(d)] = true
		out = append(out, d)
	}
	for _, u := range user {
		clean := SanitizeUsername(u)
		if clean == "" || seen[strings.ToLower(clean)] {
			continue
		}
		seen[strings.ToLower(clean)] = true
		out = append(out, clean)
	}
	return out
}

func (s *Store) Add(username string) error {
	username = SanitizeUsername(username)
	if username == "" {
		return ErrEmptyUsername
	}
	if IsDefault(username) {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.loadLocked()
	for _, u := range user {
		if strings.EqualFold(u, username) {
			return nil
		}
	}
	return s.saveLocked(append(user, username))
}

// --- channel titles (persisted server-side so they're shared across ports,
// not stuck in one browser's localStorage) ---

// ensureTitlesLocked loads the titles file into the in-memory cache once.
func (s *Store) ensureTitlesLocked() {
	if s.titles != nil {
		return
	}
	s.titles = map[string]string{}
	b, err := os.ReadFile(s.titlesPath)
	if err != nil {
		return
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err == nil && m != nil {
		s.titles = m
	}
}

func (s *Store) saveTitlesLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.titlesPath), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.titles, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.titlesPath, b, 0600)
}

// SetTitle records/updates a channel's latest display title (keyed by lowercase
// username). Called whenever a channel is fetched, so the newest title wins.
// In-memory cache → no disk read per call; only writes when the title changed.
func (s *Store) SetTitle(username, title string) {
	username = strings.ToLower(SanitizeUsername(username))
	title = strings.TrimSpace(title)
	if username == "" || title == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTitlesLocked()
	if s.titles[username] == title {
		return
	}
	s.titles[username] = title
	_ = s.saveTitlesLocked()
}

// Titles returns a COPY of the lowercase-username → title map (callers must not
// mutate the cache).
func (s *Store) Titles() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTitlesLocked()
	out := make(map[string]string, len(s.titles))
	for k, v := range s.titles {
		out[k] = v
	}
	return out
}

func (s *Store) Remove(username string) error {
	username = SanitizeUsername(username)
	if username == "" {
		return ErrEmptyUsername
	}
	if IsDefault(username) {
		return ErrPinnedChannel
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.loadLocked()
	out := user[:0]
	for _, u := range user {
		if !strings.EqualFold(u, username) {
			out = append(out, u)
		}
	}
	return s.saveLocked(out)
}
