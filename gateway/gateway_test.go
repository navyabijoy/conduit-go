package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"conduit/mock"
	"conduit/sdk"
)

// testWebhookSecret is the secret the test gateway stores on every instance.
const testWebhookSecret = "test_webhook_secret_value"

// slackSignature signs payload the way the mock provider signs Slack webhooks.
func slackSignature(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// newTestGateway stands up a real mock.Server and a Gateway pointing at it,
// all in-process, using an in-memory SQLite database.
func newTestGateway(t *testing.T) (*Server, *mock.Server, func()) {
	t.Helper()

	const webhookSecret = testWebhookSecret

	mockSrv := mock.NewServer("", webhookSecret)
	mockTS := httptest.NewServer(mockSrv.Handler())

	// New expects a bare host:port, not a URL.
	mockHost := strings.TrimPrefix(mockTS.URL, "http://")

	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}

	gw := New(db, mockHost, webhookSecret)

	return gw, mockSrv, func() {
		mockTS.Close()
		db.Close()
	}
}

// gatewayServer wraps the Gateway in an httptest.Server so we can make real
// HTTP calls against it.
func gatewayServer(t *testing.T, gw *Server) *httptest.Server {
	t.Helper()

	return httptest.NewServer(gw.Handler())
}

func TestDB_EncryptDecryptRoundTrip(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	plaintext := "super-secret-access-token-12345"
	encrypted, err := db.encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if encrypted == plaintext {
		t.Fatal("encrypted value should not equal plaintext")
	}

	decrypted, err := db.decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if decrypted != plaintext {
		t.Fatalf("want %q, got %q", plaintext, decrypted)
	}
}

func TestDB_EncryptEmptyString(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	enc, err := db.encrypt("")
	if err != nil {
		t.Fatalf("encrypt empty: %v", err)
	}
	if enc != "" {
		t.Fatal("encrypting empty string should return empty string")
	}
}

func TestDB_SaveAndGetInstance(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	inst := &ConnectorInstance{
		ID:             "inst_test_001",
		ConnectorID:    "slack",
		Status:         "active",
		BaselineSchema: `{"ok":"boolean"}`,
		CreatedAt:      time.Now().UTC().Truncate(time.Second),
	}

	if err := saveInstance(db.conn, inst); err != nil {
		t.Fatalf("saveInstance: %v", err)
	}

	got, err := db.Instance(inst.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.ID != inst.ID || got.ConnectorID != inst.ConnectorID || got.Status != inst.Status {
		t.Fatalf("mismatch: got %+v", got)
	}
}

func TestDB_SaveAndGetCredentials_OAuth2(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	// credentials.instance_id is a foreign key, so the instance must exist first.
	inst := &ConnectorInstance{
		ID: "inst_creds_001", ConnectorID: "slack", Status: "active", CreatedAt: time.Now(),
	}
	creds := &sdk.Credentials{
		Token: &sdk.Token{
			AccessToken:  "access_abc",
			RefreshToken: "refresh_xyz",
			Expiry:       time.Now().Add(1 * time.Hour),
		},
	}

	if err := db.CreateInstance(inst, sdk.AuthTypeOAuth2, creds); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	got, authType, err := db.Credentials("inst_creds_001")
	if err != nil {
		t.Fatalf("GetCredentials: %v", err)
	}
	if authType != sdk.AuthTypeOAuth2 {
		t.Fatalf("want OAuth2, got %s", authType)
	}
	if got.Token.AccessToken != "access_abc" {
		t.Fatalf("want access_abc, got %s", got.Token.AccessToken)
	}
	if got.Token.RefreshToken != "refresh_xyz" {
		t.Fatalf("want refresh_xyz, got %s", got.Token.RefreshToken)
	}
}

func TestDB_ListAndDeleteInstances(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	for i := 0; i < 3; i++ {
		saveInstance(db.conn, &ConnectorInstance{
			ID:          fmt.Sprintf("inst_%d", i),
			ConnectorID: "github",
			Status:      "active",
			CreatedAt:   time.Now(),
		})
	}

	list, err := db.ListInstances()
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 instances, got %d", len(list))
	}

	if err := db.DeleteInstance("inst_0"); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	list, _ = db.ListInstances()
	if len(list) != 2 {
		t.Fatalf("want 2 after delete, got %d", len(list))
	}
}

