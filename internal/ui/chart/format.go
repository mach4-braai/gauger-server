package chart

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Integer formats v rounded, with thousands separators: 12,345.
func Integer(v float64) string {
	return group(strconv.FormatFloat(math.Round(v), 'f', 0, 64))
}

// Compact formats v in at most three digits and a K, M or B suffix:
// 950, 1.2K, 34K, 5.6M.
func Compact(v float64) string {
	abs := math.Abs(v)
	for _, u := range []struct {
		div    float64
		suffix string
	}{{1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if abs >= u.div*0.9995 {
			return short(v/u.div) + u.suffix
		}
	}
	if v == math.Trunc(v) || abs >= 100 {
		return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	}
	return short(v)
}

// short keeps one decimal below 10 and none above.
func short(v float64) string {
	if math.Abs(v) < 9.95 {
		return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0")
	}
	return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
}

// Percent formats a fraction as a percentage with one decimal: 97.5%.
func Percent(v float64) string {
	return strconv.FormatFloat(v*100, 'f', 1, 64) + "%"
}

// Seconds formats a duration in seconds as 42s, 3m 05s or 1h 02m.
func Seconds(s float64) string {
	n := int64(math.Round(s))
	switch {
	case n < 60:
		return fmt.Sprintf("%ds", n)
	case n < 3600:
		return fmt.Sprintf("%dm %02ds", n/60, n%60)
	default:
		return fmt.Sprintf("%dh %02dm", n/3600, n%3600/60)
	}
}

// USD formats dollars with cents: $1,234.50. Zero is $0.
func USD(v float64) string {
	if v == 0 {
		return "$0"
	}
	s := strconv.FormatFloat(v, 'f', 2, 64)
	whole, cents, _ := strings.Cut(s, ".")
	return "$" + group(whole) + "." + cents
}

func group(digits string) string {
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}
