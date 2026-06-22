package telemirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const ImageStaleTTL = 30 * 24 * time.Hour

// ImageCache stores image bytes on disk under <dir>/<key>.bin with a
// <key>.json metadata sidecar. Use Put/Get for URL-hash keys or
// PutByKey/GetByKey for stable identifiers like channel usernames.
type ImageCache struct {
	dir      string
	mu       sync.Mutex
	maxBytes int64
	curBytes int64 // -1 = unknown
}

type imageMeta struct {
	URL         string    `json:"url"`
	ContentType string    `json:"contentType"`
	StoredAt    time.Time `json:"storedAt"`
}

func NewImageCache(dir string) *ImageCache {
	return &ImageCache{dir: dir, curBytes: -1}
}

// SetMaxBytes sets the byte budget (0 = unlimited) and enforces immediately.
func (c *ImageCache) SetMaxBytes(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxBytes = n
	if n > 0 {
		c.enforceBudgetLocked()
	}
}

// Size returns the total bytes on disk (.bin + .json). Scans once if unknown.
func (c *ImageCache) Size() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.curBytes < 0 {
		c.scanSizeLocked()
	}
	return c.curBytes
}

func (c *ImageCache) scanSizeLocked() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		c.curBytes = 0
		return
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".bin") && !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	c.curBytes = total
}

func (c *ImageCache) enforceBudgetLocked() {
	if c.maxBytes <= 0 {
		return
	}
	if c.curBytes < 0 {
		c.scanSizeLocked()
	}
	if c.curBytes <= c.maxBytes {
		return
	}
	target := c.maxBytes * 9 / 10

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type fileEntry struct {
		key   string
		mtime time.Time
		total int64 // .bin + .json size
	}
	seen := map[string]*fileEntry{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var key string
		if strings.HasSuffix(name, ".bin") {
			key = strings.TrimSuffix(name, ".bin")
		} else if strings.HasSuffix(name, ".json") {
			key = strings.TrimSuffix(name, ".json")
		} else {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fe, ok := seen[key]
		if !ok {
			fe = &fileEntry{key: key, mtime: info.ModTime()}
			seen[key] = fe
		}
		fe.total += info.Size()
		if info.ModTime().Before(fe.mtime) {
			fe.mtime = info.ModTime()
		}
	}
	sorted := make([]fileEntry, 0, len(seen))
	for _, fe := range seen {
		sorted = append(sorted, *fe)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].mtime.Before(sorted[j].mtime) })
	for _, fe := range sorted {
		if c.curBytes <= target {
			break
		}
		var removed int64
		bp := filepath.Join(c.dir, fe.key+".bin")
		if info, err := os.Stat(bp); err == nil {
			if os.Remove(bp) == nil {
				removed += info.Size()
			}
		}
		mp := filepath.Join(c.dir, fe.key+".json")
		if info, err := os.Stat(mp); err == nil {
			if os.Remove(mp) == nil {
				removed += info.Size()
			}
		}
		c.curBytes -= removed
	}
}

func (c *ImageCache) keyFor(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

func (c *ImageCache) bodyPath(key string) string {
	return filepath.Join(c.dir, key+".bin")
}

func (c *ImageCache) metaPath(key string) string {
	return filepath.Join(c.dir, key+".json")
}

func (c *ImageCache) Get(rawURL string) ([]byte, string, bool) {
	if rawURL == "" {
		return nil, "", false
	}
	return c.getRaw(c.keyFor(rawURL))
}

func (c *ImageCache) Put(rawURL, contentType string, body []byte) error {
	if rawURL == "" || len(body) == 0 {
		return errors.New("telemirror imgcache: empty input")
	}
	return c.putRaw(c.keyFor(rawURL), rawURL, contentType, body)
}

func (c *ImageCache) GetByKey(key string) ([]byte, string, bool) {
	key = sanitizeFileKey(key)
	if key == "" {
		return nil, "", false
	}
	return c.getRaw(key)
}

func (c *ImageCache) PutByKey(key, contentType string, body []byte) error {
	key = sanitizeFileKey(key)
	if key == "" || len(body) == 0 {
		return errors.New("telemirror imgcache: empty input")
	}
	return c.putRaw(key, key, contentType, body)
}

// sanitizeFileKey constrains keys to [a-z0-9_-] and ≤64 chars.
func sanitizeFileKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			out = append(out, r)
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return string(out)
}

func (c *ImageCache) getRaw(key string) ([]byte, string, bool) {
	mb, err := os.ReadFile(c.metaPath(key))
	if err != nil {
		return nil, "", false
	}
	var m imageMeta
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, "", false
	}
	if !m.StoredAt.IsZero() && time.Since(m.StoredAt) > ImageStaleTTL {
		var freed int64
		if info, err := os.Stat(c.bodyPath(key)); err == nil {
			if os.Remove(c.bodyPath(key)) == nil {
				freed += info.Size()
			}
		}
		if info, err := os.Stat(c.metaPath(key)); err == nil {
			if os.Remove(c.metaPath(key)) == nil {
				freed += info.Size()
			}
		}
		c.mu.Lock()
		if c.curBytes >= freed {
			c.curBytes -= freed
		}
		c.mu.Unlock()
		return nil, "", false
	}
	body, err := os.ReadFile(c.bodyPath(key))
	if err != nil {
		return nil, "", false
	}
	ctype := m.ContentType
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	return body, ctype, true
}

// putRaw writes body and metadata atomically via tmp + rename.
func (c *ImageCache) putRaw(key, originURL, contentType string, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return err
	}

	// Track old sizes for delta.
	var oldBody, oldMeta int64
	if info, err := os.Stat(c.bodyPath(key)); err == nil {
		oldBody = info.Size()
	}
	if info, err := os.Stat(c.metaPath(key)); err == nil {
		oldMeta = info.Size()
	}

	tmpBody := c.bodyPath(key) + ".tmp"
	if err := os.WriteFile(tmpBody, body, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmpBody, c.bodyPath(key)); err != nil {
		_ = os.Remove(tmpBody)
		return err
	}
	meta := imageMeta{URL: originURL, ContentType: contentType, StoredAt: time.Now()}
	mb, err := json.Marshal(meta)
	if err != nil {
		_ = os.Remove(c.bodyPath(key))
		return err
	}
	tmpMeta := c.metaPath(key) + ".tmp"
	if err := os.WriteFile(tmpMeta, mb, 0600); err != nil {
		_ = os.Remove(c.bodyPath(key))
		return err
	}
	if err := os.Rename(tmpMeta, c.metaPath(key)); err != nil {
		_ = os.Remove(tmpMeta)
		_ = os.Remove(c.bodyPath(key))
		return err
	}
	if c.curBytes >= 0 {
		c.curBytes += int64(len(body)) + int64(len(mb)) - oldBody - oldMeta
	}
	c.enforceBudgetLocked()
	return nil
}

func (c *ImageCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".bin") && !strings.HasSuffix(name, ".json") {
			continue
		}
		_ = os.Remove(filepath.Join(c.dir, name))
	}
	c.curBytes = 0
}
