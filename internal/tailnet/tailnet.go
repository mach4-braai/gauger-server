// Package tailnet joins the tailnet with tsnet and opens gauger-server's
// four listeners.
package tailnet

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

type Node struct {
	srv *tsnet.Server
	lc  *local.Client

	// Runner is tailnet only and serves OTLP and job lifecycle on :4318.
	Runner net.Listener
	// FunnelRunner is Funnel only and serves OTLP and job lifecycle on
	// :10000.
	FunnelRunner net.Listener
	// UI is tailnet only and serves the web UI with TLS on :443.
	UI net.Listener
	// Webhook is Funnel only and serves GitHub webhooks on :8443.
	Webhook net.Listener
	// DNSName is the node's MagicDNS name, such as gauger-server.example.ts.net.
	DNSName string
}

// Join starts tsnet with its state in dir. authKey is only needed the first
// time; after that tsnet logs in with the node key it kept in dir.
func Join(ctx context.Context, dir, hostname, authKey string) (*Node, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tsnet state dir: %w", err)
	}
	srv := &tsnet.Server{
		Dir:      dir,
		Hostname: hostname,
		AuthKey:  authKey,
		UserLogf: func(format string, args ...any) { slog.Info(fmt.Sprintf(format, args...)) },
	}
	n := &Node{srv: srv}
	if err := n.start(ctx); err != nil {
		srv.Close()
		return nil, err
	}
	return n, nil
}

func (n *Node) start(ctx context.Context) error {
	if _, err := n.srv.Up(ctx); err != nil {
		return fmt.Errorf("join tailnet: %w", err)
	}
	domains := n.srv.CertDomains()
	if len(domains) == 0 {
		return fmt.Errorf("tailnet has no HTTPS certificate domain; enable MagicDNS and HTTPS certificates")
	}
	n.DNSName = domains[0]

	lc, err := n.srv.LocalClient()
	if err != nil {
		return err
	}
	n.lc = lc
	if n.Runner, err = n.srv.Listen("tcp", ":4318"); err != nil {
		return fmt.Errorf("listen :4318: %w", err)
	}
	if n.UI, err = n.srv.ListenTLS("tcp", ":443"); err != nil {
		return fmt.Errorf("listen :443: %w", err)
	}
	if n.Webhook, err = n.srv.ListenFunnel("tcp", ":8443", tsnet.FunnelOnly()); err != nil {
		return fmt.Errorf("listen funnel :8443: %w", err)
	}
	if n.FunnelRunner, err = n.srv.ListenFunnel("tcp", ":10000", tsnet.FunnelOnly()); err != nil {
		return fmt.Errorf("listen funnel :10000: %w", err)
	}
	return nil
}

// RunnerURL is the public base URL gauger sends runner data to.
func (n *Node) RunnerURL() string { return "https://" + n.DNSName + ":10000" }

func (n *Node) Close() error { return n.srv.Close() }

// HasTag reports whether the tailnet peer at remoteAddr carries tag.
func (n *Node) HasTag(ctx context.Context, remoteAddr, tag string) (bool, error) {
	who, err := n.lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return false, err
	}
	return slices.Contains(who.Node.Tags, tag), nil
}

// FunnelSource returns the address of the client behind a Funnel
// connection. The TCP peer of such a connection is the Funnel relay.
func FunnelSource(c net.Conn) (netip.Addr, bool) {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	fc, ok := c.(*ipn.FunnelConn)
	if !ok {
		return netip.Addr{}, false
	}
	return fc.Src.Addr(), true
}
