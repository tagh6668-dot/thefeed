package web

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sartoopjj/thefeed/internal/client"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// runAutoUpdateLoop refreshes the active profile's AutoUpdate channels on a
// schedule that follows the server's own fetch cycle — there's no point
// polling more often than the server actually pulls fresh data from
// Telegram. User-set Profile.AutoUpdateInterval is honoured if it's >= the
// 60s floor; otherwise we align with nextFetch + settle delay.
func (s *Server) runAutoUpdateLoop(ctx context.Context) {
	select {
	case <-time.After(autoUpdateStartupDelay):
	case <-ctx.Done():
		return
	}

	var lastTick time.Time
	for {
		wait := s.nextAutoUpdateWait(lastTick)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !s.canAutoUpdate() {
			continue
		}
		s.tickAutoUpdate()
		lastTick = time.Now()
	}
}

// nextAutoUpdateWait returns how long to sleep before the next tick. Honours
// user override when set sensibly; otherwise sleeps until just after the
// server's next Telegram fetch so we always pull just-refreshed data.
func (s *Server) nextAutoUpdateWait(lastTick time.Time) time.Duration {
	pl, _ := s.loadProfiles()
	if pl != nil && pl.Active != "" {
		for _, p := range pl.Profiles {
			if p.ID != pl.Active {
				continue
			}
			if p.AutoUpdateInterval > 0 {
				user := time.Duration(p.AutoUpdateInterval) * time.Second
				if user < minAutoUpdateInterval {
					user = minAutoUpdateInterval
				}
				return user
			}
			break
		}
	}

	s.mu.RLock()
	nf := s.nextFetch
	s.mu.RUnlock()
	if nf == 0 {
		return minAutoUpdateInterval
	}
	target := time.Unix(int64(nf), 0).Add(serverFetchSettleDelay)
	delay := time.Until(target)
	if delay < minAutoUpdateInterval {
		delay = minAutoUpdateInterval
	}
	if !lastTick.IsZero() {
		if since := time.Since(lastTick); since < minAutoUpdateInterval {
			if rem := minAutoUpdateInterval - since; rem > delay {
				delay = rem
			}
		}
	}
	return delay
}

// canAutoUpdate returns false when we should skip a tick: server hasn't
// produced metadata yet (channel list empty), or there's no fetcher.
// The scanner may run concurrently — channel loading proceeds in the
// background as long as healthy resolvers are available.
func (s *Server) canAutoUpdate() bool {
	s.mu.RLock()
	channels := s.channels
	fetcher := s.fetcher
	s.mu.RUnlock()
	if fetcher == nil || len(channels) == 0 {
		return false
	}
	return true
}

func (s *Server) tickAutoUpdate() {
	pl, err := s.loadProfiles()
	if err != nil || pl == nil || pl.Active == "" {
		return
	}
	var watch []string
	for _, p := range pl.Profiles {
		if p.ID == pl.Active {
			watch = p.AutoUpdate
			break
		}
	}
	if len(watch) == 0 {
		return
	}

	s.mu.RLock()
	channels := s.channels
	s.mu.RUnlock()
	if len(channels) == 0 {
		return
	}

	wantSet := make(map[string]bool, len(watch))
	for _, name := range watch {
		wantSet[strings.TrimPrefix(strings.TrimSpace(name), "@")] = true
	}

	for i, ch := range channels {
		if !wantSet[ch.Name] {
			continue
		}
		go s.refreshChannel(i + 1) // 1-indexed
	}
}

func (s *Server) checkLatestVersion(ctx context.Context) (string, error) {
	s.mu.RLock()
	cfg := s.config
	s.mu.RUnlock()
	if cfg == nil {
		return "", fmt.Errorf("no config")
	}

	// Match the regular fetcher: selected active list, then bank, then cfg.
	resolvers := cfg.Resolvers
	var debug bool
	pl, _ := s.loadProfiles()
	if pl != nil {
		debug = pl.Debug
		if list := findList(pl, pl.SelectedList); list != nil && len(list.Resolvers) > 0 {
			resolvers = list.Resolvers
		} else if len(pl.ResolverBank) > 0 {
			resolvers = pl.ResolverBank
		}
	}

	fetcher, err := client.NewFetcher(cfg.Domain, cfg.Key, resolvers)
	if err != nil {
		return "", fmt.Errorf("create fetcher: %w", err)
	}
	if len(cfg.ExtraDomains) > 0 {
		fetcher.SetDomains(cfg.ExtraDomains)
	}
	if cfg.ServerKey != "" {
		_ = fetcher.SetServerPublicKey(cfg.ServerKey)
	}
	qm, rl, sc, to := connectionSettings(pl)
	if qm == "double" {
		fetcher.SetQueryMode(protocol.QueryMultiLabel)
	}
	fetcher.SetDebug(debug)
	s.scanner.SetDebug(debug)
	if rl > 0 {
		fetcher.SetRateLimit(rl)
	}
	if sc > 1 {
		fetcher.SetScatter(sc)
	}
	timeout := time.Duration(to * float64(time.Second))
	fetcher.SetTimeout(timeout)
	fetcher.SetLogFunc(func(msg string) {
		s.addLog(msg)
	})
	fetcher.SetActiveResolvers(resolvers)

	checkCtx, cancel := context.WithTimeout(ctx, timeout*3)
	defer cancel()
	fetcher.Start(checkCtx)

	v, err := fetcher.FetchLatestVersion(checkCtx)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.latestVersion = v
	s.mu.Unlock()
	return v, nil
}

