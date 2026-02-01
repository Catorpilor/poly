package live

import (
	"testing"
)

func TestGetPrimaryMarket(t *testing.T) {
	resolver := NewEventSlugResolver()

	tests := []struct {
		name           string
		event          *EventInfo
		wantQuestion   string
		wantOutcomes   []string
		wantNil        bool
	}{
		{
			name: "Real data: Hawks vs Pacers - ML market deep in list",
			event: &EventInfo{
				ID:    "nba-atl-ind-2026-01-31",
				Title: "Hawks vs. Pacers",
				Markets: []MarketInfo{
					{ID: "1303349", Question: "Andrew Nembhard: Assists O/U 8.5", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "1309386", Question: "Hawks vs. Pacers: 1H O/U 119.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "1303749", Question: "1H Spread: Hawks (-0.5)", OutcomesRaw: `["Hawks", "Pacers"]`, Active: true, Closed: false},
					{ID: "1303750", Question: "Hawks vs. Pacers: 1H O/U 120.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "1303751", Question: "Hawks vs. Pacers: 1H Moneyline", OutcomesRaw: `["Hawks", "Pacers"]`, Active: true, Closed: false},
					{ID: "1303970", Question: "Hawks vs. Pacers: O/U 232.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "1301270", Question: "Spread: Hawks (-1.5)", OutcomesRaw: `["Hawks", "Pacers"]`, Active: true, Closed: false},
					{ID: "1305405", Question: "Jalen Johnson: Assists O/U 6.5", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "1265645", Question: "Hawks vs. Pacers", OutcomesRaw: `["Hawks", "Pacers"]`, Active: true, Closed: false},
					{ID: "1302815", Question: "Pascal Siakam: Points O/U 24.5", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Hawks vs. Pacers",
			wantOutcomes: []string{"Hawks", "Pacers"},
		},
		{
			name: "NBA game - should select ML not spreads/totals",
			event: &EventInfo{
				ID:    "nba-det-den-2026-01-27",
				Title: "Pistons vs. Nuggets",
				Markets: []MarketInfo{
					{ID: "1274422", Question: "Pistons vs. Nuggets: O/U 219.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "1274423", Question: "Jamal Murray: Points O/U 26.5", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "1278526", Question: "Spread: Pistons (-7.5)", OutcomesRaw: `["Pistons", "Nuggets"]`, Active: true, Closed: false},
					{ID: "1274892", Question: "Pistons vs. Nuggets: 1H Moneyline", OutcomesRaw: `["Pistons", "Nuggets"]`, Active: true, Closed: false},
					{ID: "1234942", Question: "Pistons vs. Nuggets", OutcomesRaw: `["Pistons", "Nuggets"]`, Active: true, Closed: false},
					{ID: "1274890", Question: "1H Spread: Pistons (-3.5)", OutcomesRaw: `["Pistons", "Nuggets"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Pistons vs. Nuggets",
			wantOutcomes: []string{"Pistons", "Nuggets"},
		},
		{
			name: "NBA game - ML market first in list",
			event: &EventInfo{
				ID:    "nba-lac-uta-2026-01-27",
				Title: "Clippers vs. Jazz",
				Markets: []MarketInfo{
					{ID: "1234931", Question: "Clippers vs. Jazz", OutcomesRaw: `["Clippers", "Jazz"]`, Active: true, Closed: false},
					{ID: "1275270", Question: "Spread: Clippers (-10.5)", OutcomesRaw: `["Clippers", "Jazz"]`, Active: true, Closed: false},
					{ID: "1274421", Question: "Clippers vs. Jazz: O/U 220.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Clippers vs. Jazz",
			wantOutcomes: []string{"Clippers", "Jazz"},
		},
		{
			name: "Esports LoL - should select ML not First Blood/Kills/Tower",
			event: &EventInfo{
				ID:    "lol-lpl-wbg-vs-ig",
				Title: "Weibo Gaming vs. Invictus Gaming",
				Markets: []MarketInfo{
					{ID: "1", Question: "First Blood in Game 1?", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
					{ID: "2", Question: "First Tower in Game 1?", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
					{ID: "3", Question: "First Dragon in Game 1?", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
					{ID: "4", Question: "First Baron in Game 1?", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
					{ID: "5", Question: "Total Kills O/U 25.5 in Game 1?", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "6", Question: "First Inhibitor in Game 1?", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
					{ID: "7", Question: "Map 1 Winner", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
					{ID: "8", Question: "Map 2 Winner", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
					{ID: "9", Question: "Series: Who will win?", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
					{ID: "10", Question: "Weibo Gaming vs. Invictus Gaming", OutcomesRaw: `["Weibo Gaming", "Invictus Gaming"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Weibo Gaming vs. Invictus Gaming",
			wantOutcomes: []string{"Weibo Gaming", "Invictus Gaming"},
		},
		{
			name: "Esports LoL - First Blood first in list should be skipped",
			event: &EventInfo{
				ID:    "lol-lck-t1-vs-gen",
				Title: "T1 vs. Gen.G",
				Markets: []MarketInfo{
					{ID: "1", Question: "First Blood in Game 1?", OutcomesRaw: `["T1", "Gen.G"]`, Active: true, Closed: false},
					{ID: "2", Question: "T1 vs. Gen.G", OutcomesRaw: `["T1", "Gen.G"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "T1 vs. Gen.G",
			wantOutcomes: []string{"T1", "Gen.G"},
		},
		{
			name: "Esports - Handicap market should be skipped",
			event: &EventInfo{
				ID:    "esports-match",
				Title: "Team A vs. Team B",
				Markets: []MarketInfo{
					{ID: "1", Question: "Handicap: Team A (-1.5)", OutcomesRaw: `["Team A", "Team B"]`, Active: true, Closed: false},
					{ID: "2", Question: "Team A vs. Team B", OutcomesRaw: `["Team A", "Team B"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Team A vs. Team B",
			wantOutcomes: []string{"Team A", "Team B"},
		},
		{
			name: "Soccer 3-way - should select ML with Draw option",
			event: &EventInfo{
				ID:    "epl-wolves-newcastle",
				Title: "Wolverhampton vs. Newcastle",
				Markets: []MarketInfo{
					{ID: "1", Question: "Total Goals O/U 2.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "2", Question: "Will Wolverhampton Wanderers FC win?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "3", Question: "Draw?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "4", Question: "Will Newcastle United FC win?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "5", Question: "First Goal Scorer", OutcomesRaw: `["Player A", "Player B"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Will Wolverhampton Wanderers FC win?",
			wantOutcomes: []string{"Yes", "No"},
		},
		{
			name: "Crypto market - Over/Under",
			event: &EventInfo{
				ID:    "crypto-btc-price",
				Title: "Bitcoin Price",
				Markets: []MarketInfo{
					{ID: "1", Question: "Will Bitcoin be over $100,000?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Will Bitcoin be over $100,000?",
			wantOutcomes: []string{"Yes", "No"},
		},
		{
			name: "NBA - Player props with Rebounds/Assists should be skipped",
			event: &EventInfo{
				ID:    "nba-game",
				Title: "Lakers vs. Celtics",
				Markets: []MarketInfo{
					{ID: "1", Question: "LeBron James: Rebounds O/U 8.5", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "2", Question: "Jayson Tatum: Assists O/U 5.5", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "3", Question: "Anthony Davis: Points O/U 27.5", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "4", Question: "Lakers vs. Celtics", OutcomesRaw: `["Lakers", "Celtics"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Lakers vs. Celtics",
			wantOutcomes: []string{"Lakers", "Celtics"},
		},
		{
			name: "NBA - 1H and 1Q markets should be skipped",
			event: &EventInfo{
				ID:    "nba-game-2",
				Title: "Heat vs. Bulls",
				Markets: []MarketInfo{
					{ID: "1", Question: "1H Moneyline: Heat vs. Bulls", OutcomesRaw: `["Heat", "Bulls"]`, Active: true, Closed: false},
					{ID: "2", Question: "1Q Spread: Heat (-2.5)", OutcomesRaw: `["Heat", "Bulls"]`, Active: true, Closed: false},
					{ID: "3", Question: "Heat vs. Bulls", OutcomesRaw: `["Heat", "Bulls"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Heat vs. Bulls",
			wantOutcomes: []string{"Heat", "Bulls"},
		},
		{
			name: "Market with spread notation (-X.X) should be skipped",
			event: &EventInfo{
				ID:    "nba-game-3",
				Title: "Warriors vs. Suns",
				Markets: []MarketInfo{
					{ID: "1", Question: "Warriors (-5.5) vs. Suns", OutcomesRaw: `["Warriors", "Suns"]`, Active: true, Closed: false},
					{ID: "2", Question: "Warriors vs. Suns (+5.5)", OutcomesRaw: `["Warriors", "Suns"]`, Active: true, Closed: false},
					{ID: "3", Question: "Warriors vs. Suns", OutcomesRaw: `["Warriors", "Suns"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Warriors vs. Suns",
			wantOutcomes: []string{"Warriors", "Suns"},
		},
		{
			name: "Only sub-markets available - fallback to 'win' keyword",
			event: &EventInfo{
				ID:    "game-no-ml",
				Title: "Team X vs. Team Y",
				Markets: []MarketInfo{
					{ID: "1", Question: "First Blood?", OutcomesRaw: `["Team X", "Team Y"]`, Active: true, Closed: false},
					{ID: "2", Question: "Total Kills O/U 20.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "3", Question: "Who will win the series?", OutcomesRaw: `["Team X", "Team Y"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Who will win the series?",
			wantOutcomes: []string{"Team X", "Team Y"},
		},
		{
			name: "Only inactive/closed markets - fallback to first active",
			event: &EventInfo{
				ID:    "game-closed",
				Title: "Old Game",
				Markets: []MarketInfo{
					{ID: "1", Question: "Team A vs. Team B", OutcomesRaw: `["Team A", "Team B"]`, Active: false, Closed: true},
					{ID: "2", Question: "Spread: Team A (-3.5)", OutcomesRaw: `["Team A", "Team B"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Spread: Team A (-3.5)",
			wantOutcomes: []string{"Team A", "Team B"},
		},
		{
			name: "Empty markets - returns nil",
			event: &EventInfo{
				ID:      "empty-event",
				Title:   "Empty Event",
				Markets: []MarketInfo{},
			},
			wantNil: true,
		},
		{
			name: "Case insensitive - FIRST BLOOD should be skipped",
			event: &EventInfo{
				ID:    "case-test",
				Title: "Case Test",
				Markets: []MarketInfo{
					{ID: "1", Question: "FIRST BLOOD IN GAME 1", OutcomesRaw: `["Team A", "Team B"]`, Active: true, Closed: false},
					{ID: "2", Question: "Team A vs. Team B", OutcomesRaw: `["Team A", "Team B"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Team A vs. Team B",
			wantOutcomes: []string{"Team A", "Team B"},
		},
		{
			name: "Score keyword should be skipped",
			event: &EventInfo{
				ID:    "score-test",
				Title: "Score Test",
				Markets: []MarketInfo{
					{ID: "1", Question: "Correct Score: 2-1?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "2", Question: "Team A vs. Team B", OutcomesRaw: `["Team A", "Team B"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Team A vs. Team B",
			wantOutcomes: []string{"Team A", "Team B"},
		},
		{
			name: "Goals and Score keywords should be skipped (soccer)",
			event: &EventInfo{
				ID:    "goals-test",
				Title: "Soccer Match",
				Markets: []MarketInfo{
					{ID: "1", Question: "Total Goals O/U 2.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "2", Question: "Both Teams to Score?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "3", Question: "Chelsea vs. Arsenal", OutcomesRaw: `["Chelsea", "Arsenal"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Chelsea vs. Arsenal",
			wantOutcomes: []string{"Chelsea", "Arsenal"},
		},
		{
			name: "O/U abbreviation should be skipped",
			event: &EventInfo{
				ID:    "ou-test",
				Title: "NBA Game",
				Markets: []MarketInfo{
					{ID: "1", Question: "Lakers vs. Warriors: O/U 225.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "2", Question: "Lakers vs. Warriors", OutcomesRaw: `["Lakers", "Warriors"]`, Active: true, Closed: false},
				},
			},
			wantQuestion: "Lakers vs. Warriors",
			wantOutcomes: []string{"Lakers", "Warriors"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolver.GetPrimaryMarket(tt.event)

			if tt.wantNil {
				if got != nil {
					t.Errorf("GetPrimaryMarket() = %v, want nil", got.Question)
				}
				return
			}

			if got == nil {
				t.Fatalf("GetPrimaryMarket() = nil, want question %q", tt.wantQuestion)
			}

			if got.Question != tt.wantQuestion {
				t.Errorf("GetPrimaryMarket() question = %q, want %q", got.Question, tt.wantQuestion)
			}

			gotOutcomes := got.GetOutcomes()
			if len(gotOutcomes) != len(tt.wantOutcomes) {
				t.Errorf("GetPrimaryMarket() outcomes = %v, want %v", gotOutcomes, tt.wantOutcomes)
				return
			}
			for i, o := range gotOutcomes {
				if o != tt.wantOutcomes[i] {
					t.Errorf("GetPrimaryMarket() outcomes[%d] = %q, want %q", i, o, tt.wantOutcomes[i])
				}
			}
		})
	}
}

func TestGetAllMLMarkets(t *testing.T) {
	resolver := NewEventSlugResolver()

	tests := []struct {
		name       string
		event      *EventInfo
		wantCount  int
		wantFirst  string
	}{
		{
			name: "Soccer 3-way ML - should return all 3 ML markets",
			event: &EventInfo{
				ID:    "epl-match",
				Title: "Liverpool vs. Man City",
				Markets: []MarketInfo{
					{ID: "1", Question: "Will Liverpool FC win?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "2", Question: "Draw?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "3", Question: "Will Manchester City FC win?", OutcomesRaw: `["Yes", "No"]`, Active: true, Closed: false},
					{ID: "4", Question: "Total Goals O/U 2.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
					{ID: "5", Question: "First Goal Scorer", OutcomesRaw: `["Player A", "Player B"]`, Active: true, Closed: false},
				},
			},
			wantCount: 3,
			wantFirst: "Will Liverpool FC win?",
		},
		{
			name: "NBA 2-way ML - should return 1 ML market",
			event: &EventInfo{
				ID:    "nba-match",
				Title: "Lakers vs. Warriors",
				Markets: []MarketInfo{
					{ID: "1", Question: "Lakers vs. Warriors", OutcomesRaw: `["Lakers", "Warriors"]`, Active: true, Closed: false},
					{ID: "2", Question: "Spread: Lakers (-5.5)", OutcomesRaw: `["Lakers", "Warriors"]`, Active: true, Closed: false},
					{ID: "3", Question: "Lakers vs. Warriors: O/U 225.5", OutcomesRaw: `["Over", "Under"]`, Active: true, Closed: false},
				},
			},
			wantCount: 1,
			wantFirst: "Lakers vs. Warriors",
		},
		{
			name: "Esports - should filter out all sub-markets",
			event: &EventInfo{
				ID:    "lol-match",
				Title: "DRX vs. HLE",
				Markets: []MarketInfo{
					{ID: "1", Question: "First Blood Game 1", OutcomesRaw: `["DRX", "HLE"]`, Active: true, Closed: false},
					{ID: "2", Question: "First Tower Game 1", OutcomesRaw: `["DRX", "HLE"]`, Active: true, Closed: false},
					{ID: "3", Question: "DRX vs. HLE", OutcomesRaw: `["DRX", "HLE"]`, Active: true, Closed: false},
					{ID: "4", Question: "Map 1 Winner", OutcomesRaw: `["DRX", "HLE"]`, Active: true, Closed: false},
				},
			},
			wantCount: 1,
			wantFirst: "DRX vs. HLE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolver.GetAllMLMarkets(tt.event)

			if len(got) != tt.wantCount {
				questions := make([]string, len(got))
				for i, m := range got {
					questions[i] = m.Question
				}
				t.Errorf("GetAllMLMarkets() count = %d, want %d. Got: %v", len(got), tt.wantCount, questions)
				return
			}

			if tt.wantCount > 0 && got[0].Question != tt.wantFirst {
				t.Errorf("GetAllMLMarkets()[0].Question = %q, want %q", got[0].Question, tt.wantFirst)
			}
		})
	}
}

func TestExtractMarketShortName(t *testing.T) {
	tests := []struct {
		question string
		want     string
	}{
		{"Will Wolverhampton Wanderers FC win?", "WOL"},
		{"Draw?", "DRAW"},
		{"Will Newcastle United FC win?", "NEW"},
		{"Will Liverpool FC win?", "LIV"},
		{"Will Manchester City FC win?", "MAN"},
		{"Team A", "TEAM"},
		{"X", "X"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.question, func(t *testing.T) {
			got := ExtractMarketShortName(tt.question)
			if got != tt.want {
				t.Errorf("ExtractMarketShortName(%q) = %q, want %q", tt.question, got, tt.want)
			}
		})
	}
}
