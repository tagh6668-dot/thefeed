package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleSavedNoteCreatesItem(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}

	req := httptest.NewRequest("POST", "/api/saved/note", strings.NewReader(`{"text":"  buy milk  "}`))
	w := httptest.NewRecorder()
	s.handleSavedNote(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var out savedItemOut
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "note" || out.Text != "buy milk" || out.Domain != "" || out.SavedAt == 0 {
		t.Fatalf("unexpected note item: %+v", out)
	}
	if !out.Available || out.IsActive {
		t.Fatalf("note should be available and not config-active: %+v", out)
	}
	if s.savedCount() != 1 {
		t.Fatalf("count = %d, want 1", s.savedCount())
	}
}

func TestSavedLockUnlockFlow(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}

	// Save a note, then set a passphrase.
	noteReq := httptest.NewRequest("POST", "/api/saved/note", strings.NewReader(`{"text":"secret"}`))
	s.handleSavedNote(httptest.NewRecorder(), noteReq)
	lockReq := httptest.NewRequest("POST", "/api/saved/lock", strings.NewReader(`{"passphrase":"pw"}`))
	lw := httptest.NewRecorder()
	s.handleSavedLock(lw, lockReq)
	if lw.Code != 200 {
		t.Fatalf("set passphrase = %d", lw.Code)
	}

	// Simulate a restart: reload crypto -> locked.
	s.savedCrypto, _ = loadSavedCrypto(dir)
	if !s.savedCrypto.locked {
		t.Fatal("store should be locked after reload")
	}
	lr := httptest.NewRecorder()
	s.handleSavedList(lr, httptest.NewRequest("GET", "/api/saved", nil))
	if lr.Code != http.StatusLocked {
		t.Fatalf("locked list = %d, want 423", lr.Code)
	}

	// Wrong then right passphrase.
	bw := httptest.NewRecorder()
	s.handleSavedUnlock(bw, httptest.NewRequest("POST", "/api/saved/unlock", strings.NewReader(`{"passphrase":"nope"}`)))
	if bw.Code != http.StatusUnauthorized {
		t.Fatalf("wrong unlock = %d, want 401", bw.Code)
	}
	gw := httptest.NewRecorder()
	s.handleSavedUnlock(gw, httptest.NewRequest("POST", "/api/saved/unlock", strings.NewReader(`{"passphrase":"pw"}`)))
	if gw.Code != 200 || s.savedCrypto.locked {
		t.Fatalf("unlock failed: code=%d locked=%v", gw.Code, s.savedCrypto.locked)
	}
	if s.savedList()[0].Text != "secret" {
		t.Fatal("data not readable after unlock")
	}
}

func TestHandleSavedFromChatSnapshot(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}

	req := httptest.NewRequest("POST", "/api/saved/from-chat",
		strings.NewReader(`{"text":"see you at 8","contactName":"Sara"}`))
	w := httptest.NewRecorder()
	s.handleSavedFromChat(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var out savedItemOut
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "chat" || out.Text != "see you at 8" || out.Nickname != "Sara" || out.Domain != "" {
		t.Fatalf("unexpected chat item: %+v", out)
	}
	if out.ConfigLabel != "Sara" || !out.Available {
		t.Fatalf("chat item should label by contact and be available: %+v", out)
	}
	// Deleting the snapshot is a pure store op (no chat hub involved).
	if removed, err := s.savedDeleteAndCleanup(out.ID); err != nil || removed == nil {
		t.Fatalf("delete failed: %v %+v", err, removed)
	}
	if s.savedCount() != 0 {
		t.Fatalf("count after delete = %d, want 0", s.savedCount())
	}
}

func TestResetThenUploadWorks(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	sm, _ := newMediaDiskCache(dir+"/saved-media", 0)
	sm.crypto = sc
	s := &Server{dataDir: dir, savedCrypto: sc, savedMedia: sm}

	// Upload a file first.
	content := []byte("test-data-1234")
	body, ct := multipartBody(t, "file", "a.bin", "application/octet-stream", content)
	req := httptest.NewRequest("POST", "/api/saved/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.handleSavedUpload(w, req)
	if w.Code != 200 {
		t.Fatalf("initial upload status = %d", w.Code)
	}
	if sm.Size() == 0 {
		t.Fatal("media size should be >0 after upload")
	}

	// Reset (simulates the forgotten-passphrase escape hatch).
	rw := httptest.NewRecorder()
	s.handleSavedLockReset(rw, httptest.NewRequest("POST", "/api/saved/lock/reset", nil))
	if rw.Code != 200 {
		t.Fatalf("reset status = %d", rw.Code)
	}
	if sm.Size() != 0 {
		t.Fatalf("media size after reset = %d, want 0", sm.Size())
	}
	if s.savedCount() != 0 {
		t.Fatalf("saved count after reset = %d, want 0", s.savedCount())
	}

	// Upload again — this must succeed (directory was recreated).
	content2 := []byte("new-file-after-reset")
	body2, ct2 := multipartBody(t, "file", "b.png", "image/png", content2)
	req2 := httptest.NewRequest("POST", "/api/saved/upload", body2)
	req2.Header.Set("Content-Type", ct2)
	w2 := httptest.NewRecorder()
	s.handleSavedUpload(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("post-reset upload status = %d (%s)", w2.Code, w2.Body.String())
	}
	if s.savedCount() != 1 {
		t.Fatalf("saved count after post-reset upload = %d, want 1", s.savedCount())
	}
	if sm.Size() == 0 {
		t.Fatal("media size should be >0 after post-reset upload")
	}
}

func TestDeviceKeyLostShowsLocked(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}

	// Save something.
	req := httptest.NewRequest("POST", "/api/saved/note", strings.NewReader(`{"text":"important"}`))
	s.handleSavedNote(httptest.NewRecorder(), req)

	// Corrupt the device key (truncate to 0 bytes).
	_ = writeFileAtomic(sc.deviceKeyPath(), []byte{}, 0o600)

	// Reload — should be locked.
	s.savedCrypto, _ = loadSavedCrypto(dir)
	if !s.savedCrypto.locked {
		t.Fatal("store should be locked with corrupt device key")
	}
	if s.savedCrypto.mode != "device" {
		t.Fatalf("mode should be device, got %s", s.savedCrypto.mode)
	}

	// List should return 423.
	w := httptest.NewRecorder()
	s.handleSavedList(w, httptest.NewRequest("GET", "/api/saved", nil))
	if w.Code != http.StatusLocked {
		t.Fatalf("locked list = %d, want 423", w.Code)
	}

	// Lock state endpoint should report mode=device, locked=true.
	sw := httptest.NewRecorder()
	s.handleSavedLock(sw, httptest.NewRequest("GET", "/api/saved/lock", nil))
	var state struct {
		Mode   string `json:"mode"`
		Locked bool   `json:"locked"`
	}
	if err := json.Unmarshal(sw.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Mode != "device" || !state.Locked {
		t.Fatalf("lock state: %+v", state)
	}
}

func TestHandleSavedNoteRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	s := &Server{dataDir: dir, savedCrypto: sc}
	req := httptest.NewRequest("POST", "/api/saved/note", strings.NewReader(`{"text":"   "}`))
	w := httptest.NewRecorder()
	s.handleSavedNote(w, req)
	if w.Code != 400 {
		t.Fatalf("empty note status = %d, want 400", w.Code)
	}
	if s.savedCount() != 0 {
		t.Fatalf("empty note was stored: count=%d", s.savedCount())
	}
}
