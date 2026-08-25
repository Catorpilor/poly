package live

import "testing"

// SeriesWatchMarket (issue #94): moneylines and game/map winners are in the
// series-continuation watch set; props are not.
func TestSeriesWatchMarket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		question string
		want     bool
	}{
		{"Dota 2: TEAM VISION vs Team Spirit (BO5) - The International Playoffs", true}, // series ML
		{"Will CA Osasuna win on 2026-08-24?", true},                                    // plain ML
		{"Dota 2: TEAM VISION vs Team Spirit - Game 3 Winner", true},
		{"Counter-Strike: FUT Esports vs Spirit - Map 4 Winner", true},
		{"LoL: ThunderTalk Gaming vs LGD Gaming - Game 1 Winner", true},
		{"Game 1: Both Teams Beat Roshan?", false},
		{"Total Kills Over/Under 50.5 in Game 1?", false},
		{"First Blood in Game 1?", false},
		{"Game Handicap: TS (-1.5) vs Team Yandex (+1.5)", false},
		{"Games Total: O/U 2.5", false},
		{"Game 2: Any Player Rampage?", false},
	}
	for _, tt := range tests {
		if got := SeriesWatchMarket(tt.question); got != tt.want {
			t.Errorf("SeriesWatchMarket(%q) = %v, want %v", tt.question, got, tt.want)
		}
	}
}
