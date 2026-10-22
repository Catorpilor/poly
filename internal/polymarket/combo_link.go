package polymarket

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ComboLinkPrefix starts the Telegram start parameter of a Combo Link.
const ComboLinkPrefix = "c_"

// maxStartParamLen is Telegram's limit on a /start deep-link parameter.
const maxStartParamLen = 64

// ComboLegRef names one Leg of a Combo as carried by a Combo Link: a Gamma
// market and the outcome taken in it (0 = first outcome, e.g. "Yes" or
// "Over"; 1 = second). See ADR 0009.
type ComboLegRef struct {
	MarketID     string
	OutcomeIndex int
}

// errNotComboLink is returned for start parameters without the c_ prefix.
var errNotComboLink = errors.New("not a combo link")

// comboLegPattern matches the whole body: one or more <id><y|n> pairs,
// where an id is a positive decimal Gamma market id.
var (
	comboBodyPattern = regexp.MustCompile(`^(?:[1-9][0-9]*[yn])+$`)
	comboLegPattern  = regexp.MustCompile(`([1-9][0-9]*)([yn])`)
)

// ParseComboLink decodes a Combo Link start parameter such as
// "c_5437373y5436769y" into its legs, in link order.
func ParseComboLink(param string) ([]ComboLegRef, error) {
	if !strings.HasPrefix(param, ComboLinkPrefix) {
		return nil, errNotComboLink
	}
	if len(param) > maxStartParamLen {
		return nil, fmt.Errorf("combo link too long (%d > %d chars)", len(param), maxStartParamLen)
	}
	body := strings.TrimPrefix(param, ComboLinkPrefix)
	if body != "" && !comboBodyPattern.MatchString(body) {
		return nil, fmt.Errorf("malformed combo link %q", param)
	}

	matches := comboLegPattern.FindAllStringSubmatch(body, -1)
	if len(matches) < 2 {
		return nil, fmt.Errorf("a combo needs at least 2 legs, link has %d", len(matches))
	}
	legs := make([]ComboLegRef, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for _, m := range matches {
		id := m[1]
		if seen[id] {
			return nil, fmt.Errorf("combo link repeats market %s", id)
		}
		seen[id] = true
		idx := 0
		if m[2] == "n" {
			idx = 1
		}
		legs = append(legs, ComboLegRef{MarketID: id, OutcomeIndex: idx})
	}
	return legs, nil
}

// FormatComboLink encodes legs as a Combo Link start parameter. It is the
// inverse of ParseComboLink; the external notifier builds the same string.
func FormatComboLink(legs []ComboLegRef) string {
	var b strings.Builder
	b.WriteString(ComboLinkPrefix)
	for _, l := range legs {
		b.WriteString(l.MarketID)
		if l.OutcomeIndex == 0 {
			b.WriteByte('y')
		} else {
			b.WriteByte('n')
		}
	}
	return b.String()
}
