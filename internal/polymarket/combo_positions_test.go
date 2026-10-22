package polymarket

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// liveComboPositionsPage is the verbatim /v2/positions/combos response for
// the first bot-bought combo ($5, deposit wallet, 2026-10-22), legs trimmed
// to the fields read.
const liveComboPositionsPage = `{"data":[{"combo_condition_id":"0x03136a66e19d923f1296a3291daea378620000000000000000000000000000","outcome_index":0,"outcome_label":"Yes","combo_position_id":"1391242998767995004676664666465211806041451928835544662274895413769553313792","proxy_wallet":"0x4b2f0b7b91319419d52f01caf7d73d453770318b","current_size":13.798319,"entry_avg_price_usdc":0.350973,"entry_cost_usdc":4.842837,"gross_entry_cost_usdc":5.0,"entry_fees_usdc":0.15715,"realized_payout_usdc":0.0,"status":"OPEN","redeemable":false,"first_entry_at":"2026-10-22T11:06:09Z","resolved_at":null,"legs_total":2,"legs_resolved":0,"legs_pending":2,"legs":[{"leg_index":1,"leg_status":"OPEN","leg_outcome_label":"Yes","leg_current_price":0.595,"market":{"slug":"lal-bar-rea-2026-10-25-bar","question":"Will FC Barcelona win on 2026-10-25?"}},{"leg_index":0,"leg_status":"OPEN","leg_outcome_label":"Seahawks","leg_current_price":0.575,"market":{"slug":"nfl-kc-sea-2026-10-26","question":"Chiefs vs. Seahawks"}}]}],"pagination":{"limit":100,"has_more":false,"next_cursor":null}}`

func comboRow(status string, redeemable bool, title string) string {
	return `{"status":"` + status + `","redeemable":` + map[bool]string{true: "true", false: "false"}[redeemable] +
		`,"current_size":10,"gross_entry_cost_usdc":2,"legs_total":2,"legs_resolved":1,"legs":[{"leg_index":0,"leg_status":"RESOLVED_WIN","leg_outcome_label":"Yes","leg_current_price":1,"market":{"question":"` + title + `"}},{"leg_index":1,"leg_status":"OPEN","leg_outcome_label":"No","leg_current_price":0.3,"market":{"question":"Other leg"}}]}`
}

func TestGetComboPositions_DecodesLiveShape(t *testing.T) {
	t.Parallel()

	var gotPath, gotUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotUser = r.URL.Path, r.URL.Query().Get("user")
		_, _ = w.Write([]byte(liveComboPositionsPage))
	}))
	defer srv.Close()

	combos, err := NewPositionManagerWithDataAPI(srv.URL).GetComboPositions(context.Background(), common.HexToAddress("0x4b2f0b7B91319419d52f01cAf7D73D453770318b"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/v2/positions/combos" || gotUser != "0x4b2f0b7b91319419d52f01caf7d73d453770318b" {
		t.Errorf("request = %s user=%s", gotPath, gotUser)
	}
	if len(combos) != 1 {
		t.Fatalf("combos = %d, want 1", len(combos))
	}
	c := combos[0]
	if c.Status != "OPEN" || c.Shares != 13.798319 || c.Paid != 5.0 || c.LegsTotal != 2 || c.LegsResolved != 0 {
		t.Errorf("combo = %+v", c)
	}
	// Legs come back in leg_index order whatever the response order.
	if len(c.Legs) != 2 || c.Legs[0].Question != "Chiefs vs. Seahawks" || c.Legs[0].OutcomeLabel != "Seahawks" || c.Legs[1].Price != 0.595 {
		t.Errorf("legs = %+v", c.Legs)
	}
}

