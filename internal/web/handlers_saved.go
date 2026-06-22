package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleSaved dispatches GET (list), POST (save), DELETE (unsave).
func (s *Server) handleSaved(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleSavedList(w, r)
	case http.MethodPost:
		s.handleSavedCreate(w, r)
	case http.MethodDelete:
		s.handleSavedRemove(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSavedList(w http.ResponseWriter, r *http.Request) {
	if s.savedLocked() {
		w.WriteHeader(http.StatusLocked)
		writeJSON(w, map[string]any{"locked": true, "items": []savedItemOut{}})
		return
	}
	activeDomain := ""
	if p := s.activeProfile(); p != nil {
		activeDomain = p.Config.Domain
	}
	items := s.savedList()
	out := make([]savedItemOut, 0, len(items))
	for _, it := range items {
		out = append(out, s.enrichSaved(it, activeDomain))
	}
	writeJSON(w, map[string]any{"items": out})
}

func (s *Server) handleSavedCount(w http.ResponseWriter, r *http.Request) {
	count, unseen := s.savedCountAndUnseen()
	writeJSON(w, map[string]any{
		"count":  count,
		"unseen": unseen,
	})
}

// handleSavedSeen marks the global Saved view as seen (clears the dot).
func (s *Server) handleSavedSeen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.savedMarkSeen(); err != nil {
		http.Error(w, "seen failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSavedCreate(w http.ResponseWriter, r *http.Request) {
	if s.savedLocked() {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	var req struct {
		ChannelNum int    `json:"channelNum"`
		MessageID  uint32 `json:"messageId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChannelNum < 1 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	prof := s.activeProfile()
	if prof == nil || prof.Config.Domain == "" {
		http.Error(w, "no active config", http.StatusBadRequest)
		return
	}
	domain := prof.Config.Domain

	s.mu.RLock()
	chs := s.channels
	inMem := s.messages[req.ChannelNum]
	cache := s.cache
	s.mu.RUnlock()
	if req.ChannelNum > len(chs) {
		http.Error(w, "unknown channel", http.StatusBadRequest)
		return
	}
	channelName := chs[req.ChannelNum-1].Name

	text, ts, found := "", uint32(0), false
	if cache != nil {
		if res := cache.GetMessages(channelName); res != nil {
			for _, m := range res.Messages {
				if m.ID == req.MessageID {
					text, ts, found = m.Text, m.Timestamp, true
					break
				}
			}
		}
	}
	if !found {
		for _, m := range inMem {
			if m.ID == req.MessageID {
				text, ts, found = m.Text, m.Timestamp, true
				break
			}
		}
	}
	if !found {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}

	item := SavedItem{
		ID:         domain + "__" + channelName + "__" + strconv.FormatUint(uint64(req.MessageID), 10),
		Domain:     domain,
		Nickname:   prof.Nickname,
		ChannelID:  channelName,
		ChannelNum: req.ChannelNum,
		MessageID:  req.MessageID,
		Text:       text,
		Timestamp:  ts,
		SavedAt:    time.Now().UnixMilli(),
		Media:      parseSavedMedia(text),
	}
	// Track which blobs WE newly copied into saved-media (ttl=0, never reaped) so
	// a failed upsert below doesn't leave them orphaned on disk.
	var newlyPersisted []SavedMedia
	for i := range item.Media {
		m := &item.Media[i]
		already := false
		if s.savedMedia != nil {
			_, _, already = s.savedMedia.Get(m.Size, m.CRC)
		}
		if s.persistSavedMedia(m.Size, m.CRC) {
			m.Persisted = true
			if !already {
				newlyPersisted = append(newlyPersisted, *m)
			}
		}
	}
	if err := s.savedUpsert(item); err != nil {
		if s.savedMedia != nil {
			for _, m := range newlyPersisted {
				s.savedMedia.Remove(m.Size, m.CRC)
			}
		}
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.enrichSaved(item, domain))
}

// savedNoteMaxBytes caps a single typed note (generous for drafts).
const savedNoteMaxBytes = 16 << 10

// handleSavedNote creates a kind:"note" item from text the user typed to self.
func (s *Server) handleSavedNote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.savedLocked() {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len(text) > savedNoteMaxBytes {
		http.Error(w, "bad note text", http.StatusBadRequest)
		return
	}
	item := SavedItem{
		ID:      "note__" + generateID(),
		Kind:    "note",
		Text:    text,
		SavedAt: time.Now().UnixMilli(),
		Media:   parseSavedMedia(text),
	}
	if err := s.savedUpsert(item); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.enrichSaved(item, ""))
}

// handleSavedFromChat snapshots an (already-decrypted) E2E messenger message
// into Saved as a kind:"chat" item. The JS passes the rendered text, the
// contact name, and any media descriptors it had; media already in the cache is
// persisted into saved-media. Read-only: deleting it never touches the chat.
func (s *Server) handleSavedFromChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.savedLocked() {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	var req struct {
		Text        string       `json:"text"`
		ContactName string       `json:"contactName"`
		Media       []SavedMedia `json:"media"`
		TmSource    string       `json:"tmSource"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" && len(req.Media) == 0 {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	if len(text) > savedNoteMaxBytes {
		text = text[:savedNoteMaxBytes]
	}
	// Dedup: if an identical chat message already exists, remove it (toggle)
	if existing := s.savedFindChat(text, strings.TrimSpace(req.ContactName)); existing != "" {
		if _, err := s.savedDeleteAndCleanup(existing); err != nil {
			http.Error(w, "delete failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "toggled": "removed", "id": existing})
		return
	}
	media := req.Media
	for i := range media {
		if media[i].Size > 0 && media[i].CRC != 0 && s.persistSavedMedia(media[i].Size, media[i].CRC) {
			media[i].Persisted = true
		}
	}
	item := SavedItem{
		ID:       "chat__" + generateID(),
		Kind:     "chat",
		Text:     text,
		Nickname: strings.TrimSpace(req.ContactName),
		SavedAt:  time.Now().UnixMilli(),
		Media:    media,
		TmSource: strings.TrimSpace(req.TmSource),
	}
	if err := s.savedUpsert(item); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, s.enrichSaved(item, ""))
}

// handleSavedPin sets/clears the pinned flag on a saved item.
func (s *Server) handleSavedPin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.savedLocked() {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	var req struct {
		ID     string `json:"id"`
		Pinned bool   `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.savedSetPinned(req.ID, req.Pinned); err != nil {
		http.Error(w, "pin failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "pinned": req.Pinned})
}

func (s *Server) handleSavedRemove(w http.ResponseWriter, r *http.Request) {
	if s.savedLocked() {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if _, err := s.savedDeleteAndCleanup(req.ID); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleSavedMediaPersist copies already-downloaded media (size, crc) from the
// ephemeral cache into saved-media and marks matching records persisted.
func (s *Server) handleSavedMediaPersist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Size int64  `json:"size"`
		CRC  uint32 `json:"crc"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Size <= 0 || req.CRC == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.persistSavedMedia(req.Size, req.CRC) {
		http.Error(w, "media not in cache", http.StatusNotFound)
		return
	}
	if err := s.savedSetPersisted(req.Size, req.CRC); err != nil {
		http.Error(w, "persist flag failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "persisted": true})
}
