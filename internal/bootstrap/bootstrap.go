// Package bootstrap composes the application with Uber Fx: configuration,
// connections, repositories, use cases, transports and workers, all wired by
// constructors and organized as fx.Modules (challenge section 4).
package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/chalkan3/backend-challenge-go/internal/auth"
	"github.com/chalkan3/backend-challenge-go/internal/messaging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/config"
	"github.com/chalkan3/backend-challenge-go/internal/platform/logging"
	"github.com/chalkan3/backend-challenge-go/internal/platform/metrics"
	"github.com/chalkan3/backend-challenge-go/internal/postgres"
	"github.com/chalkan3/backend-challenge-go/internal/service"
	"github.com/chalkan3/backend-challenge-go/internal/sqs"
	wallethttp "github.com/chalkan3/backend-challenge-go/internal/transport/http"
)

// NewHTTPDeps assembles the transport dependencies.
func NewHTTPDeps(
	cfg *config.Config, log *slog.Logger, a *auth.Authenticator,
	ws *service.WalletService, wg *service.WagerService, m *metrics.Metrics,
	pool *pgxpool.Pool, client *awssqs.Client, urls sqs.QueueURLs,
) wallethttp.Deps {
	return wallethttp.Deps{
		Cfg: cfg, Log: log.With(slog.String("component", "http")), Auth: a,
		Ep: wallethttp.NewEndpoints(ws, wg, m), Pool: pool, SQS: client, URLs: urls, Reg: m.Registry(),
	}
}

// newWalletService adapts the functional-options constructor to Fx: the
// composition root reads configuration once here, and the use case only
// receives plain values (dependency injection at the boundary).
func newWalletService(
	txm service.TxManager, log *slog.Logger, hook service.Instrumentation, cfg *config.Config,
) *service.WalletService {
	return service.NewWalletService(txm, log, hook, service.WithLedgerPageSize(cfg.LedgerPageMax))
}

// newWagerService adapts the functional-options constructor to Fx.
func newWagerService(
	txm service.TxManager, log *slog.Logger, hook service.Instrumentation, cfg *config.Config,
) *service.WagerService {
	return service.NewWagerService(txm, log, hook,
		service.WithReferencePolicy(cfg.ReferenceMaxAttempts, cfg.ReferenceTTL))
}

// App is the full Fx application option set. Lifecycle rules:
//   - start: migrations → HTTP server → workers;
//   - stop (reverse): workers drain → HTTP server stops accepting → pool
//     closes last, after every consumer released it.
var App = fx.Options(
	fx.Module("platform",
		fx.Provide(func() context.Context { return context.Background() }),
		fx.Provide(config.Load),
		fx.Provide(logging.New),
		fx.Provide(metrics.New),
		fx.Provide(
			fx.Annotate(newInstrumentation, fx.As(new(service.Instrumentation))),
			fx.Annotate(newInstrumentation, fx.As(new(messaging.Instrumentation))),
			fx.Annotate(newInstrumentation, fx.As(new(messaging.ConsumerMetrics))),
		),
	),
	fx.Module("auth", fx.Provide(auth.NewAuthenticator)),
	fx.Module("postgres",
		fx.Provide(postgres.NewPool),
		fx.Provide(fx.Annotate(postgres.NewStore, fx.As(new(service.TxManager)))),
	),
	fx.Module("sqs",
		fx.Provide(sqs.NewClient),
		fx.Provide(sqs.ResolveQueueURLs),
		fx.Provide(sqs.NewPublisher),
	),
	fx.Module("usecases",
		fx.Provide(newWalletService),
		fx.Provide(newWagerService),
	),
	fx.Module("http",
		fx.Provide(NewHTTPDeps),
		fx.Provide(wallethttp.NewServer),
	),
	fx.Module("workers",
		fx.Provide(messaging.NewOutboxWorker),
		fx.Provide(messaging.NewReferenceWorker),
		fx.Provide(messaging.NewConsumer),
	),
	// 1st invoke: pool closes LAST (OnStop runs in reverse append order).
	fx.Invoke(registerPoolClose),
	// 2nd invoke: migrations, then HTTP server.
	fx.Invoke(registerMigrations),
	fx.Invoke(registerHTTP),
	// 3rd invoke: workers start last and stop first.
	fx.Invoke(registerWorkers),
)

func registerPoolClose(lc fx.Lifecycle, pool *pgxpool.Pool, log *slog.Logger) {
	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			log.Info("closing postgres pool")
			pool.Close()
			return nil
		},
	})
}

func registerMigrations(lc fx.Lifecycle, cfg *config.Config, log *slog.Logger) {
	if !cfg.MigrateOnStart {
		return
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			log.Info("applying database migrations")
			return postgres.MigrateUp(ctx, cfg.DBDSN)
		},
		OnStop: func(context.Context) error { return nil },
	})
}

func registerHTTP(lc fx.Lifecycle, cfg *config.Config, log *slog.Logger, srv *http.Server) {
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return err
			}
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("http server failed", slog.String("error", err.Error()))
				}
			}()
			log.Info("http server listening", slog.String("addr", srv.Addr))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
			defer cancel()
			log.Info("http server draining")
			return srv.Shutdown(sctx)
		},
	})
}

func registerWorkers(lc fx.Lifecycle, cfg *config.Config, log *slog.Logger,
	ow *messaging.OutboxWorker, rw *messaging.ReferenceWorker, cons *messaging.Consumer,
) {
	if !cfg.WorkersEnabled {
		log.Info("workers disabled by configuration")
		return
	}
	var cancel context.CancelFunc
	var wg sync.WaitGroup
	done := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			wctx, c := context.WithCancel(context.Background())
			cancel = c
			run := func(name string, f func(context.Context) error) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer c() // one worker crashing stops the others promptly
					if err := f(wctx); err != nil {
						log.Error("worker exited with error", slog.String("worker", name), slog.String("error", err.Error()))
					}
				}()
			}
			run("outbox", ow.Run)
			run("references", rw.Run)
			run("consumer", cons.Run)
			go func() { wg.Wait(); close(done) }()
			return nil
		},
		OnStop: func(context.Context) error {
			if cancel != nil {
				cancel()
			}
			select {
			case <-done:
				log.Info("all workers stopped")
				return nil
			case <-time.After(cfg.ShutdownGrace):
				log.Warn("workers did not acknowledge shutdown within grace")
				return nil // visibility timeout + inbox guarantee safe redelivery
			}
		},
	})
}
