package polymarket

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseComboLink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		param   string
		want    []ComboLegRef
		wantErr string // substring; "" = no error
	}{
		{
			name:  "first real link: Over 0.5 + Villa 3-1",
			param: "c_5437373y5436769y",
			want:  []ComboLegRef{{MarketID: "5437373", OutcomeIndex: 0}, {MarketID: "5436769", OutcomeIndex: 0}},
		},
		{
			name:  "mixed sides keep link order",
			param: "c_5801197y5889785n5905290n",
			want: []ComboLegRef{
				{MarketID: "5801197", OutcomeIndex: 0},
				{MarketID: "5889785", OutcomeIndex: 1},
				{MarketID: "5905290", OutcomeIndex: 1},
			},
		},
		{name: "missing prefix", param: "5437373y5436769y", wantErr: "not a combo link"},
		{name: "empty body", param: "c_", wantErr: "at least 2 legs"},
		{name: "single leg", param: "c_5437373y", wantErr: "at least 2 legs"},
		{name: "trailing digits without side", param: "c_5437373y5436769", wantErr: "malformed"},
		{name: "side without market id", param: "c_y5436769y", wantErr: "malformed"},
		{name: "uppercase side", param: "c_5437373Y5436769y", wantErr: "malformed"},
		{name: "separator not allowed", param: "c_5437373y-5436769y", wantErr: "malformed"},
		{name: "same market twice", param: "c_5437373y5437373n", wantErr: "repeats market 5437373"},
		{name: "leading zero id", param: "c_0437373y5436769y", wantErr: "malformed"},
		{name: "over telegram's 64-char limit", param: "c_" + strings.Repeat("1234567y", 7) + "99999999y", wantErr: "too long"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseComboLink(tt.param)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("legs = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestComboLinkRoundTrip(t *testing.T) {
	t.Parallel()

	legs := []ComboLegRef{{MarketID: "5437373", OutcomeIndex: 0}, {MarketID: "5436671", OutcomeIndex: 1}}
	param := FormatComboLink(legs)
	if param != "c_5437373y5436671n" {
		t.Fatalf("FormatComboLink = %q", param)
	}
	back, err := ParseComboLink(param)
	if err != nil || !reflect.DeepEqual(back, legs) {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
}
