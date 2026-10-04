package tailnet

import (
	"crypto/tls"
	"net"
	"net/netip"
	"testing"

	"tailscale.com/ipn"
)

func TestFunnelSourceIsTheClientNotTheRelay(t *testing.T) {
	relay, _ := net.Pipe()
	src := netip.MustParseAddrPort("203.0.113.7:51234")
	funnel := &ipn.FunnelConn{Conn: relay, Src: src}

	if got, ok := FunnelSource(tls.Server(funnel, &tls.Config{})); !ok || got != src.Addr() {
		t.Fatalf("FunnelSource(TLS over Funnel) = %v, %v; want %v", got, ok, src.Addr())
	}
	if _, ok := FunnelSource(tls.Server(relay, &tls.Config{})); ok {
		t.Fatal("a tailnet connection has no Funnel source")
	}
}