func TestGetComboPositions_KeepsOnlyHeldCombos(t *testing.T) {
	t.Parallel()

	page := `{"data":[` + strings.Join([]string{
		comboRow("OPEN", false, "open"),
		comboRow("PARTIAL", false, "partial"),
		comboRow("REDEEMABLE", true, "redeemable"),
		comboRow("RESOLVED_WIN", true, "won, unclaimed"),
		comboRow("RESOLVED_LOSS", false, "lost"),
		comboRow("RESOLVED_WIN", false, "won, claimed"),
		comboRow("RESOLVED_PARTIAL", false, "settled partial"),
	}, ",") + `],"pagination":{"next_cursor":null}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(page)) }))
	defer srv.Close()

	combos, err := NewPositionManagerWithDataAPI(srv.URL).GetComboPositions(context.Background(), common.HexToAddress("0x1"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range combos {
		got = append(got, c.Legs[0].Question)
	}
	if strings.Join(got, ",") != "open,partial,redeemable,won, unclaimed" {
		t.Errorf("held combos = %v", got)
	}
}

func TestGetComboPositions_FollowsCursor(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cursor")
		mu.Lock()
		cursors = append(cursors, c)
		mu.Unlock()
		if r.URL.Query().Has("offset") {
			t.Error("offset must never be sent")
		}
		if c == "" {
			_, _ = w.Write([]byte(`{"data":[` + comboRow("OPEN", false, "p1") + `],"pagination":{"next_cursor":"n2"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[` + comboRow("OPEN", false, "p2") + `],"pagination":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	combos, err := NewPositionManagerWithDataAPI(srv.URL).GetComboPositions(context.Background(), common.HexToAddress("0x1"))
	if err != nil || len(combos) != 2 {
		t.Fatalf("combos = %d, %v; want 2", len(combos), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(cursors, "|") != "|n2" {
		t.Errorf("cursors = %q", cursors)
	}
}

func TestFormatComboPositions(t *testing.T) {
	t.Parallel()

	combos := []*ComboPosition{
		{
			Status: "OPEN", Shares: 13.798319, Paid: 5, LegsTotal: 2,
			Legs: []ComboPositionLeg{
				{Question: "Chiefs vs. Seahawks", OutcomeLabel: "Seahawks", Status: "OPEN", Price: 0.575},
				{Question: "Will FC_Barcelona win?", OutcomeLabel: "Yes", Status: "RESOLVED_WIN", Price: 1},
			},
		},
		{Status: "REDEEMABLE", Redeemable: true, Shares: 4, Paid: 1, LegsTotal: 2, LegsResolved: 2,
			Legs: []ComboPositionLeg{{Question: "A", OutcomeLabel: "Yes", Status: "RESOLVED_WIN"}, {Question: "B", OutcomeLabel: "No", Status: "RESOLVED_WIN"}}},
	}
	out := FormatComboPositions(combos)
	for _, s := range []string{
		"🧩 *Combos (2)*",
		"Chiefs vs. Seahawks — *Seahawks* @ 0.57 ⏳",
		"FC\\_Barcelona", "*Yes* ✅",
		"Paid $5.00 (fees incl.) · 13.80 shares",
		"Pays $13.80 if every leg wins",
		"🎁 Won — $4.00 to redeem on polymarket.com",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in:\n%s", s, out)
		}
	}
	if FormatComboPositions(nil) != "" {
		t.Error("no combos -> no section")
	}
}

func TestScanAllStrategies_AppendsCombos(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		comboCode int
		want      []string
		notWant   []string
	}{
		{name: "combos listed after positions", comboCode: 200, want: []string{"📊 *Your Positions (1)*", "🧩 *Combos (1)*", "Chiefs vs. Seahawks"}},
		{name: "combo fetch failure does not hide positions", comboCode: 500, want: []string{"📊 *Your Positions (1)*", "⚠️ Couldn't load combos"}, notWant: []string{"🧩 *Combos"}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/positions":
					_, _ = w.Write([]byte(liveV2PositionsPage))
				case "/v2/positions/combos":
					w.WriteHeader(tt.comboCode)
					_, _ = w.Write([]byte(liveComboPositionsPage))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()

			s := &UnifiedPositionScanner{positionManager: NewPositionManagerWithDataAPI(srv.URL)}
			out, err := s.ScanAllStrategies(context.Background(), common.HexToAddress("0x1"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(out, w) {
					t.Errorf("must not contain %q:\n%s", w, out)
				}
			}
		})
	}
}
