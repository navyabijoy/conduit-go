// Package mock stands in for the Slack, GitHub and Stripe APIs so the gateway runs without
// credentials. Its /mock/* endpoints toggle schema drift and rate limiting.
package mock

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is the mock provider; drift and rate limiting are toggled through
// /mock/toggle-drift and /mock/toggle-rate-limit.
type Server struct {
	addr            string
	mu              sync.RWMutex
	driftSlack      bool
	driftGitHub     bool
	driftStripe     bool
	rateLimitSlack  bool
	rateLimitGitHub bool
	rateLimitStripe bool
	webhookSecret   string
}

// NewServer returns a mock provider listening on addr that signs webhooks with
// webhookSecret.
func NewServer(addr string, webhookSecret string) *Server {
	return &Server{
		addr:          addr,
		webhookSecret: webhookSecret,
	}
}

// Handler returns the mock's routes; tests mount it on an httptest.Server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/mock/oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("/services/slack/oauth/token", s.handleSlackToken)
	mux.HandleFunc("/services/github/login/oauth/access_token", s.handleGitHubToken)
	mux.HandleFunc("/services/slack/chat.postMessage", s.handleSlackPostMessage)
	mux.HandleFunc("/services/github/repos/", s.handleGitHubIssues)
	mux.HandleFunc("/services/stripe/v1/charges", s.handleStripeListCharges)

	mux.HandleFunc("/mock/toggle-drift", s.handleToggleDrift)
	mux.HandleFunc("/mock/toggle-rate-limit", s.handleToggleRateLimit)
	mux.HandleFunc("/mock/trigger-webhook", s.handleTriggerWebhook)

	return mux
}

// Start binds the address before returning, so the port is open for callers, then serves
// in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}

	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("mock provider starting", "component", "mock", "addr", s.addr)
	go func() {
		if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("mock provider stopped serving", "component", "mock", "err", err)
		}
	}()
	return nil
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	redirectURI := r.URL.Query().Get("redirect_uri")
	state := r.URL.Query().Get("state")
	scopes := r.URL.Query().Get("scope")
	clientID := r.URL.Query().Get("client_id")

	// Escaping alone would not stop "javascript:" links, so only http(s) targets are allowed.
	if u, err := url.Parse(redirectURI); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "redirect_uri must be an http or https URL", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := fmt.Sprintf(`
		<!DOCTYPE html>
		<html>
		<head>
			<title>Mock OAuth Consent</title>
			<style>
				body { font-family: -apple-system, sans-serif; background: #0f172a; color: #f8fafc; padding: 2rem; display: flex; justify-content: center; align-items: center; height: 80vh; margin: 0; }
				.card { background: #1e293b; border: 1px solid #334155; padding: 2rem; border-radius: 12px; width: 400px; box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1); }
				h2 { margin-top: 0; color: #818cf8; }
				.info { margin: 1rem 0; color: #94a3b8; font-size: 0.9rem; line-height: 1.4; }
				.btn { display: block; width: 100%%; padding: 0.75rem; background: #6366f1; border: none; color: white; border-radius: 6px; font-weight: bold; cursor: pointer; text-align: center; text-decoration: none; }
				.btn:hover { background: #4f46e5; }
			</style>
		</head>
		<body>
			<div class="card">
				<h2>Authorize Integration</h2>
				<p class="info">Client ID: <code>%s</code></p>
				<p class="info">Requested Scopes: <code>%s</code></p>
				<p class="info">This is a simulated OAuth screen. Click the button to grant access and return to Conduit.</p>
				<a href="%s?code=mock_code_12345&state=%s" class="btn">Authorize & Connect</a>
			</div>
		</body>
		</html>
	`, stdhtml.EscapeString(clientID), stdhtml.EscapeString(scopes),
		stdhtml.EscapeString(redirectURI), url.QueryEscape(state))
	w.Write([]byte(html))
}

