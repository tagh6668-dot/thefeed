package web

import (
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"time"
)

// savedUploadMaxBytes caps a single uploaded file. 50 MiB matches the design.
const savedUploadMaxBytes = 50 << 20

// mediaTagForMime picks the wire-style media tag the timeline renderer keys on.
func mediaTagForMime(mime string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "[IMAGE]"
	case strings.HasPrefix(mime, "video/"):
		return "[VIDEO]"
	case strings.HasPrefix(mime, "audio/"):
		return "[AUDIO]"
	default:
		return "[FILE]"
	}
}

// handleSavedUpload accepts one multipart file part and stores it as a
// kind:"file" item, with the bytes sealed in the content-addressed saved-media
// store. The existing GET /api/saved/media?size=&crc= serves it back.
func (s *Server) handleSavedUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.savedLocked() {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	if s.savedMedia == nil {
		http.Error(w, "saved media unavailable", http.StatusInternalServerError)
		return
	}
	// Hard cap the whole request body, then parse entirely in RAM so Go never
	// spills to a temp file — mobile WebViews may lack a writable tmpdir.
	r.Body = http.MaxBytesReader(w, r.Body, savedUploadMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(savedUploadMaxBytes + (1 << 20)); err != nil {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	body, err := io.ReadAll(io.LimitReader(file, savedUploadMaxBytes+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty file", http.StatusBadRequest)
		return
	}
	if len(body) > savedUploadMaxBytes {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}

	mime := hdr.Header.Get("Content-Type")
	if mime == "" {
		mime = http.DetectContentType(body)
	}
	// Sanitize at store time, not just on serve, so a hostile Content-Type never
	// reaches disk or the timeline renderer.
	mime = sanitizeMime(mime)
	size := int64(len(body))
	crc := crc32.ChecksumIEEE(body)
	if err := s.savedMedia.Put(size, crc, body, mime); err != nil {
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}

	item := SavedItem{
		ID:       "file__" + generateID(),
		Kind:     "file",
		Text:     "",
		FileName: sanitizeFileName(hdr.Filename),
		MimeType: mime,
		SavedAt:  time.Now().UnixMilli(),
		Media:    []SavedMedia{{Tag: mediaTagForMime(mime), Size: size, CRC: crc, Persisted: true}},
	}
	if err := s.savedUpsert(item); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.enrichSaved(item, ""))
}

// sanitizeFileName strips path separators and control chars from a client
// filename so it's safe to echo back into the UI.
func sanitizeFileName(name string) string {
	name = name[strings.LastIndexAny(name, "/\\")+1:]
	name = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, name)
	if len(name) > 200 {
		name = name[:200]
	}
	return strings.TrimSpace(name)
}
