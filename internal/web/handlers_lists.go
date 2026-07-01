package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// =====================================================================
// Named active resolver lists
//
// Users keep one ResolverBank (the master pool) but many named subsets
// of it — typically one per network situation (home, office, mobile).
// Switching the selected list hot-swaps the fetcher's active resolvers
// without rescanning. When a resolver is removed from the bank it's
// removed from every list too, so a list never references a resolver
// that no longer exists in the master pool.
// =====================================================================

const defaultListName = "Default"

// persistScanResultsToList commits checker healthy results to the
// selected list when (a) the list is empty (bank-scan fallback) or
// (b) handleRescan asked for an overwrite. Re-pins the runtime pool
// and broadcasts so the open modal repopulates.
func (s *Server) persistScanResultsToList(healthy []string) {
	if len(healthy) == 0 {
		return
	}
	s.rescanFlagMu.Lock()
	overwrite := s.rescanReplaceList
	s.rescanReplaceList = false
	s.rescanFlagMu.Unlock()

	pl, err := s.loadProfiles()
	if err != nil || pl == nil {
		return
	}
	list := findList(pl, pl.SelectedList)
	if list == nil {
		// First scan with no lists yet — seed a Default list so the
		// UI doesn't show empty after the very first scan completes.
		pl.ActiveLists = append(pl.ActiveLists, ActiveList{Name: defaultListName})
		list = &pl.ActiveLists[len(pl.ActiveLists)-1]
		pl.SelectedList = defaultListName
	}
	// Don't shrink a populated list on routine periodic checks.
	if !overwrite && len(list.Resolvers) > 0 {
		return
	}
	list.Resolvers = append([]string(nil), healthy...)
	list.LastUsed = time.Now().Unix()
	if err := s.saveProfiles(pl); err != nil {
		s.addLog(fmt.Sprintf("save profiles after scan: %v", err))
		return
	}
	s.mu.RLock()
	f := s.fetcher
	s.mu.RUnlock()
	if f != nil {
		f.UpdateResolverPool(list.Resolvers)
	}
	s.addLog(fmt.Sprintf("resolvers: list %q populated with %d healthy resolvers", list.Name, len(healthy)))
	s.broadcast("event: update\ndata: \"resolver-lists\"\n\n")
}

// persistLastScanToProfiles seeds an empty selected list and/or empty
// bank from boot-time last_scan.json so the UI counts match the
// runtime fetcher's active set.
func (s *Server) persistLastScanToProfiles(resolvers []string) {
	if len(resolvers) == 0 {
		return
	}
	pl, err := s.loadProfiles()
	if err != nil || pl == nil {
		return
	}
	changed := false
	if list := findList(pl, pl.SelectedList); list != nil && len(list.Resolvers) == 0 {
		list.Resolvers = append([]string(nil), resolvers...)
		list.LastUsed = time.Now().Unix()
		changed = true
	}
	if len(pl.ResolverBank) == 0 {
		pl.ResolverBank = append([]string(nil), resolvers...)
		changed = true
	}
	if !changed {
		return
	}
	if err := s.saveProfiles(pl); err != nil {
		s.addLog(fmt.Sprintf("save profiles after last_scan boot: %v", err))
	}
}

// applySelectedList is the boot-time short-circuit. Returns true when
// a populated saved list was applied; false routes the caller to the
// last_scan.json / full-scan fallback chain.
func (s *Server) applySelectedList() bool {
	pl, err := s.loadProfiles()
	if err != nil || pl == nil {
		return false
	}
	migrated := s.migrateActiveLists(pl)
	if list := findList(pl, pl.SelectedList); list != nil && len(list.Resolvers) > 0 {
		s.mu.RLock()
		f := s.fetcher
		s.mu.RUnlock()
		if f != nil {
			f.UpdateResolverPool(list.Resolvers)
			f.SetActiveResolvers(list.Resolvers)
			list.LastUsed = time.Now().Unix()
			if err := s.saveProfiles(pl); err != nil {
				s.addLog(fmt.Sprintf("save profiles after select: %v", err))
			}
			s.addLog(fmt.Sprintf("resolvers: applied list %q (%d resolvers)", list.Name, len(list.Resolvers)))
			return true
		}
	}
	if migrated {
		_ = s.saveProfiles(pl)
	}
	return false
}

