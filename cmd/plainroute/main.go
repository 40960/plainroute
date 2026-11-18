// SPDX-License-Identifier: AGPL-3.0-only

// PlainRoute is a small, boring, transparent LLM gateway.
//
// The binary is the wiring only: load config, build the handler, listen, handle
// signals. Everything that decides what happens to a request lives in internal.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/40960/plainroute/internal/config"
	"github.com/40960/plainroute/internal/logging"
	"github.com/40960/plainroute/internal/metrics"
	"github.com/40960/plainroute/internal/proxy"
	"github.com/40960/plainroute/internal/routing"
)

const version = "0.1.0"

func main() {
	cfgPath := flag.String("config", envOr("PLAINROUTE_CONFIG", "config.yaml"), "path to the YAML config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("plainroute", version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plainroute: %v\n", err)
		os.Exit(1)
	}

	log := logging.New(cfg.Logging, os.Stderr)
	m := metrics.New()
	handler := proxy.New(cfg, m, log, routing.NewStateStore(), time.Now)

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		// WriteTimeout stays 0 by default: a streaming response must not be cut off
		// by an ordinary request timeout.
		WriteTimeout:   cfg.Server.WriteTimeout.Std(),
		IdleTimeout:    cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
		ErrorLog:       logging.StdLogger(log),
	}

	listener, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plainroute: listen %s: %v\n", cfg.Server.Listen, err)
		os.Exit(1)
	}

	// Readiness means the config loaded and the server can serve. A single
	// upstream returning 429 never makes the whole gateway unready.
	handler.MarkReady()

	// The gate state, and never the value. A key is a value in the file now, so there is nothing
	// here that can be reported about it except whether the gate is on: printing the value would
	// print a secret into the log, and a log line is what a bug report and a terminal show.
	gate := "off"
	if cfg.Auth.APIKey != "" {
		gate = "on"
	}

	log.Info("listening",
		"listen", cfg.Server.Listen,
		"routes", len(cfg.Routes),
		"strategy", cfg.Routing.Strategy,
		"retry_enabled", cfg.Retry.Enabled,
		"stream_idle", cfg.Timeouts.StreamIdle.String(),
		"version", version,
		"gate", gate,
		"config", *cfgPath,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Serve(listener) }()

	reloading := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				reloading <- struct{}{}
			}
		}
	}()

	for {
		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "plainroute: serve: %v\n", err)
				os.Exit(1)
			}
			return
		case <-reloading:
			reloaded, err := config.Load(*cfgPath)
			if err != nil {
				m.Reloads.WithLabelValues("failed").Inc()
				log.Error("reload_failed", "error", err)
				continue
			}
			handler.Reload(reloaded)
			log.Info("config_reloaded", "routes", len(reloaded.Routes))
		case <-ctx.Done():
			// Drain: stop taking new requests, then let the requests already
			// streaming finish within the grace period.
			handler.Drain()
			deadline := time.Now().Add(cfg.Server.ShutdownGrace.Std())
			if err := srv.Shutdown(context.Background()); err != nil {
				log.Warn("shutdown_wait_failed", "error", err)
			}
			if !handler.Wait(deadline) {
				log.Warn("shutdown_grace_exceeded", "grace", cfg.Server.ShutdownGrace.String())
			}
			if err := srv.Close(); err != nil {
				log.Warn("close_failed", "error", err)
			}
			log.Info("shutdown_complete")
			return
		}
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
