package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// SavedMedia is one media item referenced by a saved message. Bytes live in
// the saved-media store, content-addressed by (Size, CRC). Persisted is true
// once the bytes have been copied into saved-media/.
type SavedMedia struct {
	Tag       string `json:"tag"`
	Size      int64  `json:"size"`
	CRC       uint32 `json:"crc"`
	Persisted bool   `json:"persisted"`
	Fname     string `json:"fname,omitempty"`
}

// SavedItem is one bookmarked message, scoped to a config's domain (the stable
// identity of a config, unlike the random profile ID).
type SavedItem struct {
	ID string `json:"id"` // <domain>__<channelId>__<messageId> for bookmarks
	// Kind discriminates the self-chat timeline: "bookmark" (Telemirror channel
	// post), "note" (typed to self), "file" (uploaded), "chat" (forwarded from
	// the E2E messenger). Empty in legacy records -> backfilled to "bookmark".
	Kind       string       `json:"kind"`
	Domain     string       `json:"domain"`
	Nickname   string       `json:"nickname"` // snapshot of the config (or contact) name at save time
	ChannelID  string       `json:"channelId"`
	ChannelNum int          `json:"channelNum"`
	MessageID  uint32       `json:"messageId"`
	Text       string       `json:"text"`
	Timestamp  uint32       `json:"timestamp"`
	SavedAt    int64        `json:"savedAt"` // unix millis
	Media      []SavedMedia `json:"media"`
	FileName   string       `json:"fileName,omitempty"` // kind:"file"
	MimeType   string       `json:"mimeType,omitempty"` // kind:"file"
	Pinned     bool         `json:"pinned,omitempty"`
	// TmSource locates the originating Telemirror post for items saved from the
	// Telemirror browser ("<channel>/<msgNum>", e.g. "VahidOnline/12345"). When
	// set, the UI shows a "jump to post" action that reopens that channel.
	TmSource string `json:"tmSource,omitempty"`
}

type savedStore struct {
	Items []SavedItem `json:"items"`
	// SeenAt is the savedAt (unix millis) of the newest item the user had seen
	// the last time they opened the (global) Saved view. Anything newer is
	// "unseen" and lights the sidebar dot.
	SeenAt int64 `json:"seenAt,omitempty"`
}

func (s *Server) savedPath() string {
	return filepath.Join(s.dataDir, "saved.json")
}

// loadSaved reads saved.json, decrypting if sealed. Legacy plaintext stores
// (and pre-encryption records) are migrated: missing Kind -> "bookmark", then
// the whole store is re-written sealed. Bookmark records without a Domain are
// the old per-profile schema and are dropped (start fresh).
func (s *Server) loadSaved() *savedStore {
	st := &savedStore{Items: []SavedItem{}}
	data, err := os.ReadFile(s.savedPath())
	if err != nil {
		return st
	}
	plain := data
	migrated := false
	if s.savedCrypto != nil && !s.savedCrypto.locked {
		if opened, oerr := openBytes(s.savedCrypto.dek, data); oerr == nil {
			plain = opened
		} else {
			// Not sealed (or unreadable) — treat as legacy plaintext and re-seal.
			migrated = true
		}
	}
	if err := json.Unmarshal(plain, st); err != nil {
		return &savedStore{Items: []SavedItem{}}
	}
	kept := st.Items[:0]
	for _, it := range st.Items {
		if it.Kind == "" {
			it.Kind = "bookmark"
		}
		if it.Kind == "bookmark" && it.Domain == "" {
			continue // legacy per-profile record
		}
		kept = append(kept, it)
	}
	st.Items = kept
	if st.Items == nil {
		st.Items = []SavedItem{}
	}
	if migrated && s.savedCrypto != nil && !s.savedCrypto.locked {
		_ = s.writeSaved(st) // re-seal in place
	}
	return st
}

// savedHasUnseen reports whether any saved item is newer than the last time the
// Saved view was opened (drives the sidebar "new" dot).
func (s *Server) savedHasUnseen() bool {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	for _, it := range st.Items {
		if it.SavedAt > st.SeenAt {
			return true
		}
	}
	return false
}

// savedCountAndUnseen returns (count, unseen) from a single store load.
func (s *Server) savedCountAndUnseen() (int, bool) {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	unseen := false
	for _, it := range st.Items {
		if it.SavedAt > st.SeenAt {
			unseen = true
			break
		}
	}
	return len(st.Items), unseen
}

// savedMarkSeen sets SeenAt to the newest item's savedAt, clearing the dot.
func (s *Server) savedMarkSeen() error {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	// A locked store can't be written; opening the view while locked is a no-op
	// for the seen marker (avoids a 500 on every locked open).
	if s.savedCrypto != nil && s.savedCrypto.locked {
		return nil
	}
	st := s.loadSaved()
	var newest int64
	for _, it := range st.Items {
		if it.SavedAt > newest {
			newest = it.SavedAt
		}
	}
	st.SeenAt = newest
	return s.writeSaved(st)
}