// migrateActiveLists fills in ActiveLists for installs that pre-date
// this feature. The current last_scan.json (or, failing that, the
// resolver bank) becomes a single list named "Default" so users keep
// their current setup. Returns true if a write is needed.
func (s *Server) migrateActiveLists(pl *ProfileList) bool {
	if pl == nil || len(pl.ActiveLists) > 0 {
		// Even if lists exist, make sure SelectedList points at one.
		if findList(pl, pl.SelectedList) == nil && len(pl.ActiveLists) > 0 {
			pl.SelectedList = pl.ActiveLists[0].Name
			return true
		}
		return false
	}
	var seed []string
	if ls := s.loadLastScan(); ls != nil && len(ls.Resolvers) > 0 {
		seed = ls.Resolvers
	} else {
		// Seed only from VALIDATED bank resolvers. A freshly imported config
		// drops ~hundreds of unproven resolvers into the bank; seeding them into
		// a "Default" list would let applySelectedList activate them without a
		// scan (bypassing the scan-on-fresh-import path). usableBankResolvers
		// returns nil when nothing is validated → no list → the caller scans.
		seed = usableBankResolvers(pl)
	}
	if len(seed) == 0 {
		return false
	}
	pl.ActiveLists = []ActiveList{{
		Name:      defaultListName,
		Resolvers: append([]string(nil), seed...),
		LastUsed:  time.Now().Unix(),
	}}
	pl.SelectedList = defaultListName
	return true
}

// findList returns a pointer into pl.ActiveLists so callers can mutate
// the entry directly. Match is case-insensitive on the trimmed name.
func findList(pl *ProfileList, name string) *ActiveList {
	if pl == nil || name == "" {
		return nil
	}
	for i := range pl.ActiveLists {
		if strings.EqualFold(strings.TrimSpace(pl.ActiveLists[i].Name), strings.TrimSpace(name)) {
			return &pl.ActiveLists[i]
		}
	}
	return nil
}

// pruneResolverFromLists removes resolver from every named list. Called
// after the user removes a resolver from the bank so dangling
// references don't outlive their entry. Returns true if any list was
// modified.
func pruneResolverFromLists(pl *ProfileList, resolver string) bool {
	if pl == nil || resolver == "" {
		return false
	}
	changed := false
	for i := range pl.ActiveLists {
		out := pl.ActiveLists[i].Resolvers[:0]
		for _, r := range pl.ActiveLists[i].Resolvers {
			if r == resolver {
				changed = true
				continue
			}
			out = append(out, r)
		}
		pl.ActiveLists[i].Resolvers = out
	}
	return changed
}

// pruneResolversFromLists removes a set of resolvers from every list in
// one pass — used by the bank cleanup path that drops many at once.
func pruneResolversFromLists(pl *ProfileList, removed map[string]bool) bool {
	if pl == nil || len(removed) == 0 {
		return false
	}
	changed := false
	for i := range pl.ActiveLists {
		out := pl.ActiveLists[i].Resolvers[:0]
		for _, r := range pl.ActiveLists[i].Resolvers {
			if removed[r] {
				changed = true
				continue
			}
			out = append(out, r)
		}
		pl.ActiveLists[i].Resolvers = out
	}
	return changed
}

