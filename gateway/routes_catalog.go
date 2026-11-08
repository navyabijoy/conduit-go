package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"conduit/sdk"
)

func (s *Server) handleListConnectors(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	type CatalogItem struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Category    string   `json:"category"`
		Scopes      []string `json:"scopes"`
	}

	var items []CatalogItem
	for _, conn := range s.registry {
		meta := conn.Metadata()
		items = append(items, CatalogItem{
			ID:          meta.ID,
			Name:        meta.Name,
			Description: meta.Description,
			Category:    meta.Category,
			Scopes:      meta.Scopes,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// handleInstallConnector serves POST /v1/connectors/{id}/install. OAuth2 connectors get the
// authorize URL and are created in handleOAuthCallback; API-key ones are created at once.
func (s *Server) handleInstallConnector(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/connectors/"), "/")
	if len(parts) < 2 || parts[1] != "install" {
		writeJSONError(w, http.StatusBadRequest, "Invalid installation path")
		return
	}

	connectorID := parts[0]
	conn, ok := s.registry[connectorID]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "Connector not found")
		return
	}

	authCfg := conn.AuthConfig()
	if authCfg.Type == sdk.AuthTypeOAuth2 {
		// state ties the callback to this install and guards against CSRF.
		stateBytes := make([]byte, 16)
		rand.Read(stateBytes)
		state := hex.EncodeToString(stateBytes)

		if !s.rememberOAuthState(state, connectorID) {
			writeJSONError(w, http.StatusServiceUnavailable, "Too many installs in progress; try again later")
			return
		}

		// TODO: Derive the redirect URI and client ID from configuration; they
		// are hardcoded for the mock provider and a gateway on :8080.
		redirectURI := "http://localhost:8080/oauth/callback"
		authURL, err := url.Parse(authCfg.OAuth2.AuthURL)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "Invalid authorize URL")
			return
		}

		q := authURL.Query()
		q.Set("client_id", "conduit_client_"+connectorID)
		q.Set("redirect_uri", redirectURI)
		q.Set("state", state)
		q.Set("scope", strings.Join(authCfg.OAuth2.Scopes, " "))
		authURL.RawQuery = q.Encode()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"type":         "oauth2",
			"redirect_url": authURL.String(),
		})
		return
	}

	if authCfg.Type == sdk.AuthTypeAPIKey {
		var body struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&body); err != nil || body.APIKey == "" {
			writeJSONError(w, http.StatusBadRequest, "api_key is required in request body for API key connectors")
			return
		}

		inst, err := s.createInstance(conn, sdk.AuthTypeAPIKey, &sdk.Credentials{APIKey: body.APIKey})
		if err != nil {
			slog.Error("install failed", "component", "gateway", "connector", connectorID, "err", err)
			writeJSONError(w, http.StatusInternalServerError, "Failed to save connector instance")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"type":        "apikey",
			"instance_id": inst.ID,
			"status":      "active",
		})
		return
	}

	writeJSONError(w, http.StatusBadRequest, "Unsupported auth type")
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	if code == "" || state == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing code or state")
		return
	}

	connectorID, ok := s.takeOAuthState(state)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "Invalid or expired state token")
		return
	}

	conn := s.registry[connectorID]
	authCfg := conn.AuthConfig()

	tokenURL := authCfg.OAuth2.TokenURL
	client := &http.Client{Timeout: 10 * time.Second}

	// TODO: Read client credentials from configuration instead of deriving
	// them from the connector ID, which only the mock provider accepts.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", "conduit_client_"+connectorID)
	form.Set("client_secret", "conduit_secret_"+connectorID)
	form.Set("redirect_uri", "http://localhost:8080/oauth/callback")

	resp, err := client.PostForm(tokenURL, form)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "Failed to exchange authorization code: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("Token exchange returned status %d: %s", resp.StatusCode, string(bodyBytes)))
		return
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to parse token response")
		return
	}

	token := &sdk.Token{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		Expiry:       time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}

	inst, err := s.createInstance(conn, sdk.AuthTypeOAuth2, &sdk.Credentials{Token: token})
	if err != nil {
		slog.Error("install failed", "component", "gateway", "connector", connectorID, "err", err)
		writeJSONError(w, http.StatusInternalServerError, "Failed to save connector instance")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := fmt.Sprintf(`
		<!DOCTYPE html>
		<html>
		<head>
			<title>Installation Successful</title>
			<style>
				body { font-family: -apple-system, sans-serif; background: #0f172a; color: #f8fafc; padding: 2rem; display: flex; justify-content: center; align-items: center; height: 80vh; margin: 0; }
				.card { background: #1e293b; border: 1px solid #334155; padding: 2rem; border-radius: 12px; width: 400px; box-shadow: 0 4px 6px -1px rgba(0,0,0,0.1); text-align: center; }
				h2 { margin-top: 0; color: #10b981; }
				.info { margin: 1rem 0; color: #94a3b8; font-size: 0.9rem; line-height: 1.4; }
				.btn { display: inline-block; padding: 0.75rem 1.5rem; background: #6366f1; border: none; color: white; border-radius: 6px; font-weight: bold; cursor: pointer; text-decoration: none; margin-top: 1rem; }
				.btn:hover { background: #4f46e5; }
			</style>
		</head>
		<body>
			<div class="card">
				<h2>Installation Successful!</h2>
				<p class="info">Connector <strong>%s</strong> was installed successfully.</p>
				<p class="info">Instance ID: <code>%s</code></p>
				<p class="info">You can now close this tab and return to the main dashboard.</p>
				<a href="/ui/" class="btn">Go to Dashboard</a>
			</div>
		</body>
		</html>
	`, conn.Metadata().Name, inst.ID)
	w.Write([]byte(html))
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	list, err := s.db.ListInstances()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "Failed to load instances: "+err.Error())
		return
	}

	// Encode an empty result as [] rather than null so clients can iterate it.
	if list == nil {
		list = []*ConnectorInstance{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// handleInstanceRoute dispatches GET/DELETE /v1/instances/{id} and POST .../endpoints/{endpoint}.
func (s *Server) handleInstanceRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/instances/")
	parts := strings.Split(path, "/")

	if len(parts) == 0 || parts[0] == "" {
		writeJSONError(w, http.StatusBadRequest, "Missing instance ID")
		return
	}

	instanceID := parts[0]

	if r.Method == "DELETE" && len(parts) == 1 {
		if err := s.db.DeleteInstance(instanceID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "Failed to delete instance: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"deleted"}`))
		return
	}

	if r.Method == "GET" && len(parts) == 1 {
		inst, err := s.db.Instance(instanceID)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "Instance not found: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(inst)
		return
	}

	if r.Method == "POST" && len(parts) >= 3 && parts[1] == "endpoints" {
		s.handleExecuteEndpoint(w, r, instanceID, parts[2])
		return
	}

	writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
}

// createInstance captures a baseline for creds, then stores the instance and its credentials.
func (s *Server) createInstance(conn sdk.Connector, authType sdk.AuthType, creds *sdk.Credentials) (*ConnectorInstance, error) {
	connectorID := conn.Metadata().ID
	inst := &ConnectorInstance{
		ID:          newInstanceID(connectorID),
		ConnectorID: connectorID,
		Status:      "active",
		CreatedAt:   time.Now(),
	}

	// Without a baseline the instance still works; drift detection just skips it.
	baselineJSON, err := s.generateBaseline(inst.ID, conn, creds)
	if err != nil {
		slog.Warn("baseline capture failed", "component", "gateway", "instance_id", inst.ID, "connector", connectorID, "err", err)
	} else {
		inst.BaselineSchema = baselineJSON
	}

	if err := s.db.CreateInstance(inst, authType, creds); err != nil {
		return nil, err
	}
	return inst, nil
}

// rememberOAuthState stores a started install and drops abandoned ones. It reports false
// when maxPendingInstalls are already open, since the API has no auth to stop a flood.
func (s *Server) rememberOAuthState(state, connectorID string) bool {
	s.pendingOAuthMu.Lock()
	defer s.pendingOAuthMu.Unlock()

	for k, p := range s.pendingOAuth {
		if time.Since(p.started) > oauthStateTTL {
			delete(s.pendingOAuth, k)
		}
	}
	if len(s.pendingOAuth) >= maxPendingInstalls {
		return false
	}
	s.pendingOAuth[state] = pendingInstall{connectorID: connectorID, started: time.Now()}
	return true
}

// takeOAuthState consumes state so it works once; unknown or expired states fail.
func (s *Server) takeOAuthState(state string) (string, bool) {
	s.pendingOAuthMu.Lock()
	p, ok := s.pendingOAuth[state]
	delete(s.pendingOAuth, state)
	s.pendingOAuthMu.Unlock()

	if !ok || time.Since(p.started) > oauthStateTTL {
		return "", false
	}
	return p.connectorID, true
}

func newInstanceID(connectorID string) string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("inst_%s_%s", connectorID, hex.EncodeToString(b))
}

// generateBaseline returns the flattened response shape of the connector's probe, as JSON,
// for later drift comparison.
func (s *Server) generateBaseline(instanceID string, conn sdk.Connector, creds *sdk.Credentials) (string, error) {
	connectorID := conn.Metadata().ID
	slog.Info("capturing baseline schema", "component", "gateway", "instance_id", instanceID, "connector", connectorID)

	client := sdk.NewHTTPClient(
		"http://"+s.mockHost,
		*creds,
		conn.AuthConfig(),
		s.tokenRefreshHook(instanceID, conn),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	payload, err := runProbe(ctx, conn, client, probes[connectorID])
	if err != nil {
		return "", fmt.Errorf("baseline request failed: %w", err)
	}

	var obj interface{}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", fmt.Errorf("failed to unmarshal baseline response: %w", err)
	}

	flatSchema := make(map[string]string)
	flattenJSONSchema("", obj, flatSchema)

	schemaBytes, err := json.Marshal(flatSchema)
	if err != nil {
		return "", fmt.Errorf("marshal baseline schema: %w", err)
	}
	return string(schemaBytes), nil
}
