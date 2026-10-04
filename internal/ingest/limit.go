package ingest

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// JobLimiter allows each job a request a second on average. The burst of
// 120 covers gauger replaying 10 minutes of 5-second batches back to back
// after an outage.
func JobLimiter() *Limiter { return NewLimiter(time.Second, 120) }

// FailedAuthLimiter allows each client address 20 failed authentications,
// then one every 6 seconds.
func FailedAuthLimiter() *Limiter { return NewLimiter(6*time.Second, 20) }

// Limiter keeps a token bucket per key. A bucket that has refilled is the
// same as a new one, so it is dropped, and memory follows the keys active
// within the last two refill periods.
type Limiter struct {
	every  time.Duration
	burst  float64
	refill time.Duration
	now    func() time.Time

	mu    sync.Mutex
	keys  map[string]*bucket
	swept time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// NewLimiter adds a token every every, up to burst.
func NewLimiter(every time.Duration, burst int) *Limiter {
	return &Limiter{
		every:  every,
		burst:  float64(burst),
		refill: every * time.Duration(burst),
		now:    time.Now,
		keys:   map[string]*bucket{},
	}
}

// Take spends a token for key. With none left it spends nothing and returns
// how long until one is available.
func (l *Limiter) Take(key string) time.Duration {
	wait, _ := l.Reserve(key)
	return wait
}

// Reserve spends a token for key and returns a func that gives it back.
// With none left it spends nothing and returns how long until one is
// available.
func (l *Limiter) Reserve(key string) (wait time.Duration, refund func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	b, ok := l.keys[key]
	if !ok {
		b = &bucket{tokens: l.burst, at: now}
		l.keys[key] = b
	}
	l.fill(b, now)
	if b.tokens < 1 {
		return l.until(b), nil
	}
	b.tokens--
	return 0, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.fill(b, l.now())
		b.tokens = min(b.tokens+1, l.burst)
	}
}

// Wait returns how long until key has a token, without spending one.
func (l *Limiter) Wait(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	b, ok := l.keys[key]
	if !ok {
		return 0
	}
	l.fill(b, now)
	return l.until(b)
}

func (l *Limiter) fill(b *bucket, now time.Time) {
	if now.After(b.at) {
		b.tokens = min(b.tokens+float64(now.Sub(b.at))/float64(l.every), l.burst)
		b.at = now
	}
}

func (l *Limiter) until(b *bucket) time.Duration {
	if b.tokens >= 1 {
		return 0
	}
	return time.Duration((1 - b.tokens) * float64(l.every))
}

// sweep drops refilled buckets once per refill period. It copies the rest
// into a new map, because a Go map keeps its size after deletes.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < l.refill {
		return
	}
	l.swept = now
	keep := map[string]*bucket{}
	for k, b := range l.keys {
		l.fill(b, now)
		if b.tokens < l.burst {
			keep[k] = b
		}
	}
	l.keys = keep
}

type clientAddrKey struct{}

// WithClientAddr records the address of the client behind a relayed
// connection, such as a Funnel connection's source.
func WithClientAddr(ctx context.Context, addr netip.Addr) context.Context {
	return context.WithValue(ctx, clientAddrKey{}, addr)
}

// clientAddr is the client's IP address: the one WithClientAddr recorded,
// or else the TCP peer's.
func clientAddr(r *http.Request) netip.Addr {
	if a, ok := r.Context().Value(clientAddrKey{}).(netip.Addr); ok {
		return a.Unmap()
	}
	ap, _ := netip.ParseAddrPort(r.RemoteAddr)
	return ap.Addr().Unmap()
}

// addrKey is the limiter key for a client. An IPv6 client usually holds a
// whole /64, so the key is that prefix.
func addrKey(a netip.Addr) string {
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}
