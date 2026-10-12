package config

import "fmt"

// Mode selects which parts of the single binary run in this process.
//
// The service is one program with three modes rather than three programs.
// Splitting a process only pays off when the parts need different scaling
// dimensions, and here they do not: the SQS consumer's parallelism is already
// per-process (WALLET_CONSUMER_CONCURRENCY), so running more replicas of a
// "workers" binary and running more replicas of a combined binary yield the
// same throughput. What actually differs between the two roles is *resource
// isolation*: the shared pgx pool (WALLET_DB_MAX_CONNS) lets a queue backlog
// starve API connections in the same process. Modes solve exactly that,
// without a second binary, a second image or a second healthcheck surface.
type Mode string

const (
	// ModeAll runs the HTTP API plus every background worker. It is the
	// default and the topology used by the challenge's `docker compose up`.
	ModeAll Mode = "all"

	// ModeAPI serves HTTP only. No worker starts, so the pool belongs
	// entirely to request handling and the process can scale on request
	// latency alone.
	ModeAPI Mode = "api"

	// ModeWorkers runs only the background workers (outbox relay, pending
	// reference resolver, SQS consumer). The HTTP server still starts, but
	// only to expose /health/* and /metrics for probes and scraping — no
	// business route is registered.
	ModeWorkers Mode = "workers"
)

// Valid reports whether m is one of the known modes.
func (m Mode) Valid() bool {
	switch m {
	case ModeAll, ModeAPI, ModeWorkers:
		return true
	}
	return false
}

// RunsHTTP reports whether the HTTP server starts in this mode.
func (m Mode) RunsHTTP() bool { return m == ModeAll || m == ModeAPI || m == ModeWorkers }

// ServesBusinessRoutes reports whether the authenticated business routes are
// registered. A workers-only process answers probes and metrics, nothing else.
func (m Mode) ServesBusinessRoutes() bool { return m == ModeAll || m == ModeAPI }

// RunsWorkers reports whether the background workers start. WorkersEnabled
// keeps its independent meaning (a kill switch for a combined deployment), so
// both must agree before a worker starts.
func (m Mode) RunsWorkers(enabled bool) bool {
	switch m {
	case ModeAPI:
		return false
	case ModeWorkers:
		return true
	}
	return enabled
}

func (m Mode) String() string { return string(m) }

// parseMode reads WALLET_MODE. An empty value is ModeAll so existing
// deployments keep working untouched.
func parseMode(raw string) (Mode, error) {
	if raw == "" {
		return ModeAll, nil
	}
	m := Mode(raw)
	if !m.Valid() {
		return "", fmt.Errorf("WALLET_MODE must be all|api|workers (got %q)", raw)
	}
	return m, nil
}
