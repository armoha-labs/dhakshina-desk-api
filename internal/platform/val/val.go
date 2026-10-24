// Package val has small helpers for loosely typed values: JSON numbers from
// requests and the int32/int64/double mix MongoDB hands back.
package val

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Int returns v as an int64 when it is a whole number.
func Int(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		if x == math.Trunc(x) && math.Abs(x) <= 1<<53 {
			return int64(x), true
		}
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, true
		}
		if f, err := x.Float64(); err == nil && f == math.Trunc(f) && math.Abs(f) <= 1<<53 {
			return int64(f), true
		}
	}
	return 0, false
}

// IntOr returns v as an int64, or def.
func IntOr(v any, def int64) int64 {
	if i, ok := Int(v); ok {
		return i
	}
	return def
}

// Paise reports a positive whole number of paise.
func Paise(v any) (int64, bool) {
	i, ok := Int(v)
	return i, ok && i > 0 && i <= 1<<53
}

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Date returns v when it is a YYYY-MM-DD string.
func Date(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok && dateRE.MatchString(s)
}

// Text returns a trimmed string cut to max runes, and whether v was a string.
func Text(v any, max int) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s, true
}

// Str returns v as a string, or "".
func Str(v any) string {
	s, _ := v.(string)
	return s
}

// Bool returns v as a bool, or false.
func Bool(v any) bool {
	b, _ := v.(bool)
	return b
}

// Time returns a stored date as time.Time.
func Time(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x, true
	case bson.DateTime:
		return x.Time(), true
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ISO formats like JavaScript's toISOString: UTC with milliseconds.
func ISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Day formats a UTC calendar date.
func Day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// M returns v as a document, or nil.
func M(v any) bson.M {
	switch x := v.(type) {
	case bson.M:
		return x
	case map[string]any:
		return x
	case bson.D:
		out := bson.M{}
		for _, e := range x {
			out[e.Key] = e.Value
		}
		return out
	}
	return nil
}

// IST is India Standard Time, used for due dates and quiet hours.
var IST = time.FixedZone("IST", 5*3600+1800)
