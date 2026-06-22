package web

import (
	"bytes"
	"encoding/json"
	"hash/crc32"
	"mime/multipart"
	"net/http/httptest"
	"testing"
)

func multipartBody(t *testing.T, field, filename, mime string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{`form-data; name="` + field + `"; filename="` + filename + `"`}
	if mime != "" {
		hdr["Content-Type"] = []string{mime}
	}
	pw, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	pw.Write(content)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestHandleSavedUploadStoresSealedFile(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	sm, _ := newMediaDiskCache(dir+"/saved-media", 0)
	sm.crypto = sc
	s := &Server{dataDir: dir, savedCrypto: sc, savedMedia: sm}

	content := []byte("PNGDATA-pretend-image-bytes")
	body, ct := multipartBody(t, "file", "pic.png", "image/png", content)
	req := httptest.NewRequest("POST", "/api/saved/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.handleSavedUpload(w, req)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var out savedItemOut
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "file" || out.FileName != "pic.png" || out.MimeType != "image/png" {
		t.Fatalf("unexpected file item: %+v", out)
	}
	if len(out.Media) != 1 || !out.Media[0].Persisted || out.Media[0].Tag != "[IMAGE]" {
		t.Fatalf("unexpected media: %+v", out.Media)
	}
	size := int64(len(content))
	crc := crc32.ChecksumIEEE(content)
	got, mime, ok := sm.Get(size, crc)
	if !ok || !bytes.Equal(got, content) || mime != "image/png" {
		t.Fatalf("blob not retrievable: ok=%v mime=%q", ok, mime)
	}
}

func TestHandleSavedUploadRejectsOverCap(t *testing.T) {
	dir := t.TempDir()
	sc, _ := loadSavedCrypto(dir)
	sm, _ := newMediaDiskCache(dir+"/saved-media", 0)
	sm.crypto = sc
	s := &Server{dataDir: dir, savedCrypto: sc, savedMedia: sm}

	big := bytes.Repeat([]byte{0x41}, savedUploadMaxBytes+1024)
	body, ct := multipartBody(t, "file", "big.bin", "application/octet-stream", big)
	req := httptest.NewRequest("POST", "/api/saved/upload", body)
	req.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.handleSavedUpload(w, req)
	if w.Code != 413 {
		t.Fatalf("over-cap status = %d, want 413", w.Code)
	}
	if s.savedCount() != 0 {
		t.Fatalf("over-cap upload stored an item: count=%d", s.savedCount())
	}
}
