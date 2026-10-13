// Command walletd is the wallet service binary: HTTP API plus background
// workers, composed with Uber Fx (dependency injection + lifecycle). It also
// exposes the `migrate` subcommand to apply/rollback/status SQL migrations
// manually, outside the application lifecycle.
package main

import (
	"context"
	"fmt"
	"os"

	"go.uber.org/fx"

	"github.com/chalkan3/backend-challenge-go/internal/bootstrap"
	"github.com/chalkan3/backend-challenge-go/internal/platform/buildinfo"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
)

func main() {
	// Deliberate data race, off unless WALLET_RACE_CANARY=1. It exists so the
	// race-instrumented verification can prove the detector is actually
	// watching before it trusts a silent run. See race_canary.go.
	raceCanary()

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			runMigrate()
			return
		case "version":
			// Prints the identity the binary was linked with (see
			// internal/platform/buildinfo). Dependency-free on purpose: it must
			// answer without a database or any configuration, which is what
			// makes it usable as an ops probe from a bare container.
			fmt.Println(buildinfo.String())
			return
		}
	}
	fx.New(bootstrap.App).Run()
}

// runMigrate executes the `migrate up|down|status` subcommand. It loads the
// configuration only (no Fx graph) so it can run from CI against a fresh
// database.
func runMigrate() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sub := "up"
	if len(os.Args) > 2 {
		sub = os.Args[2]
	}
	ctx := context.Background()
	switch sub {
	case "up":
		if err := postgres.MigrateUp(ctx, cfg.DBDSN); err != nil {
			fmt.Fprintln(os.Stderr, "migrate up:", err)
			os.Exit(1)
		}
		fmt.Println("migrations applied")
	case "down":
		if err := postgres.MigrateDown(ctx, cfg.DBDSN); err != nil {
			fmt.Fprintln(os.Stderr, "migrate down:", err)
			os.Exit(1)
		}
		fmt.Println("last migration reverted")
	case "status":
		if err := postgres.MigrateStatus(ctx, cfg.DBDSN); err != nil {
			fmt.Fprintln(os.Stderr, "migrate status:", err)
			os.Exit(2)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: walletd migrate up|down|status")
		os.Exit(2)
	}
}
