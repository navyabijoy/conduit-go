package mock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMockServerOAuthAndAPI(t *testing.T) {
	s := NewServer("127.0.0.1:0", "webhook_secret_test")

	mux := http.NewServeMux()
	mux.HandleFunc("/mock/oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("/services/slack/oauth/token", s.handleSlackToken)
	mux.HandleFunc("/services/github/login/oauth/access_token", s.handleGitHubToken)
	mux.HandleFunc("/services/slack/chat.postMessage", s.handleSlackPostMessage)
	mux.HandleFunc("/services/github/repos/", s.handleGitHubIssues)
	mux.HandleFunc("/mock/toggle-drift", s.handleToggleDrift)
	mux.HandleFunc("/mock/toggle-rate-limit", s.handleToggleRateLimit)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	postMessage := func(t *testing.T) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", ts.URL+"/services/slack/chat.postMessage", bytes.NewBufferString(`{"channel":"C123","text":"hello"}`))
		req.Header.Set("Authorization", "Bearer mock_slack_token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("failed to call chat.postMessage: %v", err)
		}
		return resp
	}

	t.Run("slack token exchange", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/services/slack/oauth/token", "application/json", nil)
		if err != nil {
			t.Fatalf("failed to call token: %v", err)
		}
		var slackToken map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&slackToken)
		resp.Body.Close()

		if slackToken["access_token"] == nil || !strings.Contains(slackToken["access_token"].(string), "mock_slack_access_token_") {
			t.Errorf("unexpected access token: %v", slackToken)
		}
	})

	t.Run("slack post message", func(t *testing.T) {
		resp := postMessage(t)
		var slackPost map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&slackPost)
		resp.Body.Close()

		if slackPost["ok"] != true || slackPost["channel"] != "C123" {
			t.Errorf("unexpected post response: %v", slackPost)
		}
	})

	t.Run("slack rate limit returns 429 with Retry-After", func(t *testing.T) {
		http.Post(fmt.Sprintf("%s/mock/toggle-rate-limit?connector_id=slack&rate_limit=true", ts.URL), "application/json", nil)
		defer http.Post(fmt.Sprintf("%s/mock/toggle-rate-limit?connector_id=slack&rate_limit=false", ts.URL), "application/json", nil)

		resp := postMessage(t)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("expected 429 too many requests, got %d", resp.StatusCode)
		}
		if resp.Header.Get("Retry-After") != "1" {
			t.Errorf("expected Retry-After: 1 header, got %q", resp.Header.Get("Retry-After"))
		}
	})

	t.Run("slack drift renames channel field", func(t *testing.T) {
		http.Post(fmt.Sprintf("%s/mock/toggle-drift?connector_id=slack&drift=true", ts.URL), "application/json", nil)

		resp := postMessage(t)
		var slackDrift map[string]interface{}
		json.NewDecoder(resp.Body).Decode(&slackDrift)
		resp.Body.Close()

		if slackDrift["channel_id_drifted"] != "C123" || slackDrift["channel"] != nil {
			t.Errorf("expected drifted schema response, got: %v", slackDrift)
		}
	})
}

func TestHandleAuthorize_EscapesQueryValues(t *testing.T) {
	s := NewServer("", "secret")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	q := url.Values{}
	q.Set("client_id", `"><script>alert(1)</script>`)
	q.Set("scope", `<b>read</b>`)
	q.Set("state", `a&b"c`)
	q.Set("redirect_uri", `http://localhost:8080/oauth/callback?x="y`)

	resp, err := http.Get(ts.URL + "/mock/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	for _, raw := range []string{"<script>", "<b>read</b>", `x="y`, `a&b"c`} {
		if strings.Contains(string(body), raw) {
			t.Errorf("page contains unescaped %q", raw)
		}
	}
}

func TestHandleAuthorize_RejectsNonWebRedirect(t *testing.T) {
	s := NewServer("", "secret")
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, target := range []string{"javascript:alert(1)", "", "data:text/html,x"} {
		q := url.Values{"redirect_uri": {target}, "state": {"s"}}
		resp, err := http.Get(ts.URL + "/mock/oauth/authorize?" + q.Encode())
		if err != nil {
			t.Fatalf("authorize: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("redirect_uri %q: want 400, got %d", target, resp.StatusCode)
		}
	}
}
