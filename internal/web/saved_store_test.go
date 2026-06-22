package web

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestSavedStoreSealedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}
	if err := s.savedUpsert(SavedItem{ID: "d__c__1", Kind: "bookmark", Domain: "d", SavedAt: 1}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.savedPath())
	var probe savedStore
	if json.Unmarshal(raw, &probe) == nil {
		t.Fatal("saved.json is plaintext JSON; expected sealed bytes")
	}
	if s.savedCount() != 1 || s.savedList()[0].Kind != "bookmark" {
		t.Fatalf("sealed store did not round-trip: %+v", s.savedList())
	}
}

func TestSavedStoreMigratesLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}
	legacy, _ := json.MarshalIndent(&savedStore{Items: []SavedItem{{ID: "d__c__1", Domain: "d", ChannelID: "c", MessageID: 1, SavedAt: 1}}}, "", "  ")
	_ = os.WriteFile(s.savedPath(), legacy, 0o600)

	list := s.savedList()
	if len(list) != 1 || list[0].Kind != "bookmark" {
		t.Fatalf("legacy migration failed: %+v", list)
	}
	raw, _ := os.ReadFile(s.savedPath())
	var probe savedStore
	if json.Unmarshal(raw, &probe) == nil {
		t.Fatal("store not re-sealed after migration")
	}
}

// TestSavedMediaMigratesLegacyPlaintext covers the upgrade path: a blob written
// by the pre-encryption feature is plaintext on disk. After attaching crypto,
// Get must still return it (fallback) and re-seal it in place.
func TestSavedMediaMigratesLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)

	// Write a legacy PLAINTEXT blob via a crypto-less cache (as the old code did).
	plain, _ := newMediaDiskCache(dir+"/sm", 0)
	size := int64(5)
	crc := uint32(0x11223344)
	if err := plain.Put(size, crc, []byte("hello"), "image/png"); err != nil {
		t.Fatal(err)
	}
	rawBefore, _ := os.ReadFile(plain.keyFile(size, crc))

	// Now open the same dir WITH crypto attached (post-upgrade).
	enc, _ := newMediaDiskCache(dir+"/sm", 0)
	enc.crypto = sc
	body, mime, ok := enc.Get(size, crc)
	if !ok || string(body) != "hello" || mime != "image/png" {
		t.Fatalf("legacy plaintext blob not served: %q %q %v", body, mime, ok)
	}
	// Re-sealed in place: on-disk bytes changed and no longer contain plaintext.
	rawAfter, _ := os.ReadFile(enc.keyFile(size, crc))
	if bytes.Equal(rawBefore, rawAfter) || bytes.Contains(rawAfter, []byte("hello")) {
		t.Fatal("legacy blob was not re-sealed on access")
	}
	// And still readable after re-seal.
	if body2, _, ok2 := enc.Get(size, crc); !ok2 || string(body2) != "hello" {
		t.Fatalf("re-sealed blob unreadable: %q %v", body2, ok2)
	}
}

// TestSavedWriteRefusedWhenLocked guards against silent data loss: a locked
// store must reject writes instead of clobbering the sealed file with plaintext.
func TestSavedWriteRefusedWhenLocked(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}
	if err := s.savedUpsert(SavedItem{ID: "d__c__1", Kind: "bookmark", Domain: "d", SavedAt: 1}); err != nil {
		t.Fatal(err)
	}
	sealedBefore, _ := os.ReadFile(s.savedPath())

	// Simulate a lost device key / passphrase-locked store.
	sc.locked = true
	if err := s.savedUpsert(SavedItem{ID: "d__c__2", Kind: "bookmark", Domain: "d", SavedAt: 2}); err != errSavedLocked {
		t.Fatalf("locked upsert err = %v, want errSavedLocked", err)
	}
	sealedAfter, _ := os.ReadFile(s.savedPath())
	if !bytes.Equal(sealedBefore, sealedAfter) {
		t.Fatal("locked write clobbered the sealed store on disk")
	}
}

func TestSavedSetPinned(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}
	_ = s.savedUpsert(SavedItem{ID: "note__1", Kind: "note", Text: "x", SavedAt: 1})
	if err := s.savedSetPinned("note__1", true); err != nil {
		t.Fatal(err)
	}
	if !s.savedList()[0].Pinned {
		t.Fatal("item not pinned")
	}
	if err := s.savedSetPinned("note__1", false); err != nil {
		t.Fatal(err)
	}
	if s.savedList()[0].Pinned {
		t.Fatal("item still pinned after unpin")
	}
}

