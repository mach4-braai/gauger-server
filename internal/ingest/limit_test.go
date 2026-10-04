package ingest

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func TestLimiterForgetsKeysOnceTheyRefill(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := FailedAuthLimiter()
	l.now = func() time.Time { return now }

	for i := range 100_000 {
		l.Take("10.0." + strconv.Itoa(i))
	}
	if len(l.keys) != 100_000 {
		t.Fatalf("keys = %d, want 100000 while active", len(l.keys))
	}

	now = now.Add(l.refill / 2)
	for range 20 {
		l.Take("busy")
	}
	now = now.Add(l.refill / 2)
	l.Take("new")
	if _, ok := l.keys["busy"]; !ok || len(l.keys) != 2 {
		t.Fatalf("keys = %d, want only the busy and the new one", len(l.keys))
	}
}

func TestLimiterTakeRefusesWithoutSpending(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLimiter(time.Second, 2)
	l.now = func() time.Time { return now }

	if l.Take("k") != 0 || l.Take("k") != 0 {
		t.Fatal("the burst should be allowed")
	}
	if d := l.Take("k"); d != time.Second {
		t.Fatalf("Take over the burst = %v, want 1s", d)
	}
	now = now.Add(time.Second)
	if d := l.Take("k"); d != 0 {
		t.Fatalf("a refused Take spent a token: next Take waits %v", d)
	}
}

func TestLimiterRefundGivesTheTokenBack(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := NewLimiter(time.Minute, 1)
	l.now = func() time.Time { return now }

	_, refund := l.Reserve("k")
	if d, _ := l.Reserve("k"); d == 0 {
		t.Fatal("a held reservation should leave no token")
	}
	now = now.Add(time.Second)
	refund()
	if d, _ := l.Reserve("k"); d != 0 {
		t.Fatalf("after a refund Reserve waits %v, want 0", d)
	}
}

func TestClientKeyIsTheIPv4AddressOrIPv6Slash64(t *testing.T) {
	key := func(remote string, funnelSrc string) string {
		r := httptest.NewRequest(http.MethodPost, "/v1/metrics", nil)
		r.RemoteAddr = remote
		if funnelSrc != "" {
			r = r.WithContext(WithClientAddr(r.Context(), netip.MustParseAddr(funnelSrc)))
		}
		return addrKey(clientAddr(r))
	}
	for _, tc := range []struct{ a, b string }{
		{key("[2001:db8:1:2::1]:1000", ""), key("[2001:db8:1:2:ffff::9]:2000", "")},
		{key("[::ffff:192.0.2.1]:1000", ""), key("192.0.2.1:2000", "")},
		{key("100.100.100.100:443", "198.51.100.7"), key("100.64.0.1:1", "198.51.100.7")},
	} {
		if tc.a != tc.b {
			t.Errorf("keys %q and %q should be the same client", tc.a, tc.b)
		}
	}
	if a, b := key("100.100.100.100:443", "198.51.100.7"), key("100.100.100.100:443", "198.51.100.8"); a == b {
		t.Errorf("two clients behind one Funnel relay share key %q", a)
	}
	if a, b := key("[2001:db8:1:2::1]:1", ""), key("[2001:db8:1:3::1]:1", ""); a == b {
		t.Errorf("two IPv6 /64s share key %q", a)
	}
}