// startCheckerThenRefresh runs the resolver health-check pass synchronously
// (in a new goroutine), then starts the periodic checker and fetches metadata.
// This ensures fresh resolver data is used for the very first metadata query.
func (s *Server) startCheckerThenRefresh() {
	s.mu.RLock()
	checker := s.checker
	ctx := s.fetcherCtx
	s.mu.RUnlock()
	if checker == nil {
		return
	}

	s.armFirstHealthyEarlyLoad(checker)
	checker.StartAndNotify(ctx, func() {
		s.refreshMetadataOnly()
	})
}

// armFirstHealthyEarlyLoad makes channel loading start the instant the initial
// resolver bank check finds its first healthy resolver, instead of waiting for
// the whole pass to finish. Gated on channels being empty so periodic re-checks
// never trigger a redundant metadata fetch.
func (s *Server) armFirstHealthyEarlyLoad(checker *client.ResolverChecker) {
	checker.SetOnFirstHealthy(func(healthy []string) {
		s.mu.RLock()
		n := len(s.channels)
		f := s.fetcher
		s.mu.RUnlock()
		if n > 0 || f == nil {
			return
		}
		f.SetActiveResolvers(healthy)
		go s.refreshMetadataOnly()
	})
}

// reuseKnownResolvers applies an already-known resolver set WITHOUT scanning,
// honoring the user's "skip rescan". Priority: the caller's live (currently
// active) set → last scan → resolver bank (only VALIDATED entries, best first).
// Returns false when nothing reusable exists, so the caller scans.
// Keeps the fetcher's pool and active set in sync (pool == active == reuse) so a
// later periodic check validates exactly these resolvers.
//
// The bank fallback only reuses resolvers that have proven themselves (a recorded
// success). A freshly imported config drops ~hundreds of UNVALIDATED resolvers
// into the bank; those must be scanned, not activated blindly — so
// usableBankResolvers returns nil for them and we fall through to a scan.
func (s *Server) reuseKnownResolvers(live []string) bool {
	s.mu.RLock()
	fetcher := s.fetcher
	checker := s.checker
	ctx := s.fetcherCtx
	s.mu.RUnlock()
	if fetcher == nil {
		return false
	}
	var reuse []string
	fromLastScan := false
	switch {
	case len(live) > 0:
		reuse = live
	default:
		if ls := s.loadLastScan(); ls != nil && len(ls.Resolvers) > 0 {
			reuse, fromLastScan = ls.Resolvers, true
		} else if pl, _ := s.loadProfiles(); pl != nil && len(pl.ResolverBank) > 0 {
			reuse = usableBankResolvers(pl)
		}
	}
	if len(reuse) == 0 {
		return false
	}
	// Pool first (it filters active to the intersection), then pin the active
	// set — leaves pool == active == reuse.
	fetcher.UpdateResolverPool(reuse)
	fetcher.SetActiveResolvers(reuse)
	if fromLastScan {
		// Seed an empty selected list / bank so the UI counts aren't 0 while the
		// fetcher happily uses the saved resolvers. No-op when already populated.
		s.persistLastScanToProfiles(reuse)
	}
	if checker != nil && ctx != nil {
		checker.StartPeriodic(ctx)
	}
	go s.refreshMetadataOnly()
	return true
}

// usableBankResolvers returns the bank resolvers that have been VALIDATED as
// working — at least one recorded success — ordered best-score-first. Resolvers
// with no score history (a freshly imported config) or only failures are
// excluded. When nothing in the bank is validated it returns nil, so the caller
// scans the bank instead of activating unproven resolvers.
func usableBankResolvers(pl *ProfileList) []string {
	if pl == nil || len(pl.ResolverBank) == 0 {
		return nil
	}
	type ranked struct {
		addr  string
		score float64
	}
	good := make([]ranked, 0, len(pl.ResolverBank))
	for _, addr := range pl.ResolverBank {
		sc := pl.ResolverScores[addr]
		if sc == nil || sc.Success == 0 {
			continue // never validated (fresh import) or known-dead → don't reuse
		}
		good = append(good, ranked{addr: addr, score: computeResolverScore(sc.Success, sc.Failure, sc.TotalMs)})
	}
	if len(good) == 0 {
		return nil
	}
	sort.SliceStable(good, func(i, j int) bool { return good[i].score > good[j].score })
	out := make([]string, len(good))
	for i, r := range good {
		out[i] = r.addr
	}
	return out
}

// skipCheckerUseSaved reuses known resolvers (live → last scan → bank) without a
// scan; only when nothing is available anywhere does it fall back to a full scan
// with retry-every-minute until the first healthy resolver appears.
func (s *Server) skipCheckerUseSaved(live []string) {
	if s.reuseKnownResolvers(live) {
		return
	}
	s.mu.RLock()
	checker := s.checker
	ctx := s.fetcherCtx
	s.mu.RUnlock()
	if checker == nil {
		return
	}
	s.armFirstHealthyEarlyLoad(checker)
	checker.StartAndNotify(ctx, func() {
		s.refreshMetadataOnly()
	})
}
