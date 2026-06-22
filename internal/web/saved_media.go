package web

import (
	"context"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// persistSavedMedia copies (size, crc) bytes from the ephemeral media cache
// into the never-reaped saved-media store. Returns true if the bytes are now
// in saved-media (including the already-present case), false on cache miss.
func (s *Server) persistSavedMedia(size int64, crc uint32) bool {
	if s.savedMedia == nil || s.mediaCache == nil || size <= 0 || crc == 0 {
		return false
	}
	if _, _, ok := s.savedMedia.Get(size, crc); ok {
		return true // already persisted
	}
	body, mime, ok := s.mediaCache.Get(size, crc)
	if !ok {
		return false
	}
	return s.savedMedia.Put(size, crc, body, mime) == nil
}

// persistTmMedia fetches a TeleMirror image (from cache or upstream) and
// stores it in saved-media. Returns (size, crc, ok).
func (s *Server) persistTmMedia(ctx context.Context, rawURL string) (int64, uint32, bool) {
	if s.savedMedia == nil || s.telemirror == nil || rawURL == "" {
		return 0, 0, false
	}
	body, mime, ok := s.telemirror.imgs.Get(rawURL)
	if !ok {
		var err error
		body, mime, err = s.telemirror.client.FetchURL(ctx, rawURL)
		if err != nil || len(body) == 0 {
			return 0, 0, false
		}
		_ = s.telemirror.imgs.Put(rawURL, mime, body)
	}
	size := int64(len(body))
	crc := crc32.ChecksumIEEE(body)
	if _, _, already := s.savedMedia.Get(size, crc); already {
		return size, crc, true
	}
	if err := s.savedMedia.Put(size, crc, body, mime); err != nil {
		return 0, 0, false
	}
	return size, crc, true
}

// handleSavedMediaPersistTm fetches a TeleMirror image and persists it into
// saved-media, returning (size, crc) for building media descriptors.
func (s *Server) handleSavedMediaPersistTm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !isProxiableHost(req.URL) {
		http.Error(w, "host not allowed", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	size, crc, ok := s.persistTmMedia(ctx, req.URL)
	if !ok {
		http.Error(w, "could not persist image", http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"ok":   true,
		"size": size,
		"crc":  crc,
	})
}

// handleSavedMediaUploadBlob accepts raw image bytes from the browser and
// stores them in saved-media. Used when the server can't reach the image
// origin (e.g. translate.goog) but the browser can.
func (s *Server) handleSavedMediaUploadBlob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.savedMedia == nil {
		http.Error(w, "saved media unavailable", http.StatusInternalServerError)
		return
	}
	const maxBlobBytes = 10 << 20 // 10 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxBlobBytes+1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read failed", http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}
	mime := r.Header.Get("Content-Type")
	if mime == "" {
		mime = http.DetectContentType(body)
	}
	mime = sanitizeMime(mime)
	if !strings.HasPrefix(mime, "image/") {
		http.Error(w, "not an image", http.StatusBadRequest)
		return
	}
	size := int64(len(body))
	crc := crc32.ChecksumIEEE(body)
	if _, _, already := s.savedMedia.Get(size, crc); already {
		writeJSON(w, map[string]any{"ok": true, "size": size, "crc": crc})
		return
	}
	if err := s.savedMedia.Put(size, crc, body, mime); err != nil {
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "size": size, "crc": crc})
}

// handleSavedMedia serves persisted media by ?size=&crc=, falling back to the
// ephemeral cache if the bytes happen to still be there.
func (s *Server) handleSavedMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	size, _ := strconv.ParseInt(q.Get("size"), 10, 64)
	crcU, err := strconv.ParseUint(strings.TrimSpace(q.Get("crc")), 16, 32)
	if err != nil || size <= 0 {
		http.Error(w, "bad size/crc", http.StatusBadRequest)
		return
	}
	crc := uint32(crcU)
	var body []byte
	var mime string
	var ok bool
	if s.savedMedia != nil {
		body, mime, ok = s.savedMedia.Get(size, crc)
	}
	if !ok && s.mediaCache != nil {
		body, mime, ok = s.mediaCache.Get(size, crc)
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if mime == "" {
		mime = http.DetectContentType(body)
	}
	w.Header().Set("Content-Type", sanitizeMime(mime))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// Defence in depth: never let a browser MIME-sniff this user-controlled blob,
	// and force download-style handling rather than inline rendering.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	_, _ = w.Write(body)
}
