package github_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"conduit/connectors/github"
	"conduit/sdk"
)

func signature(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubWebhook(t *testing.T) {
	conn := github.New("")
	payload := []byte(`{"action":"opened"}`)
	const secret = "instance_secret"

	tests := []struct {
		name    string
		headers map[string]string
		secret  string
		wantErr bool
	}{
		{"valid", map[string]string{"X-Hub-Signature-256": signature(payload, secret)}, secret, false},
		{"signed with another secret", map[string]string{"X-Hub-Signature-256": signature(payload, "other")}, secret, true},
		{"instance has no secret", map[string]string{"X-Hub-Signature-256": signature(payload, secret)}, "", true},
		{"missing header", map[string]string{}, secret, true},
		{"malformed signature", map[string]string{"X-Hub-Signature-256": "sha256=zz"}, secret, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := conn.HandleWebhook(context.Background(), sdk.WebhookEvent{
				Payload: payload,
				Headers: tt.headers,
				Secret:  []byte(tt.secret),
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("wantErr=%v, got %v", tt.wantErr, err)
			}
		})
	}
}

// owner and repo come from the caller, so they must not be able to change
// which provider path is requested.
func TestGitHubListIssues_EscapesOwnerAndRepo(t *testing.T) {
	var gotPath, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		w.Write([]byte(`[]`))
	}))
	defer ts.Close()

	conn := github.New("")
	client := sdk.NewHTTPClient(ts.URL, sdk.Credentials{Token: &sdk.Token{AccessToken: "t"}}, conn.AuthConfig(), nil)

	var list sdk.EndpointDefinition
	for _, ep := range conn.Endpoints() {
		if ep.Name == "list_issues" {
			list = ep
		}
	}

	_, err := list.Execute(context.Background(), client, sdk.Request{
		Params: map[string]string{"owner": "../../user", "repo": "r?x=1"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	const wantPath = "/services/github/repos/..%2F..%2Fuser/r%3Fx=1/issues"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if gotQuery != "" {
		t.Errorf("owner/repo leaked into the query string: %q", gotQuery)
	}
}
