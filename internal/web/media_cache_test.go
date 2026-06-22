package web

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMediaDiskCache_PutGetRemove(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("hello")
	if err := c.Put(5, 0xab, body, "text/plain"); err != nil {
		t.Fatal(err)
	}
	got, mime, ok := c.Get(5, 0xab)
	if !ok || mime != "text/plain" || string(got) != "hello" {
		t.Fatalf("Get = %q %q %v", got, mime, ok)
	}
	c.Remove(5, 0xab)
	if _, _, ok := c.Get(5, 0xab); ok {
		t.Fatal("expected miss after Remove")
	}
}

func TestMediaDiskCache_SizeTracking(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if s := c.Size(); s != 0 {
		t.Fatalf("empty cache Size = %d, want 0", s)
	}

	body := make([]byte, 100)
	if err := c.Put(100, 1, body, "x/y"); err != nil {
		t.Fatal(err)
	}
	s1 := c.Size()
	if s1 <= 0 {
		t.Fatalf("after put Size = %d, want >0", s1)
	}

	body2 := make([]byte, 200)
	if err := c.Put(200, 2, body2, "x/y"); err != nil {
		t.Fatal(err)
	}
	s2 := c.Size()
	if s2 <= s1 {
		t.Fatalf("after second put Size = %d, want > %d", s2, s1)
	}

	c.Remove(100, 1)
	s3 := c.Size()
	if s3 >= s2 {
		t.Fatalf("after Remove Size = %d, want < %d", s3, s2)
	}
}

func TestMediaDiskCache_BudgetEvictsOldest(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Put 5 entries of ~110 bytes each on disk (body=100 + 2-byte header + mime).
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		body := make([]byte, 100)
		body[0] = byte(i)
		if err := c.Put(100, uint32(i+1), body, "a/b"); err != nil {
			t.Fatal(err)
		}
		// Space mtimes apart so sort order is deterministic; oldest = crc 1.
		path := c.keyFile(100, uint32(i+1))
		mtime := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(path, mtime, mtime)
	}

	totalBefore := c.Size()
	if totalBefore == 0 {
		t.Fatal("expected non-zero size before budget")
	}

	// Set budget to ~60% of total: should evict the oldest entries.
	budget := totalBefore * 60 / 100
	c.SetMaxBytes(budget)

	totalAfter := c.Size()
	if totalAfter > budget {
		t.Fatalf("after SetMaxBytes(%d): Size = %d, want <= budget", budget, totalAfter)
	}

	// The newest entry (crc=5) should survive.
	if _, _, ok := c.Get(100, 5); !ok {
		t.Fatal("newest entry (crc=5) should survive eviction")
	}
	// The oldest entry (crc=1) should be evicted.
	if _, _, ok := c.Get(100, 1); ok {
		t.Fatal("oldest entry (crc=1) should have been evicted")
	}
}

func TestMediaDiskCache_BudgetEnforceOnPut(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-fill 3 entries with past mtimes so new Put is always newest.
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		body := make([]byte, 100)
		if err := c.Put(100, uint32(i+1), body, "a/b"); err != nil {
			t.Fatal(err)
		}
		path := c.keyFile(100, uint32(i+1))
		mtime := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(path, mtime, mtime)
	}

	sizePerEntry := c.Size() / 3
	// Budget big enough for all 3 + the upcoming 4th, but just barely triggers
	// eviction when 4th is added (total 4 > budget 3.5).
	c.SetMaxBytes(sizePerEntry*3 + sizePerEntry/2)

	body := make([]byte, 100)
	if err := c.Put(100, 4, body, "a/b"); err != nil {
		t.Fatal(err)
	}

	// crc=1 (oldest) should be evicted.
	if _, _, ok := c.Get(100, 1); ok {
		t.Fatal("oldest entry should have been evicted by Put")
	}
	// crc=4 (newest) should exist.
	if _, _, ok := c.Get(100, 4); !ok {
		t.Fatal("newest entry should exist")
	}
}

func TestMediaDiskCache_UnlimitedDefault(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Without SetMaxBytes, Put should never evict.
	for i := 0; i < 20; i++ {
		body := make([]byte, 1000)
		if err := c.Put(1000, uint32(i+1), body, "a/b"); err != nil {
			t.Fatal(err)
		}
	}
	// All 20 should exist.
	for i := 0; i < 20; i++ {
		if _, _, ok := c.Get(1000, uint32(i+1)); !ok {
			t.Fatalf("entry %d missing in unlimited mode", i+1)
		}
	}
}

