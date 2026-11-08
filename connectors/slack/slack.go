// Package slack is the Slack connector: OAuth2 install, chat.postMessage, and an
// X-Slack-Signature check that matches the mock, not real Slack (see HandleWebhook).
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"conduit/sdk"
)

// Connector is the Slack connector.
type Connector struct {
	mockBaseURL string
}

// New returns a Slack connector; an empty mockBaseURL means the mock provider at
// localhost:8081.
func New(mockBaseURL string) *Connector {
	if mockBaseURL == "" {
		mockBaseURL = "localhost:8081"
	}
	return &Connector{mockBaseURL: mockBaseURL}
}

// SendMessageInput is the request body for the send_message endpoint.
type SendMessageInput struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// SendMessageOutput is Slack's chat.postMessage response.
type SendMessageOutput struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel"`
	TS      string `json:"ts,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (c *Connector) Metadata() sdk.ConnectorMetadata {
	return sdk.ConnectorMetadata{
		ID:          "slack",
		Name:        "Slack",
		Description: "Send messages and receive events from Slack workspaces.",
		Category:    "Communication",
		Scopes:      []string{"chat:write", "channels:read", "incoming-webhook"},
	}
}

func (c *Connector) AuthConfig() sdk.AuthConfig {
	return sdk.AuthConfig{
		Type: sdk.AuthTypeOAuth2,
		OAuth2: &sdk.OAuth2Config{
			AuthURL:  fmt.Sprintf("http://%s/mock/oauth/authorize", c.mockBaseURL),
			TokenURL: fmt.Sprintf("http://%s/services/slack/oauth/token", c.mockBaseURL),
			Scopes:   []string{"chat:write", "channels:read"},
		},
	}
}

func (c *Connector) Endpoints() []sdk.EndpointDefinition {
	return []sdk.EndpointDefinition{
		{
			Name:         "send_message",
			Method:       "POST",
			Path:         "/services/slack/chat.postMessage",
			Description:  "Send a message to a channel.",
			InputSchema:  &SendMessageInput{},
			OutputSchema: &SendMessageOutput{},
			Execute:      c.executeSendMessage,
		},
	}
}

func (c *Connector) executeSendMessage(ctx context.Context, client *sdk.HTTPClient, req sdk.Request) (sdk.Response, error) {
	var input SendMessageInput
	if err := json.Unmarshal(req.Body, &input); err != nil {
		return sdk.Response{}, sdk.NewPermanentError("invalid JSON body", http.StatusBadRequest, err)
	}

	if input.Channel == "" || input.Text == "" {
		return sdk.Response{}, sdk.NewPermanentError("channel and text are required fields", http.StatusBadRequest, nil)
	}

	payloadBytes, err := json.Marshal(input)
	if err != nil {
		return sdk.Response{}, sdk.NewPermanentError("failed to marshal request", http.StatusInternalServerError, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", "/services/slack/chat.postMessage", strings.NewReader(string(payloadBytes)))
	if err != nil {
		return sdk.Response{}, sdk.NewPermanentError("failed to create HTTP request", http.StatusInternalServerError, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return sdk.Response{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sdk.Response{}, sdk.NewTransientError("failed to read response body", resp.StatusCode, err)
	}

	return sdk.Response{
		Body:       body,
		StatusCode: resp.StatusCode,
	}, nil
}

// HandleWebhook checks "v0=" plus the hex HMAC-SHA256 of the body under event.Secret.
// Real Slack signs "v0:{timestamp}:{body}".
func (c *Connector) HandleWebhook(ctx context.Context, event sdk.WebhookEvent) error {
	sig := event.Headers["X-Slack-Signature"]
	if sig == "" {
		return sdk.NewPermanentError("missing Slack signature header", http.StatusBadRequest, nil)
	}

	// TODO: Sign Slack's real basestring and reject replays via X-Slack-Request-Timestamp.
	if !sdk.VerifyHMACSHA256HexSignature(event.Payload, sig, "v0=", event.Secret) {
		return sdk.NewPermanentError("invalid webhook signature", http.StatusUnauthorized, nil)
	}
	return nil
}
