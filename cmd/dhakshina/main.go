// Command dhakshina runs the Dhakshina Desk backend: one binary, built as a
// modular monolith, that serves the API and runs the background workers.
//
//	dhakshina serve            API and/or worker, by ROLE (api, worker, all)
//	dhakshina indexes          create MongoDB indexes, once per environment
//	dhakshina admin-hash PASS  print an ADMIN_USERS hash for a staff password
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/armohalabs/dhakshina-desk-api/internal/admin"
	"github.com/armohalabs/dhakshina-desk-api/internal/app"
	"github.com/armohalabs/dhakshina-desk-api/internal/config"
	"github.com/armohalabs/dhakshina-desk-api/internal/indexes"
	"github.com/armohalabs/dhakshina-desk-api/internal/platform/db"
	"github.com/armohalabs/dhakshina-desk-api/internal/server"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	if cmd == "admin-hash" {
		if len(os.Args) < 3 || len(os.Args[2]) < 12 {
			fmt.Fprintln(os.Stderr, "usage: dhakshina admin-hash '<a password of 12+ characters>'")
			os.Exit(2)
		}
		fmt.Println(admin.HashPassword(os.Args[2], admin.NewSalt()))
		return
	}
	if err := run(cmd); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cmd string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	m, err := db.Connect(ctx, cfg.MongoURI, cfg.MongoAdminURI, cfg.MongoDB)
	if err != nil {
		return err
	}
	defer m.Close(context.Background())

	switch cmd {
	case "indexes":
		if err := indexes.Ensure(ctx, m.DB); err != nil {
			return err
		}
		slog.Info("indexes_ready", "db", cfg.MongoDB)
		return nil
	case "serve":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	a := app.New(cfg, m)
	mods := server.NewModules(a)
	if !cfg.IsProd() {
		slog.Warn("development mode: OTP codes are returned as dev_code", "gateway", cfg.Gateway, "whatsapp_live", a.WA.Live())
	}
	if cfg.Role == "worker" || cfg.Role == "all" {
		go mods.Worker.Run(ctx)
		go mods.Worker.Schedule(ctx)
	}
	if cfg.Role == "worker" {
		<-ctx.Done()
		return nil
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Router(a, mods),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "role", cfg.Role, "env", cfg.Env)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shut)
}
