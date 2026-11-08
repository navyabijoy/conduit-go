package slack_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"conduit/connectors/slack"
	"conduit/sdk"
)

func signature(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSlackWebhook(t *testing.T) {
	conn := slack.New("")
	payload := []byte(`{"type":"event_callback"}`)
	const secret = "instance_secret"

	tests := []struct {
		name    string
		headers map[string]string
		secret  string
		wantErr bool
	}{
		{"valid", map[string]string{"X-Slack-Signature": signature(payload, secret)}, secret, false},
		{"signed with another secret", map[string]string{"X-Slack-Signature": signature(payload, "other")}, secret, true},
		{"instance has no secret", map[string]string{"X-Slack-Signature": signature(payload, secret)}, "", true},
		{"missing header", map[string]string{}, secret, true},
		{"malformed signature", map[string]string{"X-Slack-Signature": "v0=zz"}, secret, true},
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