func TestMediaDiskCache_ClearResetsSize(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 500)
	c.Put(500, 1, body, "a/b")
	if c.Size() == 0 {
		t.Fatal("expected non-zero before Clear")
	}
	c.Clear()
	if s := c.Size(); s != 0 {
		t.Fatalf("after Clear: Size = %d, want 0", s)
	}
}

func TestMediaDiskCache_CleanupTracksCurBytes(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 100)
	c.Put(100, 1, body, "a/b")
	before := c.Size()
	if before == 0 {
		t.Fatal("expected non-zero")
	}
	// Backdate the file so it expires.
	path := c.keyFile(100, 1)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)

	n := c.Cleanup()
	if n != 1 {
		t.Fatalf("Cleanup removed %d, want 1", n)
	}
	after := c.Size()
	if after != 0 {
		t.Fatalf("after Cleanup: Size = %d, want 0", after)
	}
}

func TestMediaDiskCache_ScanSizeOnFirstCall(t *testing.T) {
	dir := t.TempDir()
	// Write a file manually before creating the cache, to test scan-on-first-Size.
	c1, _ := newMediaDiskCache(dir, time.Hour)
	body := make([]byte, 300)
	c1.Put(300, 42, body, "x/y")

	// Create a new cache pointing at the same dir — curBytes starts at -1.
	c2, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s := c2.Size()
	if s <= 0 {
		t.Fatalf("fresh cache over existing dir: Size = %d, want >0", s)
	}
}

func TestMediaDiskCache_OverwriteAdjustsSize(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	small := make([]byte, 50)
	c.Put(50, 1, small, "a/b")
	s1 := c.Size()

	// Overwrite same key with same body size — size should stay similar.
	c.Put(50, 1, small, "a/b")
	s2 := c.Size()
	if s2 != s1 {
		t.Fatalf("overwrite same size: %d != %d", s2, s1)
	}
}

func BenchmarkMediaDiskCache_EnforceBudget(b *testing.B) {
	dir := b.TempDir()
	c, _ := newMediaDiskCache(dir, time.Hour)
	for i := 0; i < 200; i++ {
		body := make([]byte, 1000)
		c.Put(1000, uint32(i+1), body, "a/b")
	}
	c.SetMaxBytes(c.Size())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.mu.Lock()
		c.enforceBudgetLocked()
		c.mu.Unlock()
	}
}

func TestMediaDiskCache_TTLEvictTracksCurBytes(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 100)
	c.Put(100, 1, body, "a/b")
	sizeBefore := c.Size()

	// Backdate so TTL fires on Get.
	old := time.Now().Add(-time.Hour)
	os.Chtimes(c.keyFile(100, 1), old, old)

	_, _, ok := c.Get(100, 1)
	if ok {
		t.Fatal("expected miss after TTL")
	}
	if s := c.Size(); s >= sizeBefore {
		t.Fatalf("TTL evict didn't decrement curBytes: %d >= %d", s, sizeBefore)
	}
}

