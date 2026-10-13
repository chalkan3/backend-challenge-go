// Package buildinfo carries the version, commit and build date that identify
// a running binary.
//
// The values are injected at link time by GoReleaser (or by any
// `go build -ldflags "-X ..."`), so the same source produces a binary that
// knows exactly which revision it is. Without injection — a plain `go build`,
// or `go test` — every field keeps its "dev" zero value, which is the honest
// answer for an unversioned build rather than a plausible-looking lie.
//
// The package has no dependencies and reads nothing at runtime: the linker
// rewrites the string variables below, and the accessors just read them. That
// keeps it usable from main, from config (for the telemetry resource), and in
// tests without any setup.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// These four variables are the injection points. The names are qualified with
// the full import path in the -X flag, e.g.
//
//	-ldflags "-X github.com/chalkan3/backend-challenge-go/internal/platform/buildinfo.Version=v1.2.3"
//
// so they must stay exported-and-untyped (string) for the linker to find them.
var (
	// Version is the semver tag (v1.2.3) or a snapshot name.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
	// Date is the build timestamp, RFC3339.
	Date = "unknown"
)

// String returns a single-line, human-readable identity, e.g.
//
//	walletd v1.2.3 (abc1234, 2025-01-01T00:00:00Z, go1.26.0 linux/amd64)
//
// It is what `walletd version` prints: the point is that one line answers
// "what is running here?" without shelling into the container.
func String() string {
	return fmt.Sprintf("walletd %s (%s, %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// EffectiveVersion returns the injected Version when one was set, and falls
// back to the module version embedded by the Go toolchain otherwise.
//
// This matters for `go install ...@v1.2.3`, which does NOT go through
// GoReleaser: there is no -X injection, but `debug.ReadBuildInfo` still
// reports the module version the toolchain fetched. Without this fallback such
// a binary would claim "dev" despite being a published release. When neither
// source has an answer — a local `go build` from a git checkout, where the
// module version is the empty string or "(devel)" — it returns "dev" and we
// are back to the honest default.
func EffectiveVersion() string {
	return effectiveVersion(Version, moduleVersion)
}

// effectiveVersion is the pure core of EffectiveVersion: injected is the
// linker value, readModule is the module-version lookup. Split out so both
// interesting branches — "injected wins" and "fall back to the module version
// the toolchain embedded" — are testable without a real `go install`.
func effectiveVersion(injected string, readModule func() (string, bool)) string {
	if injected != "dev" {
		return injected
	}
	if v, ok := readModule(); ok && v != "" && v != "(devel)" {
		return v
	}
	return injected
}

// moduleVersion reports the main module version the toolchain embedded, if
// any. `go install ...@v1.2.3` populates it; a local git checkout reports
// "(devel)", which effectiveVersion rejects.
func moduleVersion() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	return info.Main.Version, true
}
