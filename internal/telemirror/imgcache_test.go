package telemirror

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageCachePutGetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	c := NewImageCache(filepath.Join(dir, "images"))

	url := "https://cdn4-telegram-org.translate.goog/file/abc.jpg"
	want := []byte("\xff\xd8\xff\xe0fake-jpeg")
	if err := c.Put(url, "image/jpeg", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ctype, ok := c.Get(url)
	if !ok {
		t.Fatalf("Get: miss after Put")
	}
	if ctype != "image/jpeg" {
		t.Errorf("ctype = %q, want image/jpeg", ctype)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(want))
	}
}

func TestImageCacheSurvivesNewInstance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	first := NewImageCache(dir)
	url := "https://cdn1-telesco-pe.translate.goog/file/avatar.jpg"
	body := []byte("avatar-bytes")
	if err := first.Put(url, "image/jpeg", body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	second := NewImageCache(dir)
	got, ctype, ok := second.Get(url)
	if !ok {
		t.Fatalf("Get on fresh instance: miss")
	}
	if ctype != "image/jpeg" || !bytes.Equal(got, body) {
		t.Errorf("got (%q, %q), want (%q, %q)", ctype, got, "image/jpeg", body)
	}
}

func TestImageCacheGetMissOnEmpty(t *testing.T) {
	c := NewImageCache(t.TempDir())
	if _, _, ok := c.Get(""); ok {
		t.Errorf("Get(\"\") = ok, want miss")
	}
	if _, _, ok := c.Get("https://nope.translate.goog/x.jpg"); ok {
		t.Errorf("Get on empty cache = ok, want miss")
	}
}

func TestImageCachePutRejectsEmptyInput(t *testing.T) {
	c := NewImageCache(t.TempDir())
	if err := c.Put("", "image/jpeg", []byte("x")); err == nil {
		t.Errorf("Put with empty url succeeded, want error")
	}
	if err := c.Put("https://x.translate.goog/a", "image/jpeg", nil); err == nil {
		t.Errorf("Put with empty body succeeded, want error")
	}
}

func TestImageCacheClearWipesEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c := NewImageCache(dir)
	if err := c.Put("https://a.translate.goog/1", "image/jpeg", []byte("a")); err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	if err := c.Put("https://b.translate.goog/2", "image/png", []byte("b")); err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	c.Clear()
	if _, _, ok := c.Get("https://a.translate.goog/1"); ok {
		t.Errorf("entry 1 survived Clear")
	}
	if _, _, ok := c.Get("https://b.translate.goog/2"); ok {
		t.Errorf("entry 2 survived Clear")
	}
	// Clear on empty/missing dir must not panic.
	c.Clear()
	c2 := NewImageCache(filepath.Join(t.TempDir(), "never-created"))
	c2.Clear()
}

func TestImageCachePutByKeyStableAcrossRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	first := NewImageCache(dir)
	body := []byte("avatar-bytes-v1")
	if err := first.PutByKey("networkti", "image/jpeg", body); err != nil {
		t.Fatalf("PutByKey: %v", err)
	}
	second := NewImageCache(dir)
	got, ctype, ok := second.GetByKey("networkti")
	if !ok {
		t.Fatalf("GetByKey on fresh instance: miss")
	}
	if ctype != "image/jpeg" || !bytes.Equal(got, body) {
		t.Errorf("got (%q, %q), want (%q, %q)", ctype, got, "image/jpeg", body)
	}
}

func TestImageCacheKeyByKeyNormalisesCase(t *testing.T) {
	c := NewImageCache(t.TempDir())
	if err := c.PutByKey("NetworkTI", "image/jpeg", []byte("x")); err != nil {
		t.Fatalf("PutByKey: %v", err)
	}
	if _, _, ok := c.GetByKey("networkti"); !ok {
		t.Errorf("lower-case lookup missed")
	}
	if _, _, ok := c.GetByKey("NETWORKTI"); !ok {
		t.Errorf("upper-case lookup missed")
	}
}

func TestImageCacheKeyByKeyStripsPathEscape(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	c := NewImageCache(cacheDir)
	_ = c.PutByKey("../../etc/passwd", "image/jpeg", []byte("x"))
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasPrefix(p, cacheDir+string(filepath.Separator)) && p != cacheDir {
			t.Errorf("file written outside cache dir: %s", p)
		}
		return nil
	})
}

