package koh

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// fieldErr is a query/body validation failure, rendered as 422 "<field>: <reason>".
type fieldErr struct{ field, reason string }

func (e *fieldErr) Error() string { return e.field + ": " + e.reason }

func intParam(q url.Values, name string, def, min, max int) (int, *fieldErr) {
	raw := strings.TrimSpace(q.Get(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &fieldErr{name, "must be an integer"}
	}
	if n < min || n > max {
		if max == int(^uint(0)>>1) {
			return 0, &fieldErr{name, fmt.Sprintf("must be >= %d", min)}
		}
		return 0, &fieldErr{name, fmt.Sprintf("must be between %d and %d", min, max)}
	}
	return n, nil
}

type page struct{ Page, Size int }

func (p page) offset() int { return (p.Page - 1) * p.Size }

func pageParams(q url.Values) (page, *fieldErr) {
	pg, err := intParam(q, "page", 1, 1, int(^uint(0)>>1))
	if err != nil {
		return page{}, err
	}
	// Cap page so offset arithmetic can't overflow on absurd input.
	if pg > 1_000_000 {
		return page{}, &fieldErr{"page", "must be between 1 and 1000000"}
	}
	size, err := intParam(q, "page_size", 50, 1, 200)
	if err != nil {
		return page{}, err
	}
	return page{pg, size}, nil
}

func pagination(p page, total int) map[string]any {
	return map[string]any{"page": p.Page, "page_size": p.Size, "total": total}
}

func boolParam(q url.Values, name string, def bool) (bool, *fieldErr) {
	switch strings.ToLower(strings.TrimSpace(q.Get(name))) {
	case "":
		return def, nil
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}
	return false, &fieldErr{name, "must be a boolean"}
}

// Accepted time inputs: full ISO 8601 with an offset, or without one (then
// interpreted as +08:00), with optional fractional seconds; a bare date means
// its midnight +08:00.
var naiveLayouts = []string{
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02",
}

var (
	minTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTime = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

func parseISOTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	// An unencoded "+08:00" in a query string decodes to " 08:00"; restore it.
	if i := strings.LastIndexByte(raw, ' '); i > 0 && strings.Contains(raw[:i], "T") && len(raw)-i == 6 && raw[i+3] == ':' {
		raw = raw[:i] + "+" + raw[i+1:]
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		for _, l := range naiveLayouts {
			if t, err = time.ParseInLocation(l, raw, cst); err == nil {
				break
			}
		}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("must be an ISO 8601 time")
	}
	if t.Before(minTime) || !t.Before(maxTime) {
		return time.Time{}, fmt.Errorf("out of range")
	}
	return t, nil
}

func timeParam(q url.Values, name string) (*time.Time, *fieldErr) {
	raw := q.Get(name)
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	t, err := parseISOTime(raw)
	if err != nil {
		return nil, &fieldErr{name, err.Error()}
	}
	return &t, nil
}
