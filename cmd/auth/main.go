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

	"github.com/deepmav/pocketpilot-cloud/internal/auth"
	"github.com/deepmav/pocketpilot-cloud/internal/token"
)

func main() {
	var (
		addr      = flag.String("addr", ":8081", "HTTP listen address")
		jwtSecret = flag.String("jwt-secret", "", "HMAC secret for JWT (also env JWT_SECRET)")
		accessTTL = flag.Duration("access-ttl", time.Hour, "Access token TTL")
	)
	flag.Parse()

	if *jwtSecret == "" {
		*jwtSecret = os.Getenv("JWT_SECRET")
	}
	if *jwtSecret == "" {
		slog.Error("JWT_SECRET is required (flag -jwt-secret or env)")
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	issuer := token.NewIssuer([]byte(*jwtSecret), *accessTTL)
	verifier := token.NewVerifier([]byte(*jwtSecret))

	users := auth.NewMemoryStore()
	if err := users.SeedDev(); err != nil {
		slog.Error("seed users", "err", err)
		os.Exit(1)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h := auth.NewHTTPHandler(users, issuer)
	r.Route("/v1", func(r chi.Router) {
		r.Post("/auth/login", h.Login)
		r.Group(func(r chi.Router) {
			r.Use(token.RequireToken(verifier))
			r.Get("/me", h.Me)
		})
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("auth service listening", "addr", *addr)
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
