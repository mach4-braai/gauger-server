// Package spend prices GitHub-hosted runner minutes.
package spend

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Rate is the per-minute price of a runner label in USD. Standard runners
// are free in public repositories; larger runners are billed everywhere.
type Rate struct {
	PerMinute float64
	Standard  bool
}

// DefaultRates are GitHub's published per-minute rates, checked on
// 2026-10-01 against docs.github.com/en/billing/reference/actions-runner-pricing.
var DefaultRates = map[string]Rate{
	"ubuntu-slim":      {0.002, true},
	"ubuntu-latest":    {0.006, true},
	"ubuntu-26.04":     {0.006, true},
	"ubuntu-24.04":     {0.006, true},
	"ubuntu-22.04":     {0.006, true},
	"ubuntu-26.04-arm": {0.005, true},
	"ubuntu-24.04-arm": {0.005, true},
	"ubuntu-22.04-arm": {0.005, true},
	"windows-latest":   {0.010, true},
	"windows-2025":     {0.010, true},
	"windows-2022":     {0.010, true},
	"windows-11-arm":   {0.010, true},
	"macos-latest":     {0.062, true},
	"macos-26":         {0.062, true},
	"macos-15":         {0.062, true},
	"macos-15-intel":   {0.062, true},
	"macos-14":         {0.062, true},

	"macos-latest-large":  {0.077, false},
	"macos-26-large":      {0.077, false},
	"macos-15-large":      {0.077, false},
	"macos-14-large":      {0.077, false},
	"macos-latest-xlarge": {0.102, false},
	"macos-26-xlarge":     {0.102, false},
	"macos-15-xlarge":     {0.102, false},
	"macos-14-xlarge":     {0.102, false},
}

// Rates maps runner labels to prices.
type Rates map[string]Rate

// ParseRates reads "label=0.012,other=0.022" and returns DefaultRates with
// those labels added or replaced as larger runners.
func ParseRates(s string) (Rates, error) {
	r := Rates{}
	for k, v := range DefaultRates {
		r[k] = v
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		label, price, ok := strings.Cut(part, "=")
		p, err := strconv.ParseFloat(strings.TrimSpace(price), 64)
		if !ok || strings.TrimSpace(label) == "" || err != nil || p < 0 {
			return nil, fmt.Errorf("runner rate %q must look like label=0.012", part)
		}
		r[strings.TrimSpace(label)] = Rate{PerMinute: p}
	}
	return r, nil
}

// Price is the cost of some job minutes on one set of runner labels.
type Price struct {
	Cost float64
	Rate float64
	// Free is set for self-hosted runners and for standard runners in
	// public repositories.
	Free bool
	// Known is false when no label has a rate.
	Known bool
}

// Price looks up the first label with a rate. private is nil when the
// repository's visibility is unknown, which prices it as private.
func (r Rates) Price(labels []string, private *bool, minutes int64) Price {
	if slices.Contains(labels, "self-hosted") {
		return Price{Free: true, Known: true}
	}
	label := r.Label(labels)
	if label == "" {
		return Price{}
	}
	rate := r[label]
	if rate.Standard && private != nil && !*private {
		return Price{Rate: rate.PerMinute, Free: true, Known: true}
	}
	return Price{Cost: float64(minutes) * rate.PerMinute, Rate: rate.PerMinute, Known: true}
}

// smaller names the next smaller runner for each label, from the order
// of sizes per OS: ubuntu-slim < ubuntu-*, and macos-* < macos-*-large <
// macos-*-xlarge on the same image. Windows and arm have one size, and
// labels added with ParseRates have no known order.
var smaller = map[string]string{
	"ubuntu-latest": "ubuntu-slim",
	"ubuntu-26.04":  "ubuntu-slim",
	"ubuntu-24.04":  "ubuntu-slim",
	"ubuntu-22.04":  "ubuntu-slim",

	"macos-latest-large": "macos-latest",
	"macos-26-large":     "macos-26",
	"macos-15-large":     "macos-15",
	"macos-14-large":     "macos-14",

	"macos-latest-xlarge": "macos-latest-large",
	"macos-26-xlarge":     "macos-26-large",
	"macos-15-xlarge":     "macos-15-large",
	"macos-14-xlarge":     "macos-14-large",
}

// Smaller returns the next smaller runner label than label, if there is
// one.
func Smaller(label string) (string, bool) {
	s, ok := smaller[label]
	return s, ok
}

// Label returns the first of labels that has a rate, or "".
func (r Rates) Label(labels []string) string {
	for _, l := range labels {
		if _, ok := r[l]; ok {
			return l
		}
	}
	return ""
}

// Downsize is what running the same minutes on the next smaller runner
// would cost less.
type Downsize struct {
	// Label is the next smaller runner, empty when there is none.
	Label string
	// Saving is in USD and meaningful only when Priced is set. It is not
	// set when the minutes were free, or when there is no smaller runner
	// with a rate.
	Saving float64
	Priced bool
}

// Downsize prices minutes on labels against the same minutes on the next
// smaller label for the first label with a rate.
func (r Rates) Downsize(labels []string, private *bool, minutes int64) Downsize {
	label := r.Label(labels)
	if slices.Contains(labels, "self-hosted") || label == "" {
		return Downsize{}
	}
	small, ok := Smaller(label)
	if !ok {
		return Downsize{}
	}
	d := Downsize{Label: small}
	now := r.Price(labels, private, minutes)
	less := r.Price([]string{small}, private, minutes)
	if now.Known && less.Known && !now.Free {
		d.Saving, d.Priced = now.Cost-less.Cost, true
	}
	return d
}