// sanitizeListName normalises and length-caps user-supplied names.
// Returns "" if the cleaned name is empty.
func sanitizeListName(raw string) string {
	// Drop characters that would break the frontend's inline onclick handlers /
	// HTML (quotes, backslash, angle brackets) — list names are plain labels.
	name := strings.Map(func(r rune) rune {
		switch r {
		case '\'', '"', '\\', '<', '>', '`':
			return -1
		}
		return r
	}, raw)
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

type resolverListOut struct {
	Name      string   `json:"name"`
	Count     int      `json:"count"`
	Resolvers []string `json:"resolvers,omitempty"`
	LastUsed  int64    `json:"lastUsed,omitempty"`
	Selected  bool     `json:"selected,omitempty"`
}

// handleResolverLists supports GET (enumerate), POST (create), DELETE
// (remove). Body for POST: {name, resolvers?}; if resolvers omitted,
// snapshot the currently-active resolvers under the new name.
//
// GET ?include=resolvers also serialises each list's full resolver
// address list — used by the share panel's "source = list X" picker.
func (s *Server) handleResolverLists(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeListsInfo(w, r.URL.Query().Get("include") == "resolvers")
	case http.MethodPost:
		var body struct {
			Name      string   `json:"name"`
			Resolvers []string `json:"resolvers,omitempty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		name := sanitizeListName(body.Name)
		if name == "" {
			http.Error(w, "name required", 400)
			return
		}
		pl, err := s.loadProfiles()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.migrateActiveLists(pl)
		if findList(pl, name) != nil {
			http.Error(w, "list already exists", 409)
			return
		}
		resolvers := body.Resolvers
		if len(resolvers) == 0 {
			// Default: copy the currently-active resolvers so the user
			// can label whatever's working right now.
			s.mu.RLock()
			f := s.fetcher
			s.mu.RUnlock()
			if f != nil {
				resolvers = append([]string(nil), f.Resolvers()...)
			}
		}
		pl.ActiveLists = append(pl.ActiveLists, ActiveList{
			Name:      name,
			Resolvers: append([]string(nil), resolvers...),
			LastUsed:  time.Now().Unix(),
		})
		if err := s.saveProfiles(pl); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.writeListsInfo(w)
	case http.MethodDelete:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		name := sanitizeListName(body.Name)
		if name == "" {
			http.Error(w, "name required", 400)
			return
		}
		pl, err := s.loadProfiles()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		idx := -1
		for i := range pl.ActiveLists {
			if strings.EqualFold(strings.TrimSpace(pl.ActiveLists[i].Name), name) {
				idx = i
				break
			}
		}
		if idx < 0 {
			http.Error(w, "no such list", 404)
			return
		}
		if len(pl.ActiveLists) == 1 {
			http.Error(w, "cannot delete the only list", 400)
			return
		}
		removed := pl.ActiveLists[idx].Name
		pl.ActiveLists = append(pl.ActiveLists[:idx], pl.ActiveLists[idx+1:]...)
		// If the deleted list was selected, pick the most recently used
		// remaining list as the new selection and reapply.
		if strings.EqualFold(strings.TrimSpace(pl.SelectedList), removed) {
			best := 0
			for i := 1; i < len(pl.ActiveLists); i++ {
				if pl.ActiveLists[i].LastUsed > pl.ActiveLists[best].LastUsed {
					best = i
				}
			}
			pl.SelectedList = pl.ActiveLists[best].Name
			s.mu.RLock()
			f := s.fetcher
			s.mu.RUnlock()
			if f != nil {
				// Match handleResolverListSelect — pin both the pool
				// and active to the new list so the checker doesn't
				// re-broaden after the swap.
				f.UpdateResolverPool(pl.ActiveLists[best].Resolvers)
				f.SetActiveResolvers(pl.ActiveLists[best].Resolvers)
			}
		}
		if err := s.saveProfiles(pl); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.writeListsInfo(w)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// handleResolverListSelect switches the active list and hot-swaps the
// fetcher's resolver pool. No probing — the user is choosing a list
// they already trust to work in this situation. NoScan disables the
// empty-list bank-scan fallback so the user can deliberately switch
// to an empty list without kicking off a probe.
func (s *Server) handleResolverListSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Name   string `json:"name"`
		NoScan bool   `json:"noScan"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	name := sanitizeListName(body.Name)
	if name == "" {
		http.Error(w, "name required", 400)
		return
	}
	pl, err := s.loadProfiles()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	list := findList(pl, name)
	if list == nil {
		http.Error(w, "no such list", 404)
		return
	}
	pl.SelectedList = list.Name
	list.LastUsed = time.Now().Unix()
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.mu.RLock()
	f := s.fetcher
	checker := s.checker
	s.mu.RUnlock()
	// Cancel any in-progress bank scan first — the user is asking for
	// a different pool, so probing the previous one is wasted work.
	if checker != nil {
		checker.CancelCurrentScan()
	}
	if f != nil {
		switch {
		case len(list.Resolvers) > 0:
			f.UpdateResolverPool(list.Resolvers)
			f.SetActiveResolvers(list.Resolvers)
			s.addLog(fmt.Sprintf("resolvers: switched to list %q (%d resolvers)", list.Name, len(list.Resolvers)))
			go s.refreshMetadataOnly()
		case !body.NoScan && len(pl.ResolverBank) > 0:
			// Empty list, scan opted in: probe the bank as fallback.
			// CheckNow rather than StartAndNotify because the latter
			// has a one-shot guard that's already tripped post-boot.
			f.UpdateResolverPool(pl.ResolverBank)
			f.SetActiveResolvers(nil)
			s.addLog(fmt.Sprintf("resolvers: list %q is empty — scanning bank (%d) as fallback", list.Name, len(pl.ResolverBank)))
			s.mu.RLock()
			ctx := s.fetcherCtx
			s.mu.RUnlock()
			if checker != nil && ctx != nil {
				go func() {
					if checker.CheckNow(ctx) {
						s.refreshMetadataOnly()
					}
				}()
			}
		case body.NoScan:
			f.UpdateResolverPool(nil)
			f.SetActiveResolvers(nil)
			s.addLog(fmt.Sprintf("resolvers: switched to empty list %q (scan declined)", list.Name))
		default:
			f.UpdateResolverPool(nil)
			f.SetActiveResolvers(nil)
			s.addLog(fmt.Sprintf("resolvers: list %q is empty and bank is empty — run scanner first", list.Name))
		}
	}
	s.writeListsInfo(w)
}

// handleResolverListSave snapshots the currently-active fetcher
// resolvers into a list. Mode "create" fails if the name already
// exists; mode "overwrite" replaces an existing list's resolvers.
func (s *Server) handleResolverListSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Name string `json:"name"`
		Mode string `json:"mode"` // "create" (default) or "overwrite"
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	name := sanitizeListName(body.Name)
	if name == "" {
		http.Error(w, "name required", 400)
		return
	}
	// Mode "empty" creates a brand-new EMPTY list (the user fills it later from
	// the bank) — it must NOT copy the current active resolvers.
	var resolvers []string
	if body.Mode != "empty" {
		s.mu.RLock()
		f := s.fetcher
		s.mu.RUnlock()
		if f == nil {
			http.Error(w, "not configured", 400)
			return
		}
		resolvers = append([]string(nil), f.Resolvers()...)
		if len(resolvers) == 0 {
			http.Error(w, "no active resolvers to save", 400)
			return
		}
	}
	pl, err := s.loadProfiles()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.migrateActiveLists(pl)
	existing := findList(pl, name)
	if existing != nil {
		if body.Mode != "overwrite" {
			http.Error(w, "list already exists", 409)
			return
		}
		existing.Resolvers = resolvers
		existing.LastUsed = time.Now().Unix()
	} else {
		pl.ActiveLists = append(pl.ActiveLists, ActiveList{
			Name:      name,
			Resolvers: resolvers,
			LastUsed:  time.Now().Unix(),
		})
	}
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.writeListsInfo(w)
}

// handleResolverListAdd appends resolvers (typically picked from the
// Bank tab) to a named list. Deduped; hot-swaps the fetcher pool when
// the list is currently selected.
func (s *Server) handleResolverListAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Name      string   `json:"name"`
		Resolvers []string `json:"resolvers"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	name := sanitizeListName(body.Name)
	if name == "" || len(body.Resolvers) == 0 {
		http.Error(w, "name and resolvers required", 400)
		return
	}
	pl, err := s.loadProfiles()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	list := findList(pl, name)
	if list == nil {
		http.Error(w, "no such list", 404)
		return
	}
	seen := map[string]bool{}
	for _, r := range list.Resolvers {
		seen[r] = true
	}
	added := 0
	for _, r := range body.Resolvers {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		list.Resolvers = append(list.Resolvers, r)
		seen[r] = true
		added++
	}
	list.LastUsed = time.Now().Unix()
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if added > 0 && strings.EqualFold(strings.TrimSpace(pl.SelectedList), name) {
		s.mu.RLock()
		f := s.fetcher
		s.mu.RUnlock()
		if f != nil {
			// Pool widens AND active gains the new entries —
			// UpdateResolverPool alone only prunes active to the new
			// pool, never adds, so the freshly-added resolver would
			// stay invisible in the Active panel until a checker tick.
			f.UpdateResolverPool(list.Resolvers)
			f.SetActiveResolvers(list.Resolvers)
		}
	}
	s.broadcast("event: update\ndata: \"resolver-lists\"\n\n")
	writeJSON(w, map[string]any{"ok": true, "added": added, "count": len(list.Resolvers)})
}

// handleResolverListRemove removes resolvers from a named list. Body:
// {name, resolvers:[...]}. If the list is the selected one, the active pool is
// narrowed to match.
func (s *Server) handleResolverListRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Name      string   `json:"name"`
		Resolvers []string `json:"resolvers"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	name := sanitizeListName(body.Name)
	if name == "" || len(body.Resolvers) == 0 {
		http.Error(w, "name and resolvers required", 400)
		return
	}
	pl, err := s.loadProfiles()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	list := findList(pl, name)
	if list == nil {
		http.Error(w, "no such list", 404)
		return
	}
	drop := map[string]bool{}
	for _, r := range body.Resolvers {
		drop[strings.TrimSpace(r)] = true
	}
	kept := list.Resolvers[:0]
	removed := 0
	for _, r := range list.Resolvers {
		if drop[r] {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	list.Resolvers = kept
	list.LastUsed = time.Now().Unix()
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if removed > 0 && strings.EqualFold(strings.TrimSpace(pl.SelectedList), name) {
		s.mu.RLock()
		f := s.fetcher
		s.mu.RUnlock()
		if f != nil {
			f.UpdateResolverPool(list.Resolvers)
			f.SetActiveResolvers(list.Resolvers)
		}
	}
	s.broadcast("event: update\ndata: \"resolver-lists\"\n\n")
	writeJSON(w, map[string]any{"ok": true, "removed": removed, "count": len(list.Resolvers)})
}

// handleResolverListRename changes a list's display name. Body:
// {name, newName}. The selection pointer follows the rename.
func (s *Server) handleResolverListRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Name    string `json:"name"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	from := sanitizeListName(body.Name)
	to := sanitizeListName(body.NewName)
	if from == "" || to == "" {
		http.Error(w, "name and newName required", 400)
		return
	}
	if strings.EqualFold(from, to) {
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	pl, err := s.loadProfiles()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if findList(pl, to) != nil {
		http.Error(w, "name already used", 409)
		return
	}
	list := findList(pl, from)
	if list == nil {
		http.Error(w, "no such list", 404)
		return
	}
	list.Name = to
	if strings.EqualFold(strings.TrimSpace(pl.SelectedList), from) {
		pl.SelectedList = to
	}
	if err := s.saveProfiles(pl); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.writeListsInfo(w)
}

// writeListsInfo serialises the named lists. includeResolvers=true
// expands each entry with its full address list (used by share panel;
// regular polls skip it to keep responses small).
func (s *Server) writeListsInfo(w http.ResponseWriter, includeResolvers ...bool) {
	pl, _ := s.loadProfiles()
	out := struct {
		Selected string            `json:"selected"`
		Lists    []resolverListOut `json:"lists"`
	}{}
	if pl == nil {
		writeJSON(w, out)
		return
	}
	withAddrs := len(includeResolvers) > 0 && includeResolvers[0]
	out.Selected = pl.SelectedList
	for _, l := range pl.ActiveLists {
		entry := resolverListOut{
			Name:     l.Name,
			Count:    len(l.Resolvers),
			LastUsed: l.LastUsed,
			Selected: strings.EqualFold(strings.TrimSpace(l.Name), strings.TrimSpace(pl.SelectedList)),
		}
		if withAddrs {
			entry.Resolvers = append([]string(nil), l.Resolvers...)
		}
		out.Lists = append(out.Lists, entry)
	}
	writeJSON(w, out)
}
