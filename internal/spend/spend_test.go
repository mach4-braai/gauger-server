package spend

import "testing"

func TestPrice(t *testing.T) {
	rates, err := ParseRates("linux-8-core=0.022, ubuntu-latest=0.007")
	if err != nil {
		t.Fatal(err)
	}
	public, private := new(false), new(true)
	for _, tc := range []struct {
		name    string
		labels  []string
		private *bool
		minutes int64
		want    Price
	}{
		{"private standard", []string{"ubuntu-24.04"}, private, 10, Price{Cost: 0.06, Rate: 0.006, Known: true}},
		{"public standard is free", []string{"ubuntu-24.04"}, public, 10, Price{Rate: 0.006, Free: true, Known: true}},
		{"public larger runner is billed", []string{"linux-8-core"}, public, 10, Price{Cost: 0.22, Rate: 0.022, Known: true}},
		{"public macOS large is billed", []string{"macos-15-large"}, public, 2, Price{Cost: 0.154, Rate: 0.077, Known: true}},
		{"unknown visibility is billed", []string{"windows-latest"}, nil, 3, Price{Cost: 0.03, Rate: 0.01, Known: true}},
		{"override turns a label into a larger runner", []string{"ubuntu-latest"}, public, 1, Price{Cost: 0.007, Rate: 0.007, Known: true}},
		{"self-hosted is free", []string{"self-hosted", "linux"}, private, 100, Price{Free: true, Known: true}},
		{"unknown label", []string{"my-runner"}, private, 5, Price{}},
	} {
		got := rates.Price(tc.labels, tc.private, tc.minutes)
		if got.Free != tc.want.Free || got.Known != tc.want.Known || !near(got.Cost, tc.want.Cost) || !near(got.Rate, tc.want.Rate) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestSmaller(t *testing.T) {
	for _, tc := range []struct {
		label, want string
		ok          bool
	}{
		{"ubuntu-latest", "ubuntu-slim", true},
		{"ubuntu-22.04", "ubuntu-slim", true},
		{"ubuntu-slim", "", false},
		{"ubuntu-24.04-arm", "", false},
		{"windows-latest", "", false},
		{"macos-15", "", false},
		{"macos-15-large", "macos-15", true},
		{"macos-latest-xlarge", "macos-latest-large", true},
		{"linux-8-core", "", false},
		{"", "", false},
	} {
		got, ok := Smaller(tc.label)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Smaller(%q) = %q, %v; want %q, %v", tc.label, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSmallerLabelsAreRatedAndCheaper(t *testing.T) {
	for label, small := range smaller {
		big, ok := DefaultRates[label]
		if !ok {
			t.Errorf("%s has no rate", label)
		}
		little, ok := DefaultRates[small]
		if !ok {
			t.Errorf("%s has no rate", small)
		}
		if little.PerMinute >= big.PerMinute {
			t.Errorf("%s (%v) is not cheaper than %s (%v)", small, little.PerMinute, label, big.PerMinute)
		}
	}
}

func TestDownsize(t *testing.T) {
	public, private := new(false), new(true)
	for _, tc := range []struct {
		name    string
		labels  []string
		private *bool
		minutes int64
		want    Downsize
	}{
		{"private standard", []string{"ubuntu-latest"}, private, 10, Downsize{Label: "ubuntu-slim", Saving: 0.04, Priced: true}},
		{"unknown visibility is billed", []string{"ubuntu-latest"}, nil, 10, Downsize{Label: "ubuntu-slim", Saving: 0.04, Priced: true}},
		{"public standard is free", []string{"ubuntu-latest"}, public, 10, Downsize{Label: "ubuntu-slim"}},
		{"public larger runner saves all of its cost", []string{"macos-15-large"}, public, 10, Downsize{Label: "macos-15", Saving: 0.77, Priced: true}},
		{"no smaller label", []string{"windows-latest"}, private, 10, Downsize{}},
		{"self-hosted", []string{"self-hosted", "ubuntu-latest"}, private, 10, Downsize{}},
		{"unknown label", []string{"my-runner"}, private, 10, Downsize{}},
	} {
		got := Rates(DefaultRates).Downsize(tc.labels, tc.private, tc.minutes)
		if got.Label != tc.want.Label || got.Priced != tc.want.Priced || !near(got.Saving, tc.want.Saving) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestParseRatesRejectsGarbage(t *testing.T) {
	for _, s := range []string{"ubuntu-latest", "=0.1", "x=-1", "x=abc"} {
		if _, err := ParseRates(s); err == nil {
			t.Errorf("ParseRates(%q) should fail", s)
		}
	}
}
