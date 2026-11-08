package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"conduit/sdk"
)

// handleWebhookPayload hands the raw body to the connector, which verifies the signature;
// a failure is a 401.
func (s *Server) handleWebhookPayload(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	instanceID := strings.TrimPrefix(r.URL.Path, "/webhooks/")
	if instanceID == "" || strings.Contains(instanceID, "/") {
		writeJSONError(w, http.StatusBadRequest, "Invalid instance ID")
		return
	}

	inst, err := s.db.Instance(instanceID)
	if err != nil {
		recordWebhookFailure("unknown", "instance_not_found")
		writeJSONError(w, http.StatusNotFound, "Instance not found")
		return
	}

	conn, ok := s.registry[inst.ConnectorID]
	if !ok {
		recordWebhookFailure(inst.ConnectorID, "connector_not_found")
		writeJSONError(w, http.StatusInternalServerError, "Connector not registered")
		return
	}

	// The body must reach the connector byte for byte; re-encoding would break the HMAC.
	payloadBytes, status, err := readBody(w, r)
	if err != nil {
		recordWebhookFailure(inst.ConnectorID, "cannot_read_body")
		writeJSONError(w, status, "Cannot read request body")
		return
	}

	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	event := sdk.WebhookEvent{
		Payload: payloadBytes,
		Headers: headers,
		Secret:  []byte(s.webhookSecret),
		// TODO: Parse the provider's event type instead of a fixed value.
		EventType: "webhook",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := conn.HandleWebhook(ctx, event); err != nil {
		slog.Warn("webhook verification failed", "component", "gateway", "instance_id", instanceID, "connector", inst.ConnectorID, "err", err)

		reason := "verification_failed"
		if strings.Contains(err.Error(), "signature") {
			reason = "signature_mismatch"
		}
		recordWebhookFailure(inst.ConnectorID, reason)

		writeJSONError(w, http.StatusUnauthorized, "Webhook verification failed: "+err.Error())
		return
	}

	slog.Info("webhook delivered", "component", "gateway", "instance_id", instanceID, "connector", inst.ConnectorID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"received"}`))
}
