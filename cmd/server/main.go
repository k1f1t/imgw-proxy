package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"imgw-openmeteo-proxy/internal/api"
	"imgw-openmeteo-proxy/internal/config"
	"imgw-openmeteo-proxy/internal/imgw"
	"imgw-openmeteo-proxy/internal/scheduler"
	"imgw-openmeteo-proxy/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load configuration", "err", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	logger.Info("starting imgw-openmeteo-proxy (self-hosted)",
		"listen_addr", cfg.ListenAddr,
		"db_path", cfg.DBPath,
		"forecast_mode", cfg.ForecastMode,
		"allow_ondemand_forecasts", cfg.AllowOnDemandForecasts(),
		"retention_days", cfg.RetentionDays,
		"auth_enabled", cfg.APIKey != "",
	)
	logger.Info("IMGW-PIB attribution", "notice", api.AttributionSourcePL)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Error("failed to open sqlite store", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Warn("error closing sqlite store", "err", err)
		}
	}()

	client := imgw.NewClient(
		cfg.DanepubliczneBaseURL,
		cfg.MeteoForecastBaseURL,
		cfg.MeteoForecastToken,
		cfg.ForecastModel,
		cfg.UserAgent,
		cfg.UpstreamTimeout,
	)

	sched, err := scheduler.New(cfg, client, st, logger)
	if err != nil {
		logger.Error("failed to initialize scheduler", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sched.Start(ctx)

	srv := api.NewServer(cfg, st, sched, sched)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP server listening", "addr", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down gracefully...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("HTTP shutdown error", "err", err)
		}
	case err := <-errCh:
		if err != nil {
			logger.Error("HTTP server failed", "err", err)
			os.Exit(1)
		}
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