func TestMediaDiskCache_HysteresisLowWaterMark(t *testing.T) {
	dir := t.TempDir()
	c, err := newMediaDiskCache(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Put 10 entries with past mtimes; entry i+1 is older for smaller i.
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 10; i++ {
		body := make([]byte, 100)
		if err := c.Put(100, uint32(i+1), body, "a/b"); err != nil {
			t.Fatal(err)
		}
		path := c.keyFile(100, uint32(i+1))
		mtime := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(path, mtime, mtime)
	}

	total := c.Size()
	// Set budget = total - 1 byte: just barely over. Enforce should evict to 90%.
	c.SetMaxBytes(total - 1)
	after := c.Size()
	target := (total - 1) * 9 / 10
	if after > target {
		t.Fatalf("after enforce: Size=%d, target=%d", after, target)
	}

	// Count remaining entries.
	remaining := 0
	for i := 0; i < 10; i++ {
		if _, _, ok := c.Get(100, uint32(i+1)); ok {
			remaining++
		}
	}
	t.Logf("total=%d budget=%d target=%d after=%d remaining=%d/10", total, total-1, target, after, remaining)

	// Verify the latest files survived. Check the last 3 at least.
	for i := 8; i < 10; i++ {
		if _, _, ok := c.Get(100, uint32(i+1)); !ok {
			t.Fatalf("entry %d (newest) should survive hysteresis eviction", i+1)
		}
	}
}

func TestMediaDiskCache_SizePath(t *testing.T) {
	dir := t.TempDir()
	c, _ := newMediaDiskCache(dir, time.Hour)

	// Non-.cache files should not be counted.
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignore me"), 0600)

	body := make([]byte, 50)
	c.Put(50, 1, body, "a/b")
	s := c.Size()

	// Verify only .cache files are counted by creating a subdir.
	os.MkdirAll(filepath.Join(dir, "subdir"), 0700)
	os.WriteFile(filepath.Join(dir, "subdir", "file.cache"), make([]byte, 9999), 0600)

	// Force rescan.
	c.mu.Lock()
	c.curBytes = -1
	c.mu.Unlock()
	s2 := c.Size()
	if s2 != s {
		t.Fatalf("subdir .cache counted: %d vs %d", s2, s)
	}
	// Non-.cache in root dir also not counted.
	fi, _ := os.Stat(filepath.Join(dir, "readme.txt"))
	if fi == nil {
		t.Fatal("readme.txt should exist")
	}
}

func TestMediaDiskCache_RemoveNonexistent(t *testing.T) {
	dir := t.TempDir()
	c, _ := newMediaDiskCache(dir, time.Hour)
	body := make([]byte, 50)
	c.Put(50, 1, body, "a/b")
	before := c.Size()
	c.Remove(999, 999) // doesn't exist
	if c.Size() != before {
		t.Fatalf("Remove of nonexistent changed Size: %d -> %d", before, c.Size())
	}
}

func TestMediaDiskCache_BudgetEvictionOrder(t *testing.T) {
	dir := t.TempDir()
	c, _ := newMediaDiskCache(dir, time.Hour)

	// Files 1-5 with distinct mtimes.
	base := time.Now().Add(-time.Hour)
	for i := 1; i <= 5; i++ {
		body := make([]byte, 100)
		c.Put(100, uint32(i), body, "a/b")
		mtime := base.Add(time.Duration(i) * time.Minute)
		os.Chtimes(c.keyFile(100, uint32(i)), mtime, mtime)
	}

	// Budget for 3 entries: 5 files exceeds it, evict to 90% (2.7 entries) = keep 2.
	perEntry := c.Size() / 5
	c.SetMaxBytes(perEntry * 3)

	// Entries 1,2,3 (oldest) should be gone; 4,5 survive.
	for i := 1; i <= 3; i++ {
		if _, _, ok := c.Get(100, uint32(i)); ok {
			t.Fatalf("entry %d (old) should be evicted", i)
		}
	}
	for i := 4; i <= 5; i++ {
		if _, _, ok := c.Get(100, uint32(i)); !ok {
			t.Fatalf("entry %d (new) should survive", i)
		}
	}
}

func TestMediaDiskCache_PutErrors(t *testing.T) {
	dir := t.TempDir()
	c, _ := newMediaDiskCache(dir, time.Hour)

	if err := c.Put(0, 1, nil, "a/b"); err == nil {
		t.Fatal("Put with size=0 should error")
	}
	if err := c.Put(5, 0, make([]byte, 5), "a/b"); err == nil {
		t.Fatal("Put with crc=0 should error")
	}
	if err := c.Put(5, 1, make([]byte, 3), "a/b"); err == nil {
		t.Fatal("Put with mismatched size should error")
	}
	if s := c.Size(); s != 0 {
		t.Fatalf("failed Puts should not affect size: %d", s)
	}
}

func TestMediaDiskCache_ConcurrentPutGet(t *testing.T) {
	dir := t.TempDir()
	c, _ := newMediaDiskCache(dir, time.Hour)
	c.SetMaxBytes(50000) // plenty of room

	done := make(chan struct{})
	for g := 0; g < 4; g++ {
		go func(id int) {
			for i := 0; i < 50; i++ {
				body := make([]byte, 100)
				crc := uint32(id*1000 + i + 1)
				c.Put(100, crc, body, fmt.Sprintf("g%d", id))
				c.Get(100, crc)
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < 4; g++ {
		<-done
	}
	if s := c.Size(); s < 0 {
		t.Fatalf("negative Size after concurrent ops: %d", s)
	}
}
