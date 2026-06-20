package web

import (
	"encoding/json"
	"net/http"
)

// savedLocked reports whether the Saved store is currently passphrase-locked.
// Read under savedMu.RLock so it doesn't race the pointer swap in
// handleSavedUnlock. Must NOT be called while already holding savedMu (RWMutex
// is not reentrant).
func (s *Server) savedLocked() bool {
	s.savedMu.RLock()
	defer s.savedMu.RUnlock()
	return s.savedCrypto != nil && s.savedCrypto.locked
}

// savedModeLocked snapshots (mode, locked) under savedMu.RLock for the UI.
func (s *Server) savedModeLocked() (string, bool) {
	s.savedMu.RLock()
	defer s.savedMu.RUnlock()
	if s.savedCrypto == nil {
		return "device", false
	}
	return s.savedCrypto.mode, s.savedCrypto.locked
}

// savedLockState reports the current encryption mode/lock status for the UI.
func (s *Server) savedLockState(w http.ResponseWriter, r *http.Request) {
	mode, locked := s.savedModeLocked()
	writeJSON(w, map[string]any{"mode": mode, "locked": locked})
}

// handleSavedLock dispatches the lock-management endpoints:
//
//	GET  /api/saved/lock         -> {mode, locked}
//	POST /api/saved/lock         -> set a passphrase   {passphrase}
//	POST /api/saved/lock/remove  -> back to device     {passphrase-less; must be unlocked}
//	POST /api/saved/lock/reset   -> discard everything (forgotten-passphrase escape hatch)
func (s *Server) handleSavedLock(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.savedLockState(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.savedCrypto == nil {
		http.Error(w, "encryption unavailable", http.StatusInternalServerError)
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	if s.savedCrypto.locked {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	if err := s.savedCrypto.setPassphrase(req.Passphrase); err != nil {
		http.Error(w, "set passphrase failed", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "mode": "passphrase"})
}

// handleSavedUnlock unlocks a passphrase-protected store for the session and
// swaps the live crypto handle into the server + saved-media cache.
func (s *Server) handleSavedUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Derive the key (slow Argon2id) BEFORE taking the write lock so concurrent
	// lock-state reads aren't stalled for the unlock's duration.
	un, err := unlockSavedCrypto(s.dataDir, req.Passphrase)
	if err == errBadPassphrase {
		http.Error(w, "wrong passphrase", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "unlock failed", http.StatusInternalServerError)
		return
	}
	s.savedMu.Lock()
	s.savedCrypto = un
	sm := s.savedMedia
	s.savedMu.Unlock()
	if sm != nil {
		sm.setCrypto(un)
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleSavedLockRemove reverts a passphrase store to transparent device mode.
func (s *Server) handleSavedLockRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.savedCrypto == nil {
		http.Error(w, "encryption unavailable", http.StatusInternalServerError)
		return
	}
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	if s.savedCrypto.locked {
		http.Error(w, "locked", http.StatusLocked)
		return
	}
	if err := s.savedCrypto.removePassphrase(); err != nil {
		http.Error(w, "remove failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "mode": "device"})
}

// handleSavedLockReset is the forgotten-passphrase escape hatch: discard the
// sealed store + media + keyring and re-init a fresh transparent store.
func (s *Server) handleSavedLockReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.savedCrypto == nil {
		http.Error(w, "encryption unavailable", http.StatusInternalServerError)
		return
	}
	s.savedMu.Lock()
	defer s.savedMu.Unlock()
	if err := s.savedCrypto.resetSaved(s.dataDir); err != nil {
		http.Error(w, "reset failed", http.StatusInternalServerError)
		return
	}
	if s.savedMedia != nil {
		s.savedMedia.setCrypto(s.savedCrypto)
		s.savedMedia.resetSize()
	}
	writeJSON(w, map[string]any{"ok": true})
}
