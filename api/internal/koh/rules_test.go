package koh

import (
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The worked examples from the spec (base_score = 2.0000).
func TestTickScoreSpecExamples(t *testing.T) {
	base := Units(20000)
	cases := []struct {
		name     string
		streak   int
		takeover bool
		want     string
	}{
		{"first claim of an empty hill", 1, false, "2.0000"},
		{"5th consecutive tick: no bonus yet", 5, false, "2.0000"},
		{"6th consecutive tick", 6, false, "2.2000"},
		{"10th consecutive tick", 10, false, "2.2000"},
		{"11th consecutive tick", 11, false, "2.4000"},
		{"taken from a rival", 1, true, "3.0000"},
	}
	for _, c := range cases {
		got, _ := TickScore(base, c.streak, c.takeover)
		if got.String() != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestTickScoreAwardsAndRounding(t *testing.T) {
	total, awards := TickScore(Units(20001), 6, false) // 10% of 2.0001 = 0.20001 -> 0.2000
	if total.String() != "2.2001" || len(awards) != 1 || awards[0].Type != "streak" || awards[0].Score.String() != "0.2000" {
		t.Fatalf("got %s %+v", total, awards)
	}
	if _, awards := TickScore(Units(20000), 1, false); len(awards) != 0 {
		t.Fatalf("plain hold must have no awards, got %+v", awards)
	}
}

func TestUnits(t *testing.T) {
	for in, want := range map[string]string{"2": "2.0000", "2.5": "2.5000", "0.0001": "0.0001", "10.25": "10.2500"} {
		u, err := ParseUnits(in)
		if err != nil || u.String() != want {
			t.Errorf("ParseUnits(%q) = %s, %v; want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "1.23456", "abc", ".5", "1e3"} {
		if _, err := ParseUnits(bad); err == nil {
			t.Errorf("ParseUnits(%q) should fail", bad)
		}
	}
	if Units(25000).Float() != 2.5 {
		t.Error("Float")
	}
}

func TestNextState(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	cases := []struct {
		name       string
		prev       *uuid.UUID
		prevStreak int
		now        *uuid.UUID
		want       transition
	}{
		{"stays empty", nil, 0, nil, transition{}},
		{"claim empty hill", nil, 0, &a, transition{change: "claim", streak: 1}},
		{"keep holding", &a, 5, &a, transition{streak: 6}},
		{"take from rival", &a, 9, &b, transition{change: "takeover", streak: 1, takeover: true}},
		{"lose it", &a, 3, nil, transition{change: "lost"}},
	}
	for _, c := range cases {
		if got := nextState(c.prev, c.prevStreak, c.now); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	// Abandon-then-reclaim is a claim, not a takeover (no bonus).
	lost := nextState(&a, 4, nil)
	if lost.streak != 0 {
		t.Fatal("streak must reset on loss")
	}
	if re := nextState(nil, lost.streak, &a); re.takeover || re.change != "claim" {
		t.Fatalf("re-claim after loss: %+v", re)
	}
}

func TestNextTickAfterStaysOnGrid(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 14, 0, 0, 0, cst)
	p := 5 * time.Minute
	if got := nextTickAfter(t0, p, t0.Add(3*time.Second)); !got.Equal(t0.Add(p)) {
		t.Errorf("on time: %v", got)
	}
	// 17 minutes late: skip the missed 14:05/14:10/14:15 slots, land on 14:20.
	if got := nextTickAfter(t0, p, t0.Add(17*time.Minute)); !got.Equal(t0.Add(20 * time.Minute)) {
		t.Errorf("behind: %v", got)
	}
	// Exactly on a boundary: that slot is due now, so the next is one later.
	if got := nextTickAfter(t0, p, t0.Add(10*time.Minute)); !got.Equal(t0.Add(15 * time.Minute)) {
		t.Errorf("boundary: %v", got)
	}
}

func TestAssignRanksTieBreaks(t *testing.T) {
	early := time.Date(2026, 9, 22, 14, 0, 0, 0, cst)
	late := early.Add(time.Minute)
	list := []*standing{
		{Name: "zed", Total: 50000, Last: &early},
		{Name: "amy", Total: 50000, Last: &late},  // same score, scored later -> behind zed
		{Name: "bob", Total: 50000, Last: &early}, // full tie with zed -> name order
		{Name: "top", Total: 90000, Last: &late},
		{Name: "zero", Total: 0},
		{Name: "banned", Total: 99000, Last: &early, Disabled: true},
	}
	assignRanks(list, func(s *standing) (Units, *time.Time) { return s.Total, s.Last }, func(s *standing, r int) { s.Rank = r })
	want := []struct {
		name string
		rank int
	}{{"top", 1}, {"bob", 2}, {"zed", 3}, {"amy", 4}}
	for i, w := range want {
		if list[i].Name != w.name || list[i].Rank != w.rank {
			t.Fatalf("position %d: got %s#%d, want %s#%d", i, list[i].Name, list[i].Rank, w.name, w.rank)
		}
	}
	for _, s := range list[4:] {
		if s.Rank != 0 {
			t.Errorf("%s must be unranked, got %d", s.Name, s.Rank)
		}
	}
}

func TestBearerToken(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abc":     "abc",
		"bearer   abc  ": "abc",
		"  Bearer abc\t": "abc",
	} {
		if got, ok := bearerToken(in); !ok || got != want {
			t.Errorf("bearerToken(%q) = %q, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "Bearer", "Bearer   ", "Basic abc", "abc"} {
		if _, ok := bearerToken(bad); ok {
			t.Errorf("bearerToken(%q) should be malformed", bad)
		}
	}
}

func TestParseISOTime(t *testing.T) {
	want := time.Date(2026, 9, 22, 14, 15, 0, 0, cst)
	for _, in := range []string{
		"2026-09-22T14:15:00+08:00",
		"2026-09-22T06:15:00Z",
		"2026-09-22T14:15:00",       // no zone -> +08:00
		"2026-09-22T14:15:00 08:00", // unencoded '+' in a query string
	} {
		got, err := parseISOTime(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseISOTime(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"yesterday", "2026-13-01", "1900-01-01T00:00:00Z"} {
		if _, err := parseISOTime(bad); err == nil {
			t.Errorf("parseISOTime(%q) should fail", bad)
		}
	}
	if s := isoTime(want.UTC()); s != "2026-09-22T14:15:00+08:00" {
		t.Errorf("isoTime = %s", s)
	}
}

func TestPageParams(t *testing.T) {
	p, err := pageParams(url.Values{})
	if err != nil || p.Page != 1 || p.Size != 50 {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	for _, q := range []string{"page=0", "page_size=0", "page_size=201", "page=x"} {
		v, _ := url.ParseQuery(q)
		if _, err := pageParams(v); err == nil {
			t.Errorf("%s should be rejected", q)
		}
	}
}

func TestPlayerFlagShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		f := NewPlayerFlag()
		if len(f) != 20 {
			t.Fatalf("len %d", len(f))
		}
		for _, c := range f {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				t.Fatalf("non-alphanumeric %q", f)
			}
		}
		if seen[f] {
			t.Fatal("duplicate flag")
		}
		seen[f] = true
	}
}
