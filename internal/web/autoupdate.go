package web

import (
	"context"
	"fmt"
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

// skipCheckerUseSaved uses saved resolvers from the last scan and starts
// periodic health checks without an initial scan pass.
// If no saved resolvers are available, falls back to a full scan with
// retry-every-minute until at least one resolver is found.
func (s *Server) skipCheckerUseSaved() {
	s.mu.RLock()
	checker := s.checker
	ctx := s.fetcherCtx
	fetcher := s.fetcher
	s.mu.RUnlock()
	if checker == nil || fetcher == nil {
		return
	}
	if ls := s.loadLastScan(); ls != nil && len(ls.Resolvers) > 0 {
		fetcher.SetActiveResolvers(ls.Resolvers)
		checker.StartPeriodic(ctx)
		go s.refreshMetadataOnly()
	} else {
		// No saved resolvers — do a full scan (with retry-every-minute),
		// loading channels the moment the first healthy resolver appears.
		s.armFirstHealthyEarlyLoad(checker)
		checker.StartAndNotify(ctx, func() {
			s.refreshMetadataOnly()
		})
	}
}