func TestDB_DeleteInstanceRemovesCredentials(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	inst := &ConnectorInstance{ID: "inst_del", ConnectorID: "stripe", Status: "active", CreatedAt: time.Now()}
	if err := db.CreateInstance(inst, sdk.AuthTypeAPIKey, &sdk.Credentials{APIKey: "sk_test_x"}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if err := db.DeleteInstance(inst.ID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	var rows int
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM credentials WHERE instance_id = ?`, inst.ID).Scan(&rows); err != nil {
		t.Fatalf("count credentials: %v", err)
	}
	if rows != 0 {
		t.Fatalf("want credentials deleted with the instance, %d row(s) left", rows)
	}
	if _, _, err := db.Credentials(inst.ID); err == nil {
		t.Fatal("want error reading credentials of a deleted instance")
	}
}

func TestParseEncryptionKey(t *testing.T) {
	raw := strings.Repeat("k", 32)
	rawBytes := make([]byte, 32)
	for i := range rawBytes {
		rawBytes[i] = byte(i)
	}
	b64 := base64.StdEncoding.EncodeToString(rawBytes) // 44 characters, like `openssl rand -base64 32`

	tests := []struct {
		name   string
		in     string
		want   []byte
		wantOK bool
	}{
		{"base64 of 32 bytes", b64, rawBytes, true},
		{"raw 32 characters", raw, []byte(raw), true},
		{"base64 of 16 bytes", base64.StdEncoding.EncodeToString(rawBytes[:16]), nil, false},
		{"too short", "short", nil, false},
		{"too long", strings.Repeat("k", 33), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseEncryptionKey(tt.in)
			if ok != tt.wantOK || !bytes.Equal(got, tt.want) {
				t.Fatalf("parseEncryptionKey(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestHandleListConnectors(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/connectors")
	if err != nil {
		t.Fatalf("GET /v1/connectors: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var items []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) < 2 {
		t.Fatalf("expected at least 2 connectors (slack, github), got %d", len(items))
	}

	ids := make(map[string]bool)
	for _, item := range items {
		ids[item["id"].(string)] = true
	}
	if !ids["slack"] {
		t.Error("slack connector missing from catalog")
	}
	if !ids["github"] {
		t.Error("github connector missing from catalog")
	}
}

func TestHandleListConnectors_MethodNotAllowed(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/connectors", "application/json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", resp.StatusCode)
	}
}

func TestHandleInstallConnector_ReturnsOAuthRedirect(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/connectors/slack/install", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /v1/connectors/slack/install: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)

	if result["type"] != "oauth2" {
		t.Fatalf("want type=oauth2, got %q", result["type"])
	}
	if result["redirect_url"] == "" {
		t.Fatal("redirect_url should not be empty")
	}

	parsed, err := url.Parse(result["redirect_url"])
	if err != nil {
		t.Fatalf("parse redirect_url: %v", err)
	}
	if parsed.Query().Get("state") == "" {
		t.Error("redirect_url should contain a state parameter")
	}
	if parsed.Query().Get("client_id") == "" {
		t.Error("redirect_url should contain a client_id parameter")
	}
}

func TestHandleInstallConnector_NotFound(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/connectors/nonexistent/install", "application/json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

// TestOAuthCallbackFlow_Slack runs the full OAuth install: start the install
// to obtain a state, complete the callback with it, and check that exactly one
// active Slack instance exists.
func TestOAuthCallbackFlow_Slack(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/connectors/slack/install", "application/json", nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	defer resp.Body.Close()

	var installResult map[string]string
	json.NewDecoder(resp.Body).Decode(&installResult)

	parsed, _ := url.Parse(installResult["redirect_url"])
	state := parsed.Query().Get("state")
	if state == "" {
		t.Fatal("no state in redirect_url")
	}

	// The mock provider accepts any code, so no consent step is needed.
	callbackURL := fmt.Sprintf("%s/oauth/callback?code=mock_code_12345&state=%s", ts.URL, state)
	cbResp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer cbResp.Body.Close()

	if cbResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(cbResp.Body)
		t.Fatalf("callback returned %d: %s", cbResp.StatusCode, body)
	}

	listResp, err := http.Get(ts.URL + "/v1/instances")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer listResp.Body.Close()

	var instances []map[string]interface{}
	json.NewDecoder(listResp.Body).Decode(&instances)

	if len(instances) != 1 {
		t.Fatalf("want 1 instance after install, got %d", len(instances))
	}
	if instances[0]["connector_id"] != "slack" {
		t.Fatalf("want slack connector_id, got %v", instances[0]["connector_id"])
	}
	if instances[0]["status"] != "active" {
		t.Fatalf("want active status, got %v", instances[0]["status"])
	}
}

func TestOAuthCallbackFlow_InvalidState(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/oauth/callback?code=some_code&state=invalid_state_xyz")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid state, got %d", resp.StatusCode)
	}
}

func TestOAuthCallbackFlow_MissingCode(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/oauth/callback?state=somestate")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for missing code, got %d", resp.StatusCode)
	}
}

func TestHandleListInstances_EmptyList(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/instances")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	// Must be JSON array (even if empty), never "null"
	if strings.TrimSpace(string(body)) == "null" {
		t.Fatal("want empty JSON array [], not null")
	}
	if !strings.Contains(string(body), "[") {
		t.Fatalf("want JSON array, got: %s", body)
	}
}

func TestHandleGetInstance(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	inst := &ConnectorInstance{
		ID: "inst_get_test", ConnectorID: "github", Status: "active", CreatedAt: time.Now(),
	}
	saveInstance(gw.db.conn, inst)

	resp, err := http.Get(ts.URL + "/v1/instances/inst_get_test")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var got map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&got)
	if got["id"] != "inst_get_test" {
		t.Fatalf("wrong ID: %v", got["id"])
	}
}

func TestHandleDeleteInstance(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	inst := &ConnectorInstance{
		ID: "inst_del_test", ConnectorID: "slack", Status: "active", CreatedAt: time.Now(),
	}
	saveInstance(gw.db.conn, inst)

	req, _ := http.NewRequest("DELETE", ts.URL+"/v1/instances/inst_del_test", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	getResp, err := http.Get(ts.URL + "/v1/instances/inst_del_test")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("after DELETE want 404, got %d", getResp.StatusCode)
	}
}

// installAndGetInstanceID performs the full OAuth flow and returns the new instance ID.
func installAndGetInstanceID(t *testing.T, ts *httptest.Server, connectorID string) string {
	t.Helper()

	installResp, err := http.Post(ts.URL+"/v1/connectors/"+connectorID+"/install", "application/json", nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	defer installResp.Body.Close()

	var installResult map[string]string
	json.NewDecoder(installResp.Body).Decode(&installResult)

	parsed, _ := url.Parse(installResult["redirect_url"])
	state := parsed.Query().Get("state")

	cbURL := fmt.Sprintf("%s/oauth/callback?code=mock_code_12345&state=%s", ts.URL, state)
	cbResp, _ := http.Get(cbURL)
	cbResp.Body.Close()

	listResp, err := http.Get(ts.URL + "/v1/instances")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer listResp.Body.Close()

	var instances []map[string]interface{}
	json.NewDecoder(listResp.Body).Decode(&instances)

	for _, inst := range instances {
		if inst["connector_id"] == connectorID {
			return inst["id"].(string)
		}
	}

	t.Fatalf("no instance found for connector %s", connectorID)
	return ""
}

func TestHandleExecuteEndpoint_SlackSendMessage(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	instanceID := installAndGetInstanceID(t, ts, "slack")

	payload := `{"channel":"C_TEST_CHANNEL","text":"hello from gateway test"}`
	execURL := fmt.Sprintf("%s/v1/instances/%s/endpoints/send_message", ts.URL, instanceID)
	resp, err := http.Post(execURL, "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST endpoint: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if result["ok"] != true {
		t.Fatalf("want ok=true in Slack response, got: %v", result)
	}
	if result["channel"] != "C_TEST_CHANNEL" {
		t.Fatalf("want channel=C_TEST_CHANNEL, got %v", result["channel"])
	}
}

func TestHandleExecuteEndpoint_UnknownEndpoint(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	instanceID := installAndGetInstanceID(t, ts, "slack")

	execURL := fmt.Sprintf("%s/v1/instances/%s/endpoints/nonexistent_action", ts.URL, instanceID)
	resp, err := http.Post(execURL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for unknown endpoint, got %d", resp.StatusCode)
	}
}

func TestHandleExecuteEndpoint_InstanceNotFound(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/instances/inst_ghost_xyz/endpoints/send_message", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for missing instance, got %d", resp.StatusCode)
	}
}

func TestHandleWebhookPayload_SlackValid(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	instanceID := installAndGetInstanceID(t, ts, "slack")

	payload := []byte(`{"event":"message","text":"hello webhook"}`)
	sig := slackSignature(payload, testWebhookSecret)

	req, _ := http.NewRequest("POST", ts.URL+"/webhooks/"+instanceID, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Signature", sig)
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
}

// A webhook signed with any secret other than the instance's own must be
// rejected, including the mock value that was once hardcoded in the connectors.
func TestHandleWebhookPayload_UsesInstanceSecret(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	instanceID := installAndGetInstanceID(t, ts, "slack")
	payload := []byte(`{"event":"message"}`)

	post := func(secret string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/webhooks/"+instanceID, bytes.NewReader(payload))
		req.Header.Set("X-Slack-Signature", slackSignature(payload, secret))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("webhook request: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("mock_webhook_secret_value"); got != http.StatusUnauthorized {
		t.Errorf("signed with the old built-in secret: want 401, got %d", got)
	}
	if got := post(testWebhookSecret); got != http.StatusOK {
		t.Errorf("signed with the instance secret: want 200, got %d", got)
	}
}

func TestHandleWebhookPayload_InvalidSignature(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	instanceID := installAndGetInstanceID(t, ts, "slack")

	payload := []byte(`{"event":"message"}`)
	req, _ := http.NewRequest("POST", ts.URL+"/webhooks/"+instanceID, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Slack-Signature", "v0=badhex0000000000000000000000000000000000000000000000000000000000")
	req.Header.Set("X-Slack-Request-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 for bad signature, got %d", resp.StatusCode)
	}
}

func TestHandleWebhookPayload_InstanceNotFound(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/webhooks/inst_doesnt_exist", bytes.NewReader([]byte(`{}`)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
}

func TestFlattenJSONSchema_Basic(t *testing.T) {
	input := map[string]interface{}{
		"ok":      true,
		"channel": "C123",
		"ts":      "1234567890.123",
		"message": map[string]interface{}{
			"text": "hello",
			"user": "U123",
		},
	}

	schema := make(map[string]string)
	flattenJSONSchema("", input, schema)

	tests := map[string]string{
		"ok":           "boolean",
		"channel":      "string",
		"ts":           "string",
		"message.text": "string",
		"message.user": "string",
	}

	for key, wantType := range tests {
		if got := schema[key]; got != wantType {
			t.Errorf("key %q: want %q, got %q", key, wantType, got)
		}
	}
}

func TestFlattenJSONSchema_Array(t *testing.T) {
	input := []interface{}{
		map[string]interface{}{
			"id":    float64(1),
			"title": "Issue 1",
			"state": "open",
		},
	}

	schema := make(map[string]string)
	flattenJSONSchema("", input, schema)

	if schema[".[].id"] != "number" {
		t.Errorf("want .[].id = number, got %q", schema[".[].id"])
	}
	if schema[".[].title"] != "string" {
		t.Errorf("want .[].title = string, got %q", schema[".[].title"])
	}
}

func TestFlattenJSONSchema_EmptyArray(t *testing.T) {
	input := []interface{}{}

	schema := make(map[string]string)
	flattenJSONSchema("root", input, schema)

	if schema["root"] != "array[empty]" {
		t.Errorf("want array[empty], got %q", schema["root"])
	}
}

func TestDriftDetector_NoDriftDetected(t *testing.T) {
	// Baseline and live are the same → no drift
	baselineJSON := `{"ok":"boolean","channel":"string","ts":"string"}`

	liveObj := map[string]interface{}{
		"ok":      true,
		"channel": "C123",
		"ts":      "123.456",
	}

	var baseline map[string]string
	json.Unmarshal([]byte(baselineJSON), &baseline)

	live := make(map[string]string)
	flattenJSONSchema("", liveObj, live)

	drifted := false
	for key, expectedType := range baseline {
		actualType, exists := live[key]
		if !exists || actualType != expectedType {
			drifted = true
			break
		}
	}

	if drifted {
		t.Error("expected no drift but drift was detected")
	}
}

func TestDriftDetector_FieldRemoved(t *testing.T) {
	// Baseline has "ts" but live response doesn't
	baselineJSON := `{"ok":"boolean","channel":"string","ts":"string"}`

	liveObj := map[string]interface{}{
		"ok":      true,
		"channel": "C123",
		// "ts" is intentionally missing → drift
	}

	var baseline map[string]string
	json.Unmarshal([]byte(baselineJSON), &baseline)

	live := make(map[string]string)
	flattenJSONSchema("", liveObj, live)

	drifted := false
	var details []string
	for key, expectedType := range baseline {
		actualType, exists := live[key]
		if !exists {
			drifted = true
			details = append(details, fmt.Sprintf("Field %q removed", key))
		} else if actualType != expectedType {
			drifted = true
			details = append(details, fmt.Sprintf("Field %q type changed %s→%s", key, expectedType, actualType))
		}
	}

	if !drifted {
		t.Error("expected drift (missing 'ts') but none detected")
	}

	found := false
	for _, d := range details {
		if strings.Contains(d, `"ts"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ts field in drift details, got: %v", details)
	}
}

func TestDriftDetector_TypeChanged(t *testing.T) {
	// Baseline has "id" as number, live returns it as string (drifted)
	baselineJSON := `{"id":"number","title":"string"}`

	liveObj := map[string]interface{}{
		"id":    "id-as-string", // now a string, was number
		"title": "some title",
	}

	var baseline map[string]string
	json.Unmarshal([]byte(baselineJSON), &baseline)

	live := make(map[string]string)
	flattenJSONSchema("", liveObj, live)

	drifted := false
	for key, expectedType := range baseline {
		actualType, exists := live[key]
		if !exists || actualType != expectedType {
			drifted = true
			_ = key
			break
		}
	}

	if !drifted {
		t.Error("expected drift (id type changed) but none detected")
	}
}

func TestHandleOpenAPISpec(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("want JSON content-type, got %q", ct)
	}

	var spec map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}

	if spec["openapi"] != "3.0.0" {
		t.Fatalf("want openapi=3.0.0, got %v", spec["openapi"])
	}
	if spec["paths"] == nil {
		t.Fatal("paths should not be nil")
	}

	paths := spec["paths"].(map[string]interface{})
	if len(paths) == 0 {
		t.Fatal("expected at least one path in OpenAPI spec")
	}

	found := false
	for path := range paths {
		if strings.Contains(path, "send_message") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected send_message path in OpenAPI spec, paths: %v", func() []string {
			keys := make([]string, 0, len(paths))
			for k := range paths {
				keys = append(keys, k)
			}
			return keys
		}())
	}
}

