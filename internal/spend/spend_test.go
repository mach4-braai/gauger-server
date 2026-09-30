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

func TestParseRatesRejectsGarbage(t *testing.T) {
	for _, s := range []string{"ubuntu-latest", "=0.1", "x=-1", "x=abc"} {
		if _, err := ParseRates(s); err == nil {
			t.Errorf("ParseRates(%q) should fail", s)
		}
	}
}
