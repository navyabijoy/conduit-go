// Package gateway is Conduit's HTTP server: it installs connector instances, runs their
// endpoints, receives webhooks and detects schema drift.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"conduit/connectors/github"
	"conduit/connectors/slack"
	"conduit/connectors/stripe"
	"conduit/sdk"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server serves the API, webhooks, docs, metrics and dashboard.
type Server struct {
	db             *DB
	mockHost       string
	webhookSecret  string
	registry       map[string]sdk.Connector
	pendingOAuth   map[string]pendingInstall // OAuth state -> install in progress
	pendingOAuthMu sync.Mutex
	driftDetector  *DriftDetector
}

// pendingInstall is an OAuth install that was started but not completed.
type pendingInstall struct {
	connectorID string
	started     time.Time
}

// oauthStateTTL is how long a user has to finish the provider's consent screen.
const oauthStateTTL = 10 * time.Minute

// maxPendingInstalls caps unfinished OAuth installs held in memory.
const maxPendingInstalls = 1000

// maxBodyBytes caps request bodies so one large upload cannot exhaust memory. It
// matches GitHub's 25 MB webhook payload limit, the largest a provider sends.
const maxBodyBytes = 25 << 20

// New returns a Server with Slack, GitHub and Stripe registered. mockHost is the host:port
// connector requests go to; webhookSecret verifies inbound webhooks.
func New(db *DB, mockHost string, webhookSecret string) *Server {
	registry := map[string]sdk.Connector{
		"slack":  slack.New(mockHost),
		"github": github.New(mockHost),
		"stripe": stripe.New(),
	}

	s := &Server{
		db:            db,
		mockHost:      mockHost,
		webhookSecret: webhookSecret,
		registry:      registry,
		pendingOAuth:  make(map[string]pendingInstall),
	}

	s.driftDetector = NewDriftDetector(db, registry, mockHost)
	return s
}

// Handler returns the gateway's routes; tests mount it on an httptest.Server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/connectors", s.handleListConnectors)
	mux.HandleFunc("/v1/connectors/", s.handleInstallConnector) // POST /v1/connectors/{id}/install
	mux.HandleFunc("/oauth/callback", s.handleOAuthCallback)

	mux.HandleFunc("/v1/instances", s.handleListInstances)
	// GET and DELETE /v1/instances/{id}, POST /v1/instances/{id}/endpoints/{endpoint}
	mux.HandleFunc("/v1/instances/", s.handleInstanceRoute)

	mux.HandleFunc("/webhooks/", s.handleWebhookPayload)

	mux.HandleFunc("/openapi.json", s.handleOpenAPISpec)
	mux.HandleFunc("/docs", s.handleDocs)
	mux.HandleFunc("/docs/", s.handleDocs)
	mux.Handle("/metrics", promhttp.Handler())

	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir("./ui"))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	return logRequests(mux)
}

// Start binds addr before returning, so the port is open for callers, then serves in the
// background and starts the drift detector.
func (s *Server) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	s.driftDetector.Start(30 * time.Second)

	slog.Info("gateway starting", "component", "gateway", "addr", addr)
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("gateway stopped serving", "component", "gateway", "err", err)
		}
	}()
	return nil
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request", "component", "gateway", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

// tokenRefreshHook returns an instance's refresh hook; it persists the new token so later
// requests and drift scans use it.
func (s *Server) tokenRefreshHook(instanceID string, conn sdk.Connector) func(ctx context.Context, token *sdk.Token) (*sdk.Token, error) {
	connectorID := conn.Metadata().ID
	return func(ctx context.Context, oldToken *sdk.Token) (*sdk.Token, error) {
		slog.Info("refreshing oauth token", "component", "gateway", "instance_id", instanceID, "connector", connectorID)

		authCfg := conn.AuthConfig()
		if authCfg.Type != sdk.AuthTypeOAuth2 || authCfg.OAuth2 == nil {
			return nil, fmt.Errorf("connector does not support OAuth2")
		}

		// TODO: A real provider also needs the client ID and secret here; the
		// mock provider accepts the refresh token alone.
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {oldToken.RefreshToken},
		}
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.PostForm(authCfg.OAuth2.TokenURL, form)
		if err != nil {
			recordTokenRefresh(connectorID, "failure")
			return nil, fmt.Errorf("failed to call refresh URL: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			recordTokenRefresh(connectorID, "failure")
			return nil, fmt.Errorf("refresh server returned code %d", resp.StatusCode)
		}

		var tokenResp struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			ExpiresIn    int    `json:"expires_in"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
			recordTokenRefresh(connectorID, "failure")
			return nil, fmt.Errorf("failed to parse refresh response: %w", err)
		}

		newToken := &sdk.Token{
			AccessToken:  tokenResp.AccessToken,
			RefreshToken: tokenResp.RefreshToken,
			Expiry:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
		}

		creds := &sdk.Credentials{Token: newToken}
		if err := s.db.UpdateCredentials(instanceID, sdk.AuthTypeOAuth2, creds); err != nil {
			recordTokenRefresh(connectorID, "failure")
			return nil, fmt.Errorf("failed to save refreshed credentials: %w", err)
		}

		recordTokenRefresh(connectorID, "success")
		slog.Info("oauth token refreshed", "component", "gateway", "instance_id", instanceID, "connector", connectorID)
		return newToken, nil
	}
}

// readBody reads the request body up to maxBodyBytes. On failure it also returns the
// status to answer with: 413 for an oversized body, 400 otherwise.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, int, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, http.StatusRequestEntityTooLarge, err
		}
		return nil, http.StatusBadRequest, err
	}
	return body, http.StatusOK, nil
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
