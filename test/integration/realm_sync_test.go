// Package integration — but this file is NOT integration-tagged on purpose:
// it is a repository-consistency check that must run in the fast unit suite.
//
// Configuration that exists in two places drifts. Two files in this repo are
// embedded copies: the Keycloak realm and the LocalStack init script live under
// deploy/ for docker-compose, and a byte-for-byte copy of each is embedded in
// the Helm chart. A drifted copy is worse than no copy at all: the chart would
// silently import a realm without its policies, or provision queues without
// their redrive policy, while the compose stack keeps the tested behaviour.
//
// There was no sync script, and both copies HAD already diverged when the
// realm/queue policies were added. These tests make the divergence impossible
// to merge, for the price of a cheap comparison.
package integration

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestKeycloakRealmCopiesStayInSync(t *testing.T) {
	t.Parallel()

	const (
		composePath = "../../deploy/keycloak/realm-wallet.json"
		chartPath   = "../../deploy/kubernetes/charts/wallet/files/keycloak/realm-wallet.json"
	)

	load := func(path string) map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		return out
	}

	if !reflect.DeepEqual(load(composePath), load(chartPath)) {
		t.Fatalf("the realm copies diverged.\n"+
			"  compose: %s\n"+
			"  chart:   %s\n"+
			"Re-sync with: cp %s %s",
			composePath, chartPath, composePath, chartPath)
	}
}

func TestLocalstackInitCopiesStayInSync(t *testing.T) {
	t.Parallel()

	const (
		composePath = "../../deploy/localstack/init/10-queues.py"
		chartPath   = "../../deploy/kubernetes/charts/wallet/files/localstack/init/10-queues.py"
	)

	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}
	chart, err := os.ReadFile(chartPath)
	if err != nil {
		t.Fatalf("read %s: %v", chartPath, err)
	}

	if string(compose) != string(chart) {
		t.Fatalf("the LocalStack init copies diverged.\n"+
			"  compose: %s (%d bytes)\n"+
			"  chart:   %s (%d bytes)\n"+
			"Re-sync with: cp %s %s",
			composePath, len(compose), chartPath, len(chart), composePath, chartPath)
	}
}
