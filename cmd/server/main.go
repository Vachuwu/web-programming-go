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

	"playlist/internal/config"
	"playlist/internal/database"
	"playlist/internal/handler"
	"playlist/internal/middleware"
	"playlist/internal/repository"
	"playlist/internal/service"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		return
	}
	logger := newLogger(cfg.LogLevel)

	connectContext, cancelConnect := context.WithTimeout(context.Background(), cfg.DBConnectTimeout)
	defer cancelConnect()
	pool, err := database.Open(connectContext, cfg.DatabaseURL, cfg.DBMaxConnections)
	if err != nil {
		logger.Error("connect to database", "error", err)
		return
	}

	users := service.NewUserService(repository.NewUserRepository(pool))
	playlists := service.NewPlaylistService(repository.NewPlaylistRepository(pool))
	httpHandler := handler.New(users, playlists)
	routes := httpHandler.Routes()
	wrapped := middleware.RequestID(middleware.Timeout(cfg.HTTPTimeout)(middleware.Timeout(cfg.DBOperationTimeout)(middleware.Logging(logger)(routes))))
	server := &http.Server{
		Addr:              cfg.HTTPAddress,
		Handler:           wrapped,
		ReadHeaderTimeout: cfg.HTTPTimeout,
		IdleTimeout:       60 * time.Second,
	}

	shutdownSignals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	logger.Info("playlist server listening", "address", cfg.HTTPAddress)
	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("serve HTTP", "error", err)
		}
		pool.Close()
	case <-shutdownSignals.Done():
		logger.Info("shutdown signal received")
		httpHandler.EventHub().Close()
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		err := server.Shutdown(shutdownContext)
		cancelShutdown()
		if err != nil {
			logger.Error("graceful HTTP shutdown", "error", err)
		}
		pool.Close()
	}
}

func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	if err := slogLevel.UnmarshalText([]byte(level)); err != nil {
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel}))
}
