//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/chalkan3/backend-challenge-go/internal/bootstrap"
)

// Infrastructure-failure tests (challenge section 13: "recuperação de falhas",
// "falha entre as etapas do processamento").
//
// The service must not degrade silently when a dependency disappears: it has
// to FAIL FAST at startup when Postgres or SQS cannot be reached, and its
// readiness probe must report NOT READY when a dependency becomes
// unreachable at runtime, so the orchestrator withholds traffic instead of
// serving 500s to providers.
//
// These tests never touch the shared stack: they start their OWN Fx instance
// pointed at a controllable TCP proxy in front of the real dependencies (or at
// a dead endpoint), so the main app-1 container keeps running untouched.

// --- a minimal, controllable TCP proxy -------------------------------------

// tcpProxy forwards to a target address and can be "broken" on demand so a
// dependency becomes unreachable without stopping the real container.
type tcpProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	broken bool
	conns  []net.Conn
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &tcpProxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close(); p.breakAll() })
	return p
}

// addr is the proxy's own address, suitable for use as a DSN/endpoint host.
func (p *tcpProxy) addr() string { return p.ln.Addr().String() }

func (p *tcpProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.broken {
			p.mu.Unlock()
			_ = c.Close() // refuse while "broken"
			continue
		}
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		go p.pipe(c)
	}
}

func (p *tcpProxy) pipe(client net.Conn) {
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		done <- struct{}{}
	}
	go cp(upstream, client)
	go cp(client, upstream)
	<-done
}

// breakAll makes the proxy refuse new connections AND tears down the live
// ones, which is what a real dependency outage looks like to the client.
func (p *tcpProxy) breakAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.broken = true
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// --- helpers ----------------------------------------------------------------

// hostPort extracts "host:port" from a URL-ish string ("http://host:port/x").
func hostPort(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// startInstanceExpectingFailure builds and starts an Fx instance and returns
// the start error. It is used to prove the service fails fast; a nil error
// means the instance started anyway (which is the bug we are guarding
// against) and the caller fails the test.
func startInstanceExpectingFailure(t *testing.T, env map[string]string) error {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	a := fx.New(bootstrap.App)
	// A graph validation error is itself a legitimate fail-fast outcome.
	if err := a.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := a.Start(ctx)
	// Always stop: Start may have partially succeeded before failing.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer stopCancel()
	_ = a.Stop(stopCtx)
	return err
}

// TestStartupFailsFastWhenPostgresIsUnreachable points the service at a
// closed port for PostgreSQL: the pool ping at startup must fail, so no
// process ever runs against a database it cannot reach.
func TestStartupFailsFastWhenPostgresIsUnreachable(t *testing.T) {
	dead := "127.0.0.1:1" // nothing listens here
	dsn := fmt.Sprintf("postgres://wallet:wallet@%s/wallet?sslmode=disable", dead)

	err := startInstanceExpectingFailure(t, map[string]string{
		"WALLET_HTTP_ADDR":        freePort(t),
		"WALLET_DB_DSN":           dsn,
		"WALLET_SQS_ENDPOINT":     sqsEndpoint(t),
		"WALLET_MIGRATE_ON_START": "false",
		"WALLET_WORKERS_ENABLED":  "false",
	})
	if err == nil {
		t.Fatal("service started with an unreachable PostgreSQL: it must fail fast")
	}
}

// TestStartupFailsFastWhenSQSIsUnreachable keeps a healthy PostgreSQL but
// points SQS at a closed port: resolving the queue URLs at startup must fail,
// so a worker never silently consumes nothing.
func TestStartupFailsFastWhenSQSIsUnreachable(t *testing.T) {
	err := startInstanceExpectingFailure(t, map[string]string{
		"WALLET_HTTP_ADDR":        freePort(t),
		"WALLET_DB_DSN":           dbDSN(t),
		"WALLET_SQS_ENDPOINT":     "http://127.0.0.1:1",
		"WALLET_MIGRATE_ON_START": "false",
		"WALLET_WORKERS_ENABLED":  "false",
	})
	if err == nil {
		t.Fatal("service started with an unreachable SQS: it must fail fast")
	}
}

// TestReadinessReflectsDependencyOutage starts an instance whose SQS endpoint
// is a proxy in front of the real LocalStack, then breaks the proxy. The
// process must stay up (a transient dependency outage is not a crash) but
// /health/ready must answer 503 so the orchestrator stops sending it traffic,
// while /health/live keeps answering 200 (the process itself is alive).
func TestReadinessReflectsDependencyOutage(t *testing.T) {
	proxy := newTCPProxy(t, hostPort(sqsEndpoint(t)))
	// The app talks plain HTTP to the SQS endpoint; the proxy is a raw TCP
	// forwarder, so the scheme stays http:// with the proxy's address.
	endpoint := "http://" + proxy.addr()

	addr := freePort(t)
	for k, v := range map[string]string{
		"WALLET_HTTP_ADDR":        addr,
		"WALLET_DB_DSN":           dbDSN(t),
		"WALLET_SQS_ENDPOINT":     endpoint,
		"WALLET_AUTH_ISSUER":      keycloakURL(t) + "/realms/" + realm,
		"WALLET_AUTH_JWKS_URL":    "",
		"WALLET_MIGRATE_ON_START": "false",
		"WALLET_WORKERS_ENABLED":  "false",
	} {
		t.Setenv(k, v)
	}

	a := fx.New(bootstrap.App)
	if err := a.Err(); err != nil {
		t.Fatalf("fx graph invalid: %v", err)
	}
	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.Start(startCtx); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = a.Stop(stopCtx)
	})

	base := "http://" + addr
	// Healthy start: readiness is 200 while the proxy forwards.
	pollUntil(t, 10*time.Second, func() bool {
		return probeStatus(base+"/health/ready") == 200
	}, "readiness never became 200 with a healthy SQS")

	// Now the dependency disappears.
	proxy.breakAll()

	pollUntil(t, 15*time.Second, func() bool {
		return probeStatus(base+"/health/ready") == 503
	}, "readiness did not degrade to 503 after the SQS outage")

	if s := probeStatus(base + "/health/live"); s != 200 {
		t.Fatalf("liveness = %d, want 200: the process is still alive during a dependency outage", s)
	}
}

// probeStatus performs a GET and returns the status code (0 on transport
// error), used to poll readiness/liveness without failing on connection
// hiccups.
func probeStatus(url string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