func (s *Server) handleSlackToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"access_token":  "mock_slack_access_token_" + strconv.FormatInt(time.Now().Unix(), 10),
		"refresh_token": "mock_slack_refresh_token_xyz",
		"expires_in":    3600,
		"ok":            true,
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleGitHubToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]interface{}{
		"access_token":  "mock_github_access_token_" + strconv.FormatInt(time.Now().Unix(), 10),
		"refresh_token": "mock_github_refresh_token_xyz",
		"expires_in":    3600,
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleSlackPostMessage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	rateLimit := s.rateLimitSlack
	drift := s.driftSlack
	s.mu.RUnlock()

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"ok":false, "error":"not_authed"}`))
		return
	}

	if rateLimit {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"ok":false, "error":"ratelimited"}`))
		return
	}

	var reqBody map[string]interface{}
	bodyBytes, _ := io.ReadAll(r.Body)
	json.Unmarshal(bodyBytes, &reqBody)

	w.Header().Set("Content-Type", "application/json")

	if drift {
		// Drifted Slack response: channel -> channel_id_drifted, removes message ts
		resp := map[string]interface{}{
			"ok":                  true,
			"channel_id_drifted":  reqBody["channel"],
			"message_text":        reqBody["text"],
			"drift_indicator_xyz": true,
		}
		json.NewEncoder(w).Encode(resp)
		return
	}

	resp := map[string]interface{}{
		"ok":      true,
		"channel": reqBody["channel"],
		"ts":      "1234567890.123456",
		"message": map[string]interface{}{
			"text": reqBody["text"],
			"type": "message",
			"user": "U12345",
		},
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleGitHubIssues(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	rateLimit := s.rateLimitGitHub
	drift := s.driftGitHub
	s.mu.RUnlock()

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	if rateLimit {
		resetTime := time.Now().Unix() + 2
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetTime, 10))
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		return
	}

	// Path: /services/github/repos/{owner}/{repo}/issues
	parts := strings.Split(r.URL.Path, "/")
	isList := r.Method == "GET" && len(parts) >= 7 && parts[6] == "issues"
	isCreate := r.Method == "POST" && len(parts) >= 7 && parts[6] == "issues"

	w.Header().Set("Content-Type", "application/json")

	if isCreate {
		var reqBody map[string]interface{}
		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &reqBody)

		if drift {
			// Drifted GitHub issue response: missing 'number' field
			resp := map[string]interface{}{
				"id":    999,
				"title": reqBody["title"],
				"body":  reqBody["body"],
				"state": "open",
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		resp := map[string]interface{}{
			"id":     999,
			"number": 1,
			"title":  reqBody["title"],
			"body":   reqBody["body"],
			"state":  "open",
			"user": map[string]interface{}{
				"login": "mockuser",
			},
		}
		json.NewEncoder(w).Encode(resp)
		return
	}

	if isList {
		cursor := r.URL.Query().Get("cursor")

		if drift {
			// Drifted list issues: state is missing or renamed, id changes to string
			if cursor == "" || cursor == "page1" {
				w.Header().Set("Link", `</services/github/repos/owner/repo/issues?cursor=page2>; rel="next"`)
				resp := []map[string]interface{}{
					{"id": "issue-1", "title": "First Issue"},
					{"id": "issue-2", "title": "Second Issue"},
				}
				json.NewEncoder(w).Encode(resp)
				return
			}
			resp := []map[string]interface{}{
				{"id": "issue-3", "title": "Third Issue"},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		if cursor == "" || cursor == "page1" {
			w.Header().Set("Link", `</services/github/repos/owner/repo/issues?cursor=page2>; rel="next"`)
			resp := []map[string]interface{}{
				{"id": 1, "number": 1, "title": "First Issue", "state": "open"},
				{"id": 2, "number": 2, "title": "Second Issue", "state": "open"},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		resp := []map[string]interface{}{
			{"id": 3, "number": 3, "title": "Third Issue", "state": "closed"},
		}
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) handleStripeListCharges(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	rateLimit := s.rateLimitStripe
	drift := s.driftStripe
	s.mu.RUnlock()

	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer sk_") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"type":"authentication_error","message":"No valid API key provided"}}`))
		return
	}

	if rateLimit {
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"Too many requests"}}`))
		return
	}

	q := r.URL.Query()
	startingAfter := q.Get("starting_after")
	limit := 3

	w.Header().Set("Content-Type", "application/json")

	if drift {
		// Drifted: amount becomes string, currency removed
		data := []map[string]interface{}{
			{"id": "ch_drift_1", "amount_str": "2000", "status": "succeeded"},
			{"id": "ch_drift_2", "amount_str": "5000", "status": "succeeded"},
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object":   "list",
			"data":     data,
			"has_more": false,
		})
		return
	}

	var charges []map[string]interface{}
	if startingAfter == "" {
		for i := 1; i <= limit; i++ {
			charges = append(charges, map[string]interface{}{
				"id":       fmt.Sprintf("ch_page1_%d", i),
				"amount":   1000 * i,
				"currency": "usd",
				"status":   "succeeded",
				"created":  time.Now().Unix() - int64(i*3600),
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object":   "list",
			"data":     charges,
			"has_more": true,
			"url":      "/services/stripe/v1/charges",
		})
	} else {
		for i := 1; i <= 2; i++ {
			charges = append(charges, map[string]interface{}{
				"id":       fmt.Sprintf("ch_page2_%d", i),
				"amount":   5000 * i,
				"currency": "usd",
				"status":   "succeeded",
				"created":  time.Now().Unix() - int64((i+3)*3600),
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object":   "list",
			"data":     charges,
			"has_more": false,
			"url":      "/services/stripe/v1/charges",
		})
	}
}

func (s *Server) handleToggleDrift(w http.ResponseWriter, r *http.Request) {
	connectorID := r.URL.Query().Get("connector_id")
	driftStr := r.URL.Query().Get("drift")
	drift, _ := strconv.ParseBool(driftStr)

	s.mu.Lock()
	defer s.mu.Unlock()

	switch connectorID {
	case "slack":
		s.driftSlack = drift
	case "github":
		s.driftGitHub = drift
	case "stripe":
		s.driftStripe = drift
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"connector_id":"%s","drift":%v}`, connectorID, drift)
}

func (s *Server) handleToggleRateLimit(w http.ResponseWriter, r *http.Request) {
	connectorID := r.URL.Query().Get("connector_id")
	rateLimitStr := r.URL.Query().Get("rate_limit")
	rateLimit, _ := strconv.ParseBool(rateLimitStr)

	s.mu.Lock()
	defer s.mu.Unlock()

	switch connectorID {
	case "slack":
		s.rateLimitSlack = rateLimit
	case "github":
		s.rateLimitGitHub = rateLimit
	case "stripe":
		s.rateLimitStripe = rateLimit
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"connector_id":"%s","rate_limit":%v}`, connectorID, rateLimit)
}

// handleTriggerWebhook signs the payload as the named provider would, POSTs it to
// target_url and returns the response so callers can see whether verification passed.
func (s *Server) handleTriggerWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConnectorID string                 `json:"connector_id"`
		TargetURL   string                 `json:"target_url"`
		Payload     map[string]interface{} `json:"payload"`
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"cannot read body"}`))
		return
	}
	json.Unmarshal(bodyBytes, &req)

	payloadBytes, _ := json.Marshal(req.Payload)

	mac := hmac.New(sha256.New, []byte(s.webhookSecret))
	mac.Write(payloadBytes)
	signatureHex := hex.EncodeToString(mac.Sum(nil))

	webhookReq, err := http.NewRequest("POST", req.TargetURL, bytes.NewReader(payloadBytes))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf(`{"error":"%v"}`, err)))
		return
	}

	webhookReq.Header.Set("Content-Type", "application/json")
	switch req.ConnectorID {
	case "slack":
		webhookReq.Header.Set("X-Slack-Signature", "v0="+signatureHex)
		webhookReq.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	case "github":
		webhookReq.Header.Set("X-Hub-Signature-256", "sha256="+signatureHex)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(webhookReq)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(fmt.Sprintf(`{"error":"failed to dispatch webhook: %v"}`, err)))
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}
