// Package koh implements the CYBER-KING agent API: arenas scored on a fixed
// Tick, hills claimed by writing a player_flag into the KingFile, scattered
// hill flags, and the read-only intel endpoints agents poll. The HTTP surface
// (api.go) is mounted under its own prefix so its envelope, error codes and
// rate limits never mix with the hub API. tick.go is the scoring engine;
// admin.go is the operator surface under /admin/koh.
package koh

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Units is a score in fixed-point ten-thousandths (1 Unit = 0.0001), matching
// the NUMERIC(14,4) columns and the API's 4-decimal score strings exactly - no
// float ever touches a stored score.
type Units int64

const unitScale = 10000

// TakeoverBonus is the one-off award for taking a hill from another player.
const TakeoverBonus Units = 1 * unitScale

// Defaults when the operator doesn't set a hill's base_score / a flag's score.
const (
	DefaultBaseScore Units = 1 * unitScale
	DefaultFlagScore Units = 1 * unitScale
)

// String renders the API's decimal-string form, e.g. "2.2000".
func (u Units) String() string {
	sign := ""
	if u < 0 {
		sign, u = "-", -u
	}
	return fmt.Sprintf("%s%d.%04d", sign, int64(u)/unitScale, int64(u)%unitScale)
}

// Float is used only for POST /flag_submit, whose spec'd score is a JSON number.
func (u Units) Float() float64 { return float64(u) / unitScale }

// ParseUnits accepts "2", "2.5" or "2.5000" (at most 4 decimals, non-negative).
func ParseUnits(s string) (Units, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > 4 || strings.HasPrefix(whole, "-") || strings.HasPrefix(whole, "+") {
		return 0, fmt.Errorf("must be a non-negative decimal with at most 4 decimal places")
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > 1e9 {
		return 0, fmt.Errorf("must be a non-negative decimal with at most 4 decimal places")
	}
	var f int64
	if frac != "" {
		if f, err = strconv.ParseInt(frac+strings.Repeat("0", 4-len(frac)), 10, 64); err != nil || f < 0 {
			return 0, fmt.Errorf("must be a non-negative decimal with at most 4 decimal places")
		}
	}
	return Units(w*unitScale + f), nil
}

// Award is one bonus line of a tick score (score_detail.awards[]).
type Award struct {
	Type   string // "streak" | "takeover"
	Score  Units
	Reason string
}

// StreakPercent is the per-tick streak bonus for holding a hill N consecutive
// ticks (N counts the current tick): >10 -> 20%, >5 -> 10%. Highest tier only.
func StreakPercent(n int) int {
	switch {
	case n > 10:
		return 20
	case n > 5:
		return 10
	}
	return 0
}

// TickScore is what one hill pays its holder for one tick:
// base + streak bonus (from the hill's base) + takeover bonus (one-off).
// Bonuses round half-up to 4 decimals.
func TickScore(base Units, streak int, takeover bool) (Units, []Award) {
	total := base
	awards := []Award{}
	if pct := StreakPercent(streak); pct > 0 {
		bonus := (base*Units(pct) + 50) / 100
		total += bonus
		awards = append(awards, Award{"streak", bonus, fmt.Sprintf("连续持有 %d 轮，加成基础分的 %d%%", streak, pct)})
	}
	if takeover {
		total += TakeoverBonus
		awards = append(awards, Award{"takeover", TakeoverBonus, "从其他选手手中抢占山头"})
	}
	return total, awards
}

// tickMessage is the display-only text of a tick score record.
func tickMessage(player, hill string, base, total Units, awards []Award) string {
	parts := []string{"基础 " + base.String()}
	for _, a := range awards {
		label := "连续持有奖励"
		if a.Type == "takeover" {
			label = "易主奖励"
		}
		parts = append(parts, label+" +"+a.Score.String())
	}
	return fmt.Sprintf("%s 在 %s 得分 %s （%s）", player, hill, total, strings.Join(parts, " + "))
}

// cst is the platform's fixed display zone: every timestamp is ISO 8601 +08:00.
var cst = time.FixedZone("+08:00", 8*3600)

const isoLayout = "2006-01-02T15:04:05-07:00"

func isoTime(t time.Time) string { return t.In(cst).Format(isoLayout) }

func isoTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := isoTime(*t)
	return &s
}