func TestHandleDocs(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/docs")
	if err != nil {
		t.Fatalf("GET /docs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("want HTML content-type, got %q", ct)
	}
}

func TestSchemaDiff_EmptyArrays(t *testing.T) {
	flatten := func(js string) map[string]string {
		var v interface{}
		if err := json.Unmarshal([]byte(js), &v); err != nil {
			t.Fatalf("bad test JSON %q: %v", js, err)
		}
		m := make(map[string]string)
		flattenJSONSchema("", v, m)
		return m
	}

	tests := []struct {
		name        string
		baseline    string
		live        string
		wantDrifted bool
	}{
		{"empty list at install, items later", `[]`, `[{"id":1,"title":"a"}]`, false},
		{"items at install, empty list later", `[{"id":1,"title":"a"}]`, `[]`, false},
		{"wrapped list, items then empty", `{"data":[{"id":"ch_1"}],"has_more":false}`, `{"data":[],"has_more":false}`, false},
		{"wrapped list, empty then items", `{"data":[],"has_more":false}`, `{"data":[{"id":"ch_1"}],"has_more":false}`, false},
		{"primitive list, items then empty", `{"tags":["a"]}`, `{"tags":[]}`, false},
		{"field removed next to an empty list", `{"data":[],"has_more":false}`, `{"data":[]}`, true},
		{"item field removed", `[{"id":1,"title":"a"}]`, `[{"id":1}]`, true},
		{"top-level type change", `{"ok":true}`, `{"ok":"yes"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schemaDiff(flatten(tt.baseline), flatten(tt.live))
			if (len(got) > 0) != tt.wantDrifted {
				t.Fatalf("wantDrifted=%v, got diff %v", tt.wantDrifted, got)
			}
		})
	}
}

// A drifted instance must raise one alert when it first drifts, not one per scan.
func TestDriftDetector_AlertsOncePerDrift(t *testing.T) {
	var alerts atomic.Int32
	alertSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alerts.Add(1)
	}))
	defer alertSrv.Close()
	t.Setenv("CONDUIT_ALERT_WEBHOOK_URL", alertSrv.URL)

	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	instanceID := installAndGetInstanceID(t, ts, "slack")

	resp, err := http.Get("http://" + gw.mockHost + "/mock/toggle-drift?connector_id=slack&drift=true")
	if err != nil {
		t.Fatalf("toggle drift: %v", err)
	}
	resp.Body.Close()

	scan := func() *ConnectorInstance {
		inst, err := gw.db.Instance(instanceID)
		if err != nil {
			t.Fatalf("Instance: %v", err)
		}
		gw.driftDetector.scanInstance(inst)
		inst, _ = gw.db.Instance(instanceID)
		return inst
	}

	if got := scan(); got.Status != "drifted" {
		t.Fatalf("want status drifted after first scan, got %q", got.Status)
	}
	scan()
	scan()

	// The alert is sent from a goroutine; wait for it, then make sure no
	// second one follows.
	deadline := time.Now().Add(2 * time.Second)
	for alerts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if got := alerts.Load(); got != 1 {
		t.Fatalf("want exactly 1 alert for 3 scans of a drifted instance, got %d", got)
	}
}

func TestOAuthCallback_ExpiredStateRejected(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	gw.pendingOAuth["old_state"] = pendingInstall{connectorID: "slack", started: time.Now().Add(-2 * oauthStateTTL)}

	resp, err := http.Get(ts.URL + "/oauth/callback?code=c&state=old_state")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for an expired state, got %d", resp.StatusCode)
	}

	// Starting a new install drops installs that were never completed.
	gw.pendingOAuth["abandoned"] = pendingInstall{connectorID: "slack", started: time.Now().Add(-2 * oauthStateTTL)}
	gw.rememberOAuthState("fresh", "slack")
	if _, stale := gw.pendingOAuth["abandoned"]; stale {
		t.Error("expired state was not pruned")
	}
	if _, ok := gw.takeOAuthState("fresh"); !ok {
		t.Error("fresh state should be accepted")
	}
	if _, ok := gw.takeOAuthState("fresh"); ok {
		t.Error("a state must work only once")
	}
}

// fakeOAuthConnector is a connector whose token URL points at a test server.
type fakeOAuthConnector struct{ tokenURL string }

func (c fakeOAuthConnector) Metadata() sdk.ConnectorMetadata {
	return sdk.ConnectorMetadata{ID: "fake"}
}
func (c fakeOAuthConnector) AuthConfig() sdk.AuthConfig {
	return sdk.AuthConfig{Type: sdk.AuthTypeOAuth2, OAuth2: &sdk.OAuth2Config{TokenURL: c.tokenURL}}
}
func (c fakeOAuthConnector) Endpoints() []sdk.EndpointDefinition { return nil }
func (c fakeOAuthConnector) HandleWebhook(context.Context, sdk.WebhookEvent) error {
	return nil
}

// The refresh token must reach the provider intact in a form body, whatever
// characters it contains, and must not appear in the URL.
func TestTokenRefreshHook_SendsTokenAsFormBody(t *testing.T) {
	var gotForm url.Values
	var gotQuery string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotForm, gotQuery = r.PostForm, r.URL.RawQuery
		w.Write([]byte(`{"access_token":"new_access","refresh_token":"new_refresh","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	gw, _, cleanup := newTestGateway(t)
	defer cleanup()

	const instanceID = "inst_fake_1"
	inst := &ConnectorInstance{ID: instanceID, ConnectorID: "fake", Status: "active", CreatedAt: time.Now()}
	if err := gw.db.CreateInstance(inst, sdk.AuthTypeOAuth2, &sdk.Credentials{Token: &sdk.Token{AccessToken: "old"}}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	hook := gw.tokenRefreshHook(instanceID, fakeOAuthConnector{tokenURL: tokenSrv.URL})
	token, err := hook(context.Background(), &sdk.Token{RefreshToken: "a&b=c#d e"})
	if err != nil {
		t.Fatalf("hook: %v", err)
	}

	if got := gotForm.Get("refresh_token"); got != "a&b=c#d e" {
		t.Errorf("refresh_token = %q, want it unchanged", got)
	}
	if got := gotForm.Get("grant_type"); got != "refresh_token" {
		t.Errorf("grant_type = %q", got)
	}
	if gotQuery != "" {
		t.Errorf("token leaked into the URL: %q", gotQuery)
	}
	if token.AccessToken != "new_access" {
		t.Errorf("access token = %q", token.AccessToken)
	}
}

// checkDrift probes instances concurrently; every drifted instance must still
// be flagged.
func TestDriftDetector_CheckDriftScansAllInstances(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	ids := []string{installAndGetInstanceID(t, ts, "slack"), installAndGetInstanceID(t, ts, "github")}
	for _, c := range []string{"slack", "github"} {
		resp, err := http.Get("http://" + gw.mockHost + "/mock/toggle-drift?connector_id=" + c + "&drift=true")
		if err != nil {
			t.Fatalf("toggle drift: %v", err)
		}
		resp.Body.Close()
	}

	gw.driftDetector.checkDrift()

	for _, id := range ids {
		inst, err := gw.db.Instance(id)
		if err != nil {
			t.Fatalf("Instance(%s): %v", id, err)
		}
		if inst.Status != "drifted" {
			t.Errorf("instance %s: want status drifted, got %q", id, inst.Status)
		}
	}
}

// A drift scan that finishes after the instance was deleted must not recreate it.
func TestSetInstanceStatus_DoesNotResurrectDeletedInstance(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	inst := &ConnectorInstance{ID: "inst_gone", ConnectorID: "github", Status: "active", CreatedAt: time.Now()}
	if err := saveInstance(db.conn, inst); err != nil {
		t.Fatalf("saveInstance: %v", err)
	}
	if found, err := db.SetInstanceStatus(inst.ID, "drifted"); err != nil || !found {
		t.Fatalf("SetInstanceStatus = %v, %v; want true, nil", found, err)
	}
	if got, _ := db.Instance(inst.ID); got == nil || got.Status != "drifted" {
		t.Fatalf("want status drifted, got %+v", got)
	}

	if err := db.DeleteInstance(inst.ID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if found, err := db.SetInstanceStatus(inst.ID, "active"); err != nil || found {
		t.Fatalf("SetInstanceStatus after delete = %v, %v; want false, nil", found, err)
	}
	if _, err := db.Instance(inst.ID); err == nil {
		t.Fatal("deleted instance came back")
	}
}

// Databases from before the secret moved out of the instances table must have their
// stored plain-text secrets cleared on open.
func TestNewDB_ClearsLegacyWebhookSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	if _, err := db.conn.Exec(`ALTER TABLE connector_instances ADD COLUMN webhook_secret TEXT;`); err != nil {
		t.Fatalf("add legacy column: %v", err)
	}
	if _, err := db.conn.Exec(`INSERT INTO connector_instances (id, connector_id, status, webhook_secret, created_at) VALUES ('inst_old', 'slack', 'active', 'plain-secret', ?);`, time.Now()); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	db.Close()

	db, err = NewDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	var stored sql.NullString
	if err := db.conn.QueryRow(`SELECT webhook_secret FROM connector_instances WHERE id = 'inst_old';`).Scan(&stored); err != nil {
		t.Fatalf("read legacy column: %v", err)
	}
	if stored.Valid {
		t.Fatalf("plain-text secret still stored: %q", stored.String)
	}
}

// If credentials cannot be saved, the install must fail and leave no instance behind.
func TestInstall_RollsBackWhenCredentialsFail(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	if _, err := gw.db.conn.Exec(`DROP TABLE credentials;`); err != nil {
		t.Fatalf("drop credentials: %v", err)
	}

	resp, err := http.Post(ts.URL+"/v1/connectors/stripe/install", "application/json", strings.NewReader(`{"api_key":"sk_test_x"}`))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", resp.StatusCode)
	}

	list, err := gw.db.ListInstances()
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("want no instances after a failed install, got %d", len(list))
	}
}

func TestRememberOAuthState_CapsPendingInstalls(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()

	for i := 0; i < maxPendingInstalls; i++ {
		if !gw.rememberOAuthState(fmt.Sprintf("state_%d", i), "slack") {
			t.Fatalf("install %d refused below the cap", i)
		}
	}
	if gw.rememberOAuthState("one_too_many", "slack") {
		t.Fatal("install accepted above the cap")
	}

	ts := gatewayServer(t, gw)
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/connectors/slack/install", "application/json", nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when full, got %d", resp.StatusCode)
	}
}

// A token refresh that finishes after the instance was deleted must not store the
// new tokens again.
func TestUpdateCredentials_DoesNotRecreateDeletedCredentials(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	inst := &ConnectorInstance{ID: "inst_refresh", ConnectorID: "slack", Status: "active", CreatedAt: time.Now()}
	if err := db.CreateInstance(inst, sdk.AuthTypeOAuth2, &sdk.Credentials{Token: &sdk.Token{AccessToken: "old"}}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	newCreds := &sdk.Credentials{Token: &sdk.Token{AccessToken: "new", RefreshToken: "r"}}
	if err := db.UpdateCredentials(inst.ID, sdk.AuthTypeOAuth2, newCreds); err != nil {
		t.Fatalf("UpdateCredentials: %v", err)
	}
	if got, _, _ := db.Credentials(inst.ID); got == nil || got.Token.AccessToken != "new" {
		t.Fatalf("want updated token, got %+v", got)
	}

	if err := db.DeleteInstance(inst.ID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if err := db.UpdateCredentials(inst.ID, sdk.AuthTypeOAuth2, newCreds); !errors.Is(err, errNoCredentials) {
		t.Fatalf("UpdateCredentials after delete = %v, want errNoCredentials", err)
	}
	var rows int
	db.conn.QueryRow(`SELECT COUNT(*) FROM credentials WHERE instance_id = ?`, inst.ID).Scan(&rows)
	if rows != 0 {
		t.Fatalf("credentials stored again for a deleted instance")
	}
}

// A scan that finds drift on an instance deleted mid-probe must neither alert nor
// bring the instance back.
func TestDriftDetector_NoAlertForDeletedInstance(t *testing.T) {
	var alerts atomic.Int32
	alertSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alerts.Add(1)
	}))
	defer alertSrv.Close()
	t.Setenv("CONDUIT_ALERT_WEBHOOK_URL", alertSrv.URL)

	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	instanceID := installAndGetInstanceID(t, ts, "slack")
	resp, err := http.Get("http://" + gw.mockHost + "/mock/toggle-drift?connector_id=slack&drift=true")
	if err != nil {
		t.Fatalf("toggle drift: %v", err)
	}
	resp.Body.Close()

	inst, err := gw.db.Instance(instanceID)
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	// Simulate a delete that lands while the probe is running: the scan has already
	// loaded the instance and its credentials, then the instance row disappears.
	if _, err := gw.db.conn.Exec(`DELETE FROM connector_instances WHERE id = ?;`, instanceID); err != nil {
		t.Fatalf("delete instance row: %v", err)
	}

	gw.driftDetector.scanInstance(inst)

	time.Sleep(200 * time.Millisecond)
	if got := alerts.Load(); got != 0 {
		t.Fatalf("want no alert for a deleted instance, got %d", got)
	}
	if _, err := gw.db.Instance(instanceID); err == nil {
		t.Fatal("deleted instance came back")
	}
}

func TestHandleWebhookPayload_RejectsOversizedBody(t *testing.T) {
	gw, _, cleanup := newTestGateway(t)
	defer cleanup()
	ts := gatewayServer(t, gw)
	defer ts.Close()

	instanceID := installAndGetInstanceID(t, ts, "slack")
	big := bytes.Repeat([]byte("a"), maxBodyBytes+1)

	resp, err := http.Post(ts.URL+"/webhooks/"+instanceID, "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("webhook request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", resp.StatusCode)
	}
}
