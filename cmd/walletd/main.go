// walletd is the wallet service binary: API HTTP + consumidores, composto
// com Uber Fx. Também expõe o subcomando `migrate` para aplicar/reverter
// migrations manualmente.
package main

import (
	"context"
	"fmt"
	"os"

	"go.uber.org/fx"

	"github.com/chalkan3/backend-challenge-go/internal/bootstrap"
	"github.com/chalkan3/backend-challenge-go/internal/infra/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "migrate":
			runMigrate()
			return
		case "version":
			fmt.Println("walletd dev")
			return
		}
	}
	fx.New(bootstrap.App).Run()
}

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
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: walletd migrate up|down|status")
		os.Exit(2)
	}
}
