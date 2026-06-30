package web

import "testing"

// TestUsableBankResolvers covers the score-based filtering used when a config
// switch/boot reuses the resolver bank without scanning (#2): known-dead
// resolvers are dropped, the rest are best-score-first, and a freshly imported
// config (no score history) keeps its whole bank.
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

	t.Run("fresh import with no scores keeps the whole bank", func(t *testing.T) {
		pl := &ProfileList{ResolverBank: []string{"a", "b", "c", "d"}}
		got := usableBankResolvers(pl)
		if len(got) != 4 {
			t.Fatalf("want all 4 kept, got %d (%v)", len(got), got)
		}
	})

	t.Run("drops known-dead resolvers when enough survive", func(t *testing.T) {
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
			t.Fatalf("want 4 survivors, got %d (%v)", len(got), got)
		}
		for _, r := range got {
			if r == "dead" {
				t.Fatalf("known-dead resolver should be dropped, got %v", got)
			}
		}
	})

	t.Run("keeps the whole bank when too few would survive the filter", func(t *testing.T) {
		pl := &ProfileList{
			ResolverBank: []string{"good", "dead1", "dead2"},
			ResolverScores: map[string]*SavedResolverScore{
				"good":  mk(10, 0, 10000),
				"dead1": mk(0, 5, 0),
				"dead2": mk(0, 3, 0),
			},
		}
		got := usableBankResolvers(pl)
		if len(got) != 3 {
			t.Fatalf("only 1 would survive (<3) → keep all 3, got %d (%v)", len(got), got)
		}
	})

	t.Run("orders best score first", func(t *testing.T) {
		pl := &ProfileList{
			ResolverBank: []string{"slow", "fast", "mid", "noscore"},
			ResolverScores: map[string]*SavedResolverScore{
				"slow": mk(10, 0, 100000), // 100% success but very slow
				"fast": mk(100, 0, 50000), // 100% success and quick → best
				"mid":  mk(50, 0, 100000),
				// "noscore" intentionally absent → neutral default
			},
		}
		got := usableBankResolvers(pl)
		if len(got) == 0 || got[0] != "fast" {
			t.Fatalf("want 'fast' ranked first, got %v", got)
		}
		// The unscored resolver (neutral 0.2) must rank below the fast/mid/slow.
		if got[len(got)-1] != "noscore" {
			t.Fatalf("want 'noscore' ranked last, got %v", got)
		}
	})
}
