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
	for _, l := range labels {
		rate, ok := r[l]
		if !ok {
			continue
		}
		if rate.Standard && private != nil && !*private {
			return Price{Rate: rate.PerMinute, Free: true, Known: true}
		}
		return Price{Cost: float64(minutes) * rate.PerMinute, Rate: rate.PerMinute, Known: true}
	}
	return Price{}
}
