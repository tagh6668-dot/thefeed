package web

import "testing"

func TestPersistSavedMediaFromCache(t *testing.T) {
	dir := t.TempDir()
	mc, _ := newMediaDiskCache(dir+"/cache", 0)
	sm, _ := newMediaDiskCache(dir+"/saved-media", 0)
	s := &Server{mediaCache: mc, savedMedia: sm}

	body := []byte("hello-bytes")
	size := int64(len(body))
	crc := uint32(0xdeadbeef)
	if err := mc.Put(size, crc, body, "image/jpeg"); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := sm.Get(size, crc); ok {
		t.Fatal("expected miss before persist")
	}
	if !s.persistSavedMedia(size, crc) {
		t.Fatal("persistSavedMedia returned false")
	}
	got, mime, ok := sm.Get(size, crc)
	if !ok || string(got) != string(body) || mime != "image/jpeg" {
		t.Fatalf("saved-media miss/mismatch: ok=%v mime=%q", ok, mime)
	}
	if !s.persistSavedMedia(size, crc) {
		t.Fatal("second persist should still report true")
	}
}

func TestPersistSavedMediaCacheMiss(t *testing.T) {
	dir := t.TempDir()
	mc, _ := newMediaDiskCache(dir+"/cache", 0)
	sm, _ := newMediaDiskCache(dir+"/saved-media", 0)
	s := &Server{mediaCache: mc, savedMedia: sm}
	if s.persistSavedMedia(123, 0xabc) {
		t.Fatal("expected false when bytes absent from cache")
	}
}
