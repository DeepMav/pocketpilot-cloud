// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	sig "github.com/deepmav/pocketpilot-cloud/internal/signal"
	"github.com/deepmav/pocketpilot-cloud/internal/token"
)

func main() {
	var (
		addr      = flag.String("addr", ":8080", "HTTP listen address")
		jwtSecret = flag.String("jwt-secret", "", "HMAC secret for JWT (also env JWT_SECRET; must match cmd/auth)")
		turnURI   = flag.String("turn-uri", "", "TURN server URI to advertise (e.g. turn:host:3478?transport=udp)")
		turnUser  = flag.String("turn-user", "", "TURN static username (PoC; replace with RFC 7635)")
		turnPass  = flag.String("turn-pass", "", "TURN static credential")
	)
	flag.Parse()

	if *jwtSecret == "" {
		*jwtSecret = os.Getenv("JWT_SECRET")
	}
	if *jwtSecret == "" {
		slog.Error("JWT_SECRET is required (must match cmd/auth)")
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	verifier := token.NewVerifier([]byte(*jwtSecret))

	var ice []sig.IceServer
	if *turnURI != "" {
		ice = append(ice, sig.IceServer{
			URLs:       []string{*turnURI},
			Username:   *turnUser,
			Credential: *turnPass,
		})
	}

	hub := sig.NewHub(ice)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wsh := sig.NewWSHandler(hub, verifier)
	r.Route("/v1", func(r chi.Router) {
		r.Get("/signal", wsh.ServeHTTP)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("signal service listening", "addr", *addr, "turn_configured", *turnURI != "")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
}
