package telegram

import "testing"

func TestParseLiveArgs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		slug string
		tape bool
		ok   bool
	}{
		{name: "no args shows usage", args: nil, ok: false},
		{name: "slug only is quiet", args: []string{"nba-lal-por-2026-01-17"}, slug: "nba-lal-por-2026-01-17", tape: false, ok: true},
		{name: "tape keyword opts in", args: []string{"nba-lal-por-2026-01-17", "tape"}, slug: "nba-lal-por-2026-01-17", tape: true, ok: true},
		{name: "tape is case-insensitive", args: []string{"nba-lal-por-2026-01-17", "TAPE"}, slug: "nba-lal-por-2026-01-17", tape: true, ok: true},
		{name: "mixed case tape", args: []string{"epl-ast-eve-2026-01-18", "Tape"}, slug: "epl-ast-eve-2026-01-18", tape: true, ok: true},
		{name: "unknown second arg shows usage", args: []string{"nba-lal-por-2026-01-17", "loud"}, ok: false},
		{name: "extra args show usage", args: []string{"nba-lal-por-2026-01-17", "tape", "extra"}, ok: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			slug, tape, ok := parseLiveArgs(tt.args)
			if ok != tt.ok || slug != tt.slug || tape != tt.tape {
				t.Errorf("parseLiveArgs(%v) = (%q, %v, %v), want (%q, %v, %v)",
					tt.args, slug, tape, ok, tt.slug, tt.tape, tt.ok)
			}
		})
	}
}

func TestLiveModeText(t *testing.T) {
	t.Parallel()
	if got := liveModeText(false); got != "quiet — snipe watch armed; add 'tape' for trade prints" {
		t.Errorf("liveModeText(false) = %q", got)
	}
	if got := liveModeText(true); got != "tape on — batched trade prints ≥ $20 every 5s" {
		t.Errorf("liveModeText(true) = %q", got)
	}
}
