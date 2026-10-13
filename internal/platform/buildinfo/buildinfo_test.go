package buildinfo

import (
	"runtime"
	"strings"
	"testing"
)

// TestStringFormat pins the single line `walletd version` prints. The exact
// runtime suffix varies by machine, so the assertion is on the prefix and the
// presence of each field — not on a hardcoded go version.
func TestStringFormat(t *testing.T) {
	got := String()
	if !strings.HasPrefix(got, "walletd ") {
		t.Fatalf("String() must start with the binary name, got %q", got)
	}
	for _, want := range []string{Version, Commit, Date, runtime.Version()} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
	if !strings.Contains(got, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("String() = %q, missing platform %s/%s", got, runtime.GOOS, runtime.GOARCH)
	}
}

// TestEffectiveVersionDefaultsToDev documents the honest-default contract: a
// build where the linker injected nothing reports "dev", not some plausible
// but wrong tag. This is the case `go test` itself runs under.
func TestEffectiveVersionDefaultsToDev(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "dev"
	// The test binary is built from a source checkout, so debug.ReadBuildInfo
	// reports "(devel)" (or nothing usable) and the fallback must not invent a
	// version from it.
	if got := EffectiveVersion(); got != "dev" {
		t.Fatalf("EffectiveVersion() = %q, want %q for an uninjected build", got, "dev")
	}
}

// TestEffectiveVersionPrefersInjected proves the injected value wins over any
// value the toolchain embedded — the release path (GoReleaser -X) must not be
// overridden by module metadata.
func TestEffectiveVersionPrefersInjected(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "v9.9.9"
	if got := EffectiveVersion(); got != "v9.9.9" {
		t.Fatalf("EffectiveVersion() = %q, want the injected %q", got, "v9.9.9")
	}
}

// TestEffectiveVersionBranches table-drives the pure core, covering the two
// fallback branches that a local build can never reach for real: a module
// version embedded by `go install` (accepted) and "(devel)"/"" (rejected).
func TestEffectiveVersionBranches(t *testing.T) {
	tests := []struct {
		name     string
		injected string
		module   string
		ok       bool
		want     string
	}{
		{"injected wins even when module is set", "v1.0.0", "v2.0.0", true, "v1.0.0"},
		{"falls back to the embedded module version", "dev", "v1.5.0", true, "v1.5.0"},
		{"(devel) is not a version", "dev", "(devel)", true, "dev"},
		{"empty module version is not a version", "dev", "", true, "dev"},
		{"no build info at all", "dev", "", false, "dev"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveVersion(tc.injected, func() (string, bool) { return tc.module, tc.ok })
			if got != tc.want {
				t.Fatalf("effectiveVersion(%q, module=%q, ok=%v) = %q, want %q",
					tc.injected, tc.module, tc.ok, got, tc.want)
			}
		})
	}
}
