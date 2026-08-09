// Command hivenet serves the canvas, the API and the reconcile engine from a
// single binary with the frontend embedded (spec §2).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bitcrafttech/hivenet/internal/auth"
	"github.com/bitcrafttech/hivenet/internal/httpapi"
	"github.com/bitcrafttech/hivenet/internal/reconcile"
	"github.com/bitcrafttech/hivenet/internal/store"
	"github.com/bitcrafttech/hivenet/web"
)

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	debounce := flag.Duration("debounce", reconcile.DefaultDebounce,
		"settle time after the last edit before a reconcile runs")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	flag.Parse()

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	if err := run(*addr, *debounce, logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("hivenet exited", "error", err)
		os.Exit(1)
	}
}

func `run`(addr string, debounce time.Duration, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	drv, err := newDriver(logger)
	if err != nil {
		return fmt.Errorf("initialise driver: %w", err)
	}
	defer func() { _ = drv.Close() }()

	topologies := store.New()
	layouts := store.NewLayout()

	engine := reconcile.NewEngine(reconcile.Options{
		Driver:   drv,
		Store:    topologies,
		Debounce: debounce,
		Logger:   logger,
	})

	// Take over whatever is already running rather than tearing it down: the
	// store is in-memory, so without this a restart of the app would be a
	// restart of the lab.
	if err := engine.Adopt(ctx); err != nil {
		logger.Warn("could not adopt running topology", "error", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := engine.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("reconcile engine stopped", "error", err)
		}
	}()

	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("load embedded frontend: %w", err)
	}

	api := httpapi.New(httpapi.Options{
		Store:  topologies,
		Layout: layouts,
		Engine: engine,
		Assets: assets,
		Logger: logger,
	})

	srv := &http.Server{
		Addr:    addr,
		Handler: api.Handler(auth.NoOp{}),
		// Only the header read is bounded. A body or write timeout would cut
		// the WebSocket streams off mid-session.
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("hivenet listening", "addr", addr, "driver", driverName)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		wg.Wait()
		return err
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
