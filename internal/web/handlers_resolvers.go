package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/sartoopjj/thefeed/internal/client"
)

func (s *Server) handleActiveResolvers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.mu.RLock()
	fetcher := s.fetcher
	s.mu.RUnlock()
	if fetcher == nil {
		writeJSON(w, map[string]any{"resolvers": []string{}, "scoreboard": []client.ResolverInfo{}})
		return
	}
	writeJSON(w, map[string]any{
		"resolvers":  fetcher.Resolvers(),
		"all":        fetcher.AllResolvers(),
		"scoreboard": fetcher.ResolverScoreboard(),
	})
}

func (s *Server) handleRemoveResolver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Addr == "" {
		http.Error(w, "addr required", 400)
		return
	}
	s.mu.RLock()
	fetcher := s.fetcher
	s.mu.RUnlock()
	if fetcher == nil {
		http.Error(w, "no active fetcher", 400)
		return
	}
	fetcher.RemoveActiveResolver(req.Addr)

	// Persist the removal in the currently-selected named list. Without
	// this, the resolver pops back on next start because applySelectedList
	// re-applies the on-disk list verbatim. Scope is intentionally narrow:
	// only the active list is touched — the bank and other named lists
	// keep the resolver until the user removes it from the bank.
	listChanged := false
	if pl, err := s.loadProfiles(); err == nil && pl != nil {
		if list := findList(pl, pl.SelectedList); list != nil {
			out := list.Resolvers[:0]
			for _, r := range list.Resolvers {
				if r == req.Addr {
					listChanged = true
					continue
				}
				out = append(out, r)
			}
			if listChanged {
				list.Resolvers = out
				_ = s.saveProfiles(pl)
			}
		}
	}
	if listChanged {
		// Push tab counts to any open Resolver Bank modal — without
		// this, the badge stays at the old count until the user
		// switches lists.
		s.broadcast("event: update\ndata: \"resolver-lists\"\n\n")
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleResetResolverStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	s.mu.RLock()
	fetcher := s.fetcher
	s.mu.RUnlock()
	if fetcher == nil {
		http.Error(w, "no active fetcher", 400)
		return
	}
	fetcher.ResetStats()
	// Persistence: clear pl.ResolverScores on disk too. Without this, the
	// in-memory reset is lost on next restart — the fetcher reloads the
	// pre-reset scores from profiles.json on initFetcher.
	s.profilesMu.Lock()
	pl, err := s.loadProfilesExisting()
	if err == nil && pl != nil && len(pl.ResolverScores) > 0 {
		pl.ResolverScores = nil
		if err := s.saveProfiles(pl); err != nil {
			s.addLog(fmt.Sprintf("reset stats: save profiles failed: %v", err))
		}
	}
	s.profilesMu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// handleResolverBank manages the shared resolver bank.
// GET: returns all bank resolvers with scores.
// POST: adds resolvers to the bank.
// DELETE: removes specific resolvers from the bank.
func (s *Server) handleResolverBank(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pl, _ := s.loadProfiles()
		if pl == nil {
			pl = &ProfileList{}
		}

		// Get live stats from the current fetcher.
		var liveStats map[string][3]int64
		var activeSet map[string]bool
		s.mu.RLock()
		if s.fetcher != nil {
			liveStats = s.fetcher.ExportStats()
			activeSet = make(map[string]bool)
			for _, r := range s.fetcher.Resolvers() {
				k := r
				if !strings.Contains(k, ":") {
					k += ":53"
				}
				activeSet[k] = true
			}
		}
		s.mu.RUnlock()
		if activeSet == nil {
			activeSet = make(map[string]bool)
		}

		type bankResolver struct {
			Addr    string  `json:"addr"`
			Score   float64 `json:"score"`
			Success int64   `json:"success"`
			Failure int64   `json:"failure"`
			AvgMs   float64 `json:"avgMs"`
			Active  bool    `json:"active"`
		}

		var bank []bankResolver
		for _, addr := range pl.ResolverBank {
			key := addr
			if !strings.Contains(key, ":") {
				key += ":53"
			}
			br := bankResolver{Addr: addr, Active: activeSet[key]}
			// Prefer live stats, fall back to saved scores.
			if liveStats != nil {
				if st, ok := liveStats[key]; ok {
					br.Success = st[0]
					br.Failure = st[1]
					if st[0] > 0 {
						br.AvgMs = float64(st[2]) / float64(st[0])
					}
					br.Score = computeResolverScore(st[0], st[1], st[2])
				} else if ss, ok := pl.ResolverScores[addr]; ok {
					br.Success = ss.Success
					br.Failure = ss.Failure
					if ss.Success > 0 {
						br.AvgMs = float64(ss.TotalMs) / float64(ss.Success)
					}
					br.Score = computeResolverScore(ss.Success, ss.Failure, ss.TotalMs)
				} else {
					br.Score = 0.2
				}
			} else if ss, ok := pl.ResolverScores[addr]; ok {
				br.Success = ss.Success
				br.Failure = ss.Failure
				if ss.Success > 0 {
					br.AvgMs = float64(ss.TotalMs) / float64(ss.Success)
				}
				br.Score = computeResolverScore(ss.Success, ss.Failure, ss.TotalMs)
			} else {
				br.Score = 0.2
			}
			bank = append(bank, br)
		}

		sort.Slice(bank, func(i, j int) bool { return bank[i].Score > bank[j].Score })
		writeJSON(w, map[string]any{"bank": bank, "count": len(pl.ResolverBank)})

	case http.MethodPost:
		var req struct {
			Resolvers []string `json:"resolvers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
		pl, _ := s.loadProfiles()
		if pl == nil {
			pl = &ProfileList{}
		}
		added := addToBank(pl, req.Resolvers)
		if err := s.saveProfiles(pl); err != nil {
			http.Error(w, "save failed", 500)
			return
		}
		// Update the fetcher's resolver pool.
		s.mu.RLock()
		f := s.fetcher
		s.mu.RUnlock()
		if f != nil {
			f.UpdateResolverPool(pl.ResolverBank)
		}
		writeJSON(w, map[string]any{"ok": true, "added": added, "total": len(pl.ResolverBank)})

	case http.MethodDelete:
		var req struct {
			Addrs []string `json:"addrs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Addrs) == 0 {
			http.Error(w, "addrs required", 400)
			return
		}
		pl, _ := s.loadProfiles()
		if pl == nil {
			writeJSON(w, map[string]any{"ok": true, "removed": 0, "remaining": 0})
			return
		}
		removeSet := make(map[string]bool)
		for _, a := range req.Addrs {
			removeSet[a] = true
		}
		filtered := make([]string, 0, len(pl.ResolverBank))
		for _, r := range pl.ResolverBank {
			if !removeSet[r] {
				filtered = append(filtered, r)
			}
		}
		removed := len(pl.ResolverBank) - len(filtered)
		pl.ResolverBank = filtered
		for _, a := range req.Addrs {
			delete(pl.ResolverScores, a)
		}
		// Strip these resolvers from every saved active list — keeping
		// a list pointing at a resolver no longer in the bank would
		// surface as a "ghost" resolver the fetcher would try to use.
		listsTouched := pruneResolversFromLists(pl, removeSet)
		_ = s.saveProfiles(pl)
		s.mu.RLock()
		f := s.fetcher
		s.mu.RUnlock()
		if f != nil {
			f.UpdateResolverPool(pl.ResolverBank)
			// If the selected list lost members, reapply it so the
			// fetcher's active set matches the trimmed list.
			if listsTouched {
				if list := findList(pl, pl.SelectedList); list != nil && len(list.Resolvers) > 0 {
					f.SetActiveResolvers(list.Resolvers)
				}
			}
		}
		s.broadcast("event: update\ndata: \"resolver-lists\"\n\n")
		writeJSON(w, map[string]any{"ok": true, "removed": removed, "remaining": len(pl.ResolverBank)})

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleResolverBankCleanup removes resolvers with score below a threshold.
func (s *Server) handleResolverBankCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		MinScore float64 `json:"minScore"`
		DryRun   bool    `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.MinScore <= 0 {
		http.Error(w, "minScore must be > 0", 400)
		return
	}

	pl, _ := s.loadProfiles()
	if pl == nil {
		writeJSON(w, map[string]any{"ok": true, "removed": 0, "remaining": 0})
		return
	}

	// Get live stats for score computation.
	var liveStats map[string][3]int64
	s.mu.RLock()
	if s.fetcher != nil {
		liveStats = s.fetcher.ExportStats()
	}
	s.mu.RUnlock()

	var filtered []string
	removed := 0
	for _, addr := range pl.ResolverBank {
		key := addr
		if !strings.Contains(key, ":") {
			key += ":53"
		}
		var score float64
		if liveStats != nil {
			if st, ok := liveStats[key]; ok {
				score = computeResolverScore(st[0], st[1], st[2])
			} else if ss, ok := pl.ResolverScores[addr]; ok {
				score = computeResolverScore(ss.Success, ss.Failure, ss.TotalMs)
			} else {
				score = 0.2
			}
		} else if ss, ok := pl.ResolverScores[addr]; ok {
			score = computeResolverScore(ss.Success, ss.Failure, ss.TotalMs)
		} else {
			score = 0.2
		}
		if score >= req.MinScore {
			filtered = append(filtered, addr)
		} else {
			removed++
		}
	}

	if req.DryRun {
		writeJSON(w, map[string]any{"ok": true, "removed": removed, "remaining": len(filtered)})
		return
	}

	// Apply the cleanup.
	for _, addr := range pl.ResolverBank {
		key := addr
		if !strings.Contains(key, ":") {
			key += ":53"
		}
		var score float64
		if liveStats != nil {
			if st, ok := liveStats[key]; ok {
				score = computeResolverScore(st[0], st[1], st[2])
			} else if ss, ok := pl.ResolverScores[addr]; ok {
				score = computeResolverScore(ss.Success, ss.Failure, ss.TotalMs)
			} else {
				score = 0.2
			}
		} else if ss, ok := pl.ResolverScores[addr]; ok {
			score = computeResolverScore(ss.Success, ss.Failure, ss.TotalMs)
		} else {
			score = 0.2
		}
		if score < req.MinScore {
			delete(pl.ResolverScores, addr)
		}
	}
	// Build the removed-set for list pruning (addresses that didn't
	// make the score cut).
	removedSet := make(map[string]bool)
	keep := make(map[string]bool, len(filtered))
	for _, k := range filtered {
		keep[k] = true
	}
	for _, addr := range pl.ResolverBank {
		if !keep[addr] {
			removedSet[addr] = true
		}
	}
	pl.ResolverBank = filtered
	listsTouched := pruneResolversFromLists(pl, removedSet)
	_ = s.saveProfiles(pl)
	s.mu.RLock()
	f := s.fetcher
	s.mu.RUnlock()
	if f != nil {
		f.UpdateResolverPool(pl.ResolverBank)
		if listsTouched {
			if list := findList(pl, pl.SelectedList); list != nil && len(list.Resolvers) > 0 {
				f.SetActiveResolvers(list.Resolvers)
			}
		}
	}
	s.broadcast("event: update\ndata: \"resolver-lists\"\n\n")
	writeJSON(w, map[string]any{"ok": true, "removed": removed, "remaining": len(filtered)})
}