func TestSavedMediaSealedAtRest(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	c, _ := newMediaDiskCache(dir+"/sm", 0)
	c.crypto = sc
	size := int64(5)
	crc := uint32(0xabcdef01)
	if err := c.Put(size, crc, []byte("hello"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(c.keyFile(size, crc))
	if bytes.Contains(raw, []byte("hello")) {
		t.Fatal("media stored in plaintext")
	}
	body, mime, ok := c.Get(size, crc)
	if !ok || string(body) != "hello" || mime != "text/plain" {
		t.Fatalf("sealed media did not round-trip: %q %q %v", body, mime, ok)
	}
}

func TestParseSavedMedia(t *testing.T) {
	text := "[IMAGE]84213:1,0:12000:3:1234abcd\nhello caption"
	got := parseSavedMedia(text)
	want := []SavedMedia{{Tag: "[IMAGE]", Size: 84213, CRC: 0x1234abcd, Persisted: false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSavedMedia = %+v, want %+v", got, want)
	}
}

func TestParseSavedMediaFilename(t *testing.T) {
	text := "[FILE]1024:0:5:1:abcd1234:report.pdf\ncaption"
	got := parseSavedMedia(text)
	want := []SavedMedia{{Tag: "[FILE]", Size: 1024, CRC: 0xabcd1234, Fname: "report.pdf"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSavedMedia = %+v, want %+v", got, want)
	}
}

func TestParseSavedMedia_None(t *testing.T) {
	if got := parseSavedMedia("just text, no media"); len(got) != 0 {
		t.Fatalf("expected no media, got %+v", got)
	}
}

func TestSavedStoreUpsertAndList(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}

	a := SavedItem{ID: "d__c__1", Domain: "d", ChannelID: "c", MessageID: 1, SavedAt: 100}
	b := SavedItem{ID: "d__c__2", Domain: "d", ChannelID: "c", MessageID: 2, SavedAt: 200}
	e := SavedItem{ID: "e__c__9", Domain: "e", ChannelID: "c", MessageID: 9, SavedAt: 50}
	for _, it := range []SavedItem{b, a, e} {
		if err := s.savedUpsert(it); err != nil {
			t.Fatal(err)
		}
	}

	// Global list, sorted by SavedAt ascending across ALL domains.
	list := s.savedList()
	if len(list) != 3 || list[0].ID != "e__c__9" || list[2].ID != "d__c__2" {
		t.Fatalf("savedList wrong order/content: %+v", list)
	}
	if s.savedCount() != 3 {
		t.Fatalf("count = %d, want 3", s.savedCount())
	}

	// Upsert is idempotent on ID.
	a2 := a
	a2.Text = "updated"
	if err := s.savedUpsert(a2); err != nil {
		t.Fatal(err)
	}
	if s.savedCount() != 3 {
		t.Fatalf("upsert duplicated record: count=%d", s.savedCount())
	}
}

func TestSavedStoreDelete(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	_ = s.savedUpsert(SavedItem{ID: "d__c__1", Domain: "d", SavedAt: 1})
	removed, err := s.savedDeleteAndCleanup("d__c__1")
	if err != nil {
		t.Fatal(err)
	}
	if removed == nil || removed.ID != "d__c__1" {
		t.Fatalf("savedDeleteAndCleanup returned %+v", removed)
	}
	if s.savedCount() != 0 {
		t.Fatalf("record not deleted: count=%d", s.savedCount())
	}
}

func TestSavedSeenGlobal(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	_ = s.savedUpsert(SavedItem{ID: "d__c__1", Domain: "d", SavedAt: 100})
	if !s.savedHasUnseen() {
		t.Fatal("expected unseen before markSeen")
	}
	if err := s.savedMarkSeen(); err != nil {
		t.Fatal(err)
	}
	if s.savedHasUnseen() {
		t.Fatal("expected no unseen after markSeen")
	}
	// A newer save makes it unseen again.
	_ = s.savedUpsert(SavedItem{ID: "d__c__2", Domain: "d", SavedAt: 200})
	if !s.savedHasUnseen() {
		t.Fatal("expected unseen after a newer save")
	}
}

func TestLoadSavedDropsOldSchema(t *testing.T) {
	dir := t.TempDir()
	s := &Server{dataDir: dir}
	// Write a legacy record (no domain) directly.
	_ = s.writeSaved(&savedStore{Items: []SavedItem{{ID: "p__c__1", ChannelID: "c", MessageID: 1, SavedAt: 1}}})
	if s.savedCount() != 0 {
		t.Fatalf("legacy (domain-less) record should be dropped, count=%d", s.savedCount())
	}
}
