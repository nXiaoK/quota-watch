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
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("服务启动失败", "error", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", envDefault("QUOTA_WATCH_LISTEN", "127.0.0.1:8091"), "HTTP 监听地址")
	directory := flag.String("data-dir", envDefault("QUOTA_WATCH_DATA_DIR", "./data"), "SQLite 和密钥存储目录")
	flag.Parse()
	username := envDefault("QUOTA_WATCH_USERNAME", "admin")
	password := os.Getenv("QUOTA_WATCH_PASSWORD")
	if password == "" {
		return errors.New("请通过 QUOTA_WATCH_PASSWORD 设置管理页登录密码")
	}
	store, err := OpenStore(*directory, os.Getenv("QUOTA_WATCH_MASTER_KEY"))
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := slog.Default()
	engine := NewEngine(store, logger)
	updater := NewUpdater(store, engine, logger)
	telegramReceiver := NewTelegramReceiver(store, engine, logger)
	panel := NewServer(ctx, store, engine, logger, username, password)
	httpServer := &http.Server{Addr: *listen, Handler: panel.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 3 * time.Minute, IdleTimeout: 60 * time.Second}
	engineDone := make(chan struct{})
	go func() { defer close(engineDone); engine.Run(ctx) }()
	updaterDone := make(chan struct{})
	go func() { defer close(updaterDone); updater.Run(ctx) }()
	telegramDone := make(chan struct{})
	go func() { defer close(telegramDone); telegramReceiver.Run(ctx) }()
	serverDone := make(chan error, 1)
	go func() { serverDone <- httpServer.ListenAndServe() }()
	logger.Info("Quota Watch 已启动", "listen", *listen, "data_dir", *directory)
	select {
	case err := <-serverDone:
		cancel()
		<-engineDone
		<-updaterDone
		<-telegramDone
		panel.jobs.Wait()
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP 服务: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			logger.Warn("HTTP 关闭超时")
		}
		<-engineDone
		<-updaterDone
		<-telegramDone
		panel.jobs.Wait()
	}
	return nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
