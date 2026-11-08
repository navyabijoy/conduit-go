// Command gateway runs the Conduit gateway and the mock provider it talks to.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"conduit/gateway"
	"conduit/mock"
)

func main() {
	addr := flag.String("addr", ":8080", "Gateway listen address")
	dbPath := flag.String("db", "./conduit.db", "SQLite database path")
	mockAddr := flag.String("mock-addr", ":8081", "Mock provider listen address, in :port form")
	webhookSecret := flag.String("webhook-secret", envOr("CONDUIT_WEBHOOK_SECRET", devWebhookSecret), "Shared HMAC webhook secret")
	flag.Parse()

	if err := checkWebhookSecret(*webhookSecret, os.Getenv("CONDUIT_ENV") == "production"); err != nil {
		fatal("invalid webhook secret", "err", err)
	}

	// Connectors only know the mock provider's URLs, so it always runs.
	slog.Info("starting mock provider", "component", "main", "addr", *mockAddr)
	if err := mock.NewServer(*mockAddr, *webhookSecret).Start(); err != nil {
		fatal("start mock provider failed", "addr", *mockAddr, "err", err)
	}
	mockHost := "localhost" + *mockAddr

	db, err := gateway.NewDB(*dbPath)
	if err != nil {
		fatal("open database failed", "path", *dbPath, "err", err)
	}
	defer db.Close()

	gw := gateway.New(db, mockHost, *webhookSecret)
	if err := gw.Start(*addr); err != nil {
		fatal("start gateway failed", "addr", *addr, "err", err)
	}

	slog.Info("conduit gateway running", "component", "main", "addr", *addr, "mock_addr", *mockAddr)
	slog.Info("press Ctrl+C to stop", "component", "main")

	// TODO: Shut the HTTP servers down gracefully; today the process exits on
	// the first signal without draining in-flight requests.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down", "component", "main")
}

// devWebhookSecret lets the demo run with no setup. It is public, so production
// refuses it.
const devWebhookSecret = "mock_webhook_secret_value"

// checkWebhookSecret rejects secrets anyone could sign with: an empty one, and in
// production the public development default.
func checkWebhookSecret(secret string, production bool) error {
	if secret == "" {
		return errors.New("webhook secret is empty; set CONDUIT_WEBHOOK_SECRET")
	}
	if production && secret == devWebhookSecret {
		return errors.New("CONDUIT_WEBHOOK_SECRET must be set in production; the default is public")
	}
	return nil
}

// fatal logs and exits; it skips deferred calls, which is fine because it only runs during
// startup.
func fatal(msg string, args ...any) {
	slog.Error(msg, append([]any{"component", "main"}, args...)...)
	os.Exit(1)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