// writeSaved persists the store atomically (temp file + rename).
func (s *Server) writeSaved(st *savedStore) error {
	if err := os.MkdirAll(s.dataDir, 0700); err != nil {
		return err
	}
	// Refuse to write while locked: writing plaintext here would clobber the
	// sealed store on disk (silent data loss). Callers must unlock first.
	if s.savedCrypto != nil && s.savedCrypto.locked {
		return errSavedLocked
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if s.savedCrypto != nil {
		data = sealBytes(s.savedCrypto.dek, data)
	}
	path := s.savedPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// savedUpsert inserts or replaces a record by ID, then persists.
func (s *Server) savedUpsert(item SavedItem) error {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	replaced := false
	for i := range st.Items {
		if st.Items[i].ID == item.ID {
			st.Items[i] = item
			replaced = true
			break
		}
	}
	if !replaced {
		st.Items = append(st.Items, item)
	}
	return s.writeSaved(st)
}

// savedList returns ALL saved records sorted by SavedAt ascending (newest last).
func (s *Server) savedList() []SavedItem {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	out := append([]SavedItem{}, st.Items...)
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt < out[j].SavedAt })
	return out
}

func (s *Server) savedCount() int {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	return len(s.loadSaved().Items)
}

// savedDeleteAndCleanup removes a record by ID and deletes any saved-media
// bytes it referenced that no remaining record still references. The whole
// delete + orphan-check + file-removal runs under savedMu so a concurrent
// save cannot leave a record flagged Persisted with its bytes deleted.
// (mediaDiskCache.Remove takes its own lock, so there is no deadlock.)
func (s *Server) savedDeleteAndCleanup(id string) (*SavedItem, error) {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	var removed *SavedItem
	kept := st.Items[:0]
	for _, it := range st.Items {
		if it.ID == id {
			c := it
			removed = &c
			continue
		}
		kept = append(kept, it)
	}
	if removed == nil {
		return nil, nil
	}
	st.Items = kept
	if err := s.writeSaved(st); err != nil {
		return nil, err
	}
	if s.savedMedia != nil {
		for _, m := range removed.Media {
			referenced := false
			for _, it := range st.Items {
				for _, mm := range it.Media {
					if mm.Size == m.Size && mm.CRC == m.CRC {
						referenced = true
						break
					}
				}
				if referenced {
					break
				}
			}
			if !referenced {
				s.savedMedia.Remove(m.Size, m.CRC)
			}
		}
	}
	return removed, nil
}

// savedSetPinned sets the Pinned flag on a record by ID, then persists.
func (s *Server) savedSetPinned(id string, pinned bool) error {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	found := false
	for i := range st.Items {
		if st.Items[i].ID == id {
			st.Items[i].Pinned = pinned
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	return s.writeSaved(st)
}

// savedSetPersisted marks (size, crc) as persisted across all records that
// reference it, then persists the store.
func (s *Server) savedSetPersisted(size int64, crc uint32) error {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	changed := false
	for i := range st.Items {
		for j := range st.Items[i].Media {
			if st.Items[i].Media[j].Size == size && st.Items[i].Media[j].CRC == crc {
				st.Items[i].Media[j].Persisted = true
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return s.writeSaved(st)
}

// savedFindChat returns the ID of an existing kind:"chat" item with identical
// text and contact, or "" if none exists. Used for toggle dedup.
func (s *Server) savedFindChat(text, contact string) string {
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	st := s.loadSaved()
	for _, it := range st.Items {
		if it.Kind == "chat" && it.Text == text && it.Nickname == contact {
			return it.ID
		}
	}
	return ""
}

// mediaTagRe matches the downloadable-media wire format embedded in message
// text: [TAG]size:relayflags:ch:blk:crc[:filename]. Captures tag, size, crc.
var mediaTagRe = regexp.MustCompile(`\[(IMAGE|VIDEO|FILE|AUDIO|STICKER|GIF|CONTACT|LOCATION)\](\d+):[0-9,]+:\d+:\d+:([0-9a-fA-F]+)(?::([^\n]*))?`)

// parseSavedMedia extracts media references from a message's text.
func parseSavedMedia(text string) []SavedMedia {
	matches := mediaTagRe.FindAllStringSubmatch(text, -1)
	out := []SavedMedia{}
	for _, m := range matches {
		size, err1 := strconv.ParseInt(m[2], 10, 64)
		crc, err2 := strconv.ParseUint(m[3], 16, 32)
		if err1 != nil || err2 != nil || size <= 0 || crc == 0 {
			continue
		}
		out = append(out, SavedMedia{Tag: "[" + m[1] + "]", Size: size, CRC: uint32(crc), Fname: m[4]})
	}
	return out
}
