package web

import "testing"

// TestUsableBankResolvers covers which bank resolvers are reused WITHOUT a scan
// on config switch/boot: only VALIDATED entries (a recorded success), ordered
// best-score-first. A freshly imported config (no score history) reuses nothing
// so the caller scans it instead of activating unproven resolvers.
func TestUsableBankResolvers(t *testing.T) {
	mk := func(success, failure, totalMs int64) *SavedResolverScore {
		return &SavedResolverScore{Success: success, Failure: failure, TotalMs: totalMs}
	}

	t.Run("nil and empty bank return nil", func(t *testing.T) {
		if got := usableBankResolvers(nil); got != nil {
			t.Fatalf("nil pl: want nil, got %v", got)
		}
		if got := usableBankResolvers(&ProfileList{}); got != nil {
			t.Fatalf("empty bank: want nil, got %v", got)
		}
	})

	t.Run("fresh import with no scores reuses nothing (triggers a scan)", func(t *testing.T) {
		pl := &ProfileList{ResolverBank: []string{"a", "b", "c", "d"}}
		if got := usableBankResolvers(pl); got != nil {
			t.Fatalf("unvalidated bank must not be reused, got %v", got)
		}
	})

	t.Run("drops known-dead resolvers, keeps validated ones", func(t *testing.T) {
		pl := &ProfileList{
			ResolverBank: []string{"good1", "dead", "good2", "good3", "good4"},
			ResolverScores: map[string]*SavedResolverScore{
				"good1": mk(50, 1, 50000),
				"dead":  mk(0, 9, 0), // only failures, never a success → dead
				"good2": mk(30, 0, 30000),
				"good3": mk(10, 2, 20000),
				"good4": mk(5, 1, 10000),
			},
		}
		got := usableBankResolvers(pl)
		if len(got) != 4 {
			t.Fatalf("want 4 validated survivors, got %d (%v)", len(got), got)
		}
		for _, r := range got {
			if r == "dead" {
				t.Fatalf("known-dead resolver should be dropped, got %v", got)
			}
		}
	})

	t.Run("returns only the validated resolver even if just one", func(t *testing.T) {
		pl := &ProfileList{
			ResolverBank: []string{"good", "dead1", "dead2"},
			ResolverScores: map[string]*SavedResolverScore{
				"good":  mk(10, 0, 10000),
				"dead1": mk(0, 5, 0),
				"dead2": mk(0, 3, 0),
			},
		}
		got := usableBankResolvers(pl)
		if len(got) != 1 || got[0] != "good" {
			t.Fatalf("want only [good] (dead dropped, unproven not reused), got %v", got)
		}
	})

	t.Run("orders best score first, excludes unscored", func(t *testing.T) {
		pl := &ProfileList{
			ResolverBank: []string{"slow", "fast", "mid", "noscore"},
			ResolverScores: map[string]*SavedResolverScore{
				"slow": mk(10, 0, 100000), // 100% success but very slow
				"fast": mk(100, 0, 50000), // 100% success and quick → best
				"mid":  mk(50, 0, 100000),
				// "noscore" intentionally absent → never validated
			},
		}
		got := usableBankResolvers(pl)
		if len(got) != 3 || got[0] != "fast" {
			t.Fatalf("want [fast, mid, slow] best-first, got %v", got)
		}
		for _, r := range got {
			if r == "noscore" {
				t.Fatalf("unvalidated 'noscore' must be excluded, got %v", got)
			}
		}
	})
}