func TestImageCacheStaleEntryEvicted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c := NewImageCache(dir)
	url := "https://stale.translate.goog/x.jpg"
	if err := c.Put(url, "image/jpeg", []byte("old")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	key := c.keyFor(url)
	mb, err := json.Marshal(imageMeta{
		URL:         url,
		ContentType: "image/jpeg",
		StoredAt:    time.Now().Add(-2 * ImageStaleTTL),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(c.metaPath(key), mb, 0600); err != nil {
		t.Fatalf("rewrite meta: %v", err)
	}
	if _, _, ok := c.Get(url); ok {
		t.Errorf("stale entry returned hit")
	}
	if _, err := os.Stat(c.bodyPath(key)); !os.IsNotExist(err) {
		t.Errorf("body still on disk: err=%v", err)
	}
	if _, err := os.Stat(c.metaPath(key)); !os.IsNotExist(err) {
		t.Errorf("meta still on disk: err=%v", err)
	}
}

func TestImageCacheSizeTracking(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c := NewImageCache(dir)
	if s := c.Size(); s != 0 {
		t.Fatalf("empty Size = %d, want 0", s)
	}
	c.PutByKey("a", "image/jpeg", make([]byte, 500))
	s1 := c.Size()
	if s1 <= 0 {
		t.Fatalf("after put Size = %d, want >0", s1)
	}
	c.PutByKey("b", "image/png", make([]byte, 500))
	s2 := c.Size()
	if s2 <= s1 {
		t.Fatalf("after second put Size = %d, want > %d", s2, s1)
	}
	c.Clear()
	if s := c.Size(); s != 0 {
		t.Fatalf("after Clear Size = %d, want 0", s)
	}
}

func TestImageCacheBudgetEvictsOldest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c := NewImageCache(dir)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("img%d", i)
		c.PutByKey(key, "image/jpeg", make([]byte, 200))
		mtime := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(c.bodyPath(key), mtime, mtime)
		os.Chtimes(c.metaPath(key), mtime, mtime)
	}
	total := c.Size()
	// Budget for 3 entries: enforce to 90% = 2.7 entries → keep 2.
	perEntry := total / 5
	c.SetMaxBytes(perEntry * 3)

	after := c.Size()
	if after > perEntry*3 {
		t.Fatalf("after SetMaxBytes: Size=%d > budget=%d", after, perEntry*3)
	}
	// Oldest (img0, img1, img2) should be evicted; img3, img4 survive.
	if _, _, ok := c.GetByKey("img0"); ok {
		t.Fatal("img0 (oldest) should be evicted")
	}
	if _, _, ok := c.GetByKey("img4"); !ok {
		t.Fatal("img4 (newest) should survive")
	}
}

func TestImageCacheBudgetOnPut(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c := NewImageCache(dir)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("img%d", i)
		c.PutByKey(key, "image/jpeg", make([]byte, 200))
		mtime := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(c.bodyPath(key), mtime, mtime)
		os.Chtimes(c.metaPath(key), mtime, mtime)
	}
	perEntry := c.Size() / 3
	// Budget for all 3 + half an entry. Adding a 4th triggers eviction.
	c.SetMaxBytes(perEntry*3 + perEntry/2)

	c.PutByKey("img3", "image/jpeg", make([]byte, 200))
	// img0 (oldest) should be evicted.
	if _, _, ok := c.GetByKey("img0"); ok {
		t.Fatal("img0 should be evicted after Put")
	}
	if _, _, ok := c.GetByKey("img3"); !ok {
		t.Fatal("img3 (new) should exist")
	}
}

func TestImageCacheUnlimitedDefault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c := NewImageCache(dir)
	for i := 0; i < 20; i++ {
		c.PutByKey(fmt.Sprintf("img%d", i), "image/jpeg", make([]byte, 100))
	}
	for i := 0; i < 20; i++ {
		if _, _, ok := c.GetByKey(fmt.Sprintf("img%d", i)); !ok {
			t.Fatalf("entry %d missing in unlimited mode", i)
		}
	}
}

func TestImageCacheScanSizeOnFreshInstance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images")
	c1 := NewImageCache(dir)
	c1.PutByKey("x", "image/jpeg", make([]byte, 300))
	c2 := NewImageCache(dir)
	if s := c2.Size(); s <= 0 {
		t.Fatalf("fresh cache over existing dir: Size = %d, want >0", s)
	}
}
