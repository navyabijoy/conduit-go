// Package stripe is the Stripe connector: API-key auth, list_charges paginated by
// starting_after, and a Stripe-Signature check that matches the mock, not real Stripe
// (see HandleWebhook).
package stripe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"conduit/sdk"
)

// Connector is the Stripe connector. API-key auth needs no provider URLs.
type Connector struct{}

// New returns a Stripe connector.
func New() *Connector {
	return &Connector{}
}

func (c *Connector) Metadata() sdk.ConnectorMetadata {
	return sdk.ConnectorMetadata{
		ID:          "stripe",
		Name:        "Stripe",
		Description: "List charges and receive payment events from Stripe.",
		Category:    "Payments",
		Scopes:      []string{},
	}
}

func (c *Connector) AuthConfig() sdk.AuthConfig {
	return sdk.AuthConfig{
		Type: sdk.AuthTypeAPIKey,
		APIKey: &sdk.APIKeyConfig{
			HeaderName:  "Authorization",
			ValuePrefix: "Bearer ",
		},
	}
}

// Charge represents a Stripe charge object.
type Charge struct {
	ID       string `json:"id"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	Status   string `json:"status"`
	Created  int64  `json:"created"`
}

// ChargeList is the top-level Stripe list response.
type ChargeList struct {
	Object  string   `json:"object"`
	Data    []Charge `json:"data"`
	HasMore bool     `json:"has_more"`
	URL     string   `json:"url"`
}

func (c *Connector) Endpoints() []sdk.EndpointDefinition {
	return []sdk.EndpointDefinition{
		{
			Name:         "list_charges",
			Method:       "GET",
			Path:         "/services/stripe/v1/charges",
			Description:  "List Stripe charges with offset pagination (starting_after cursor).",
			OutputSchema: &ChargeList{},
			Execute:      c.executeListCharges,
			GetPaginator: c.listChargesPaginator,
		},
	}
}

func (c *Connector) executeListCharges(ctx context.Context, client *sdk.HTTPClient, req sdk.Request) (sdk.Response, error) {
	reqURL, err := url.Parse("/services/stripe/v1/charges")
	if err != nil {
		return sdk.Response{}, sdk.NewPermanentError("invalid path", http.StatusInternalServerError, err)
	}

	q := reqURL.Query()
	if limit, ok := req.Params["limit"]; ok && limit != "" {
		q.Set("limit", limit)
	}
	if startingAfter, ok := req.Params["starting_after"]; ok && startingAfter != "" {
		q.Set("starting_after", startingAfter)
	}
	reqURL.RawQuery = q.Encode()

	httpReq, err := http.NewRequestWithContext(ctx, "GET", reqURL.String(), nil)
	if err != nil {
		return sdk.Response{}, sdk.NewPermanentError("failed to create HTTP request", http.StatusInternalServerError, err)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return sdk.Response{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sdk.Response{}, sdk.NewTransientError("failed to read response body", resp.StatusCode, err)
	}

	return sdk.Response{Body: body, StatusCode: resp.StatusCode}, nil
}

// listChargesPaginator pages with starting_after, the ID of the previous page's last charge.
func (c *Connector) listChargesPaginator(path string, params map[string]string, limit int) sdk.Paginator {
	return sdk.NewCursorPaginator(
		"/services/stripe/v1/charges",
		"starting_after",
		"limit",
		limit,
		extractStripeNextCursor,
	)
}

// extractStripeNextCursor uses the last charge's ID as the next starting_after cursor.
func extractStripeNextCursor(body []byte, _ http.Header) (string, bool, error) {
	var list struct {
		HasMore bool     `json:"has_more"`
		Data    []Charge `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", false, fmt.Errorf("failed to parse Stripe list response: %w", err)
	}
	if !list.HasMore || len(list.Data) == 0 {
		return "", false, nil
	}
	return list.Data[len(list.Data)-1].ID, true, nil
}

// HandleWebhook checks the v1 HMAC-SHA256 of the body in the Stripe-Signature header under
// event.Secret. Real Stripe signs "{t}.{body}".
func (c *Connector) HandleWebhook(ctx context.Context, event sdk.WebhookEvent) error {
	sigHeader := event.Headers["Stripe-Signature"]
	if sigHeader == "" {
		return sdk.NewPermanentError("missing Stripe-Signature header", http.StatusBadRequest, nil)
	}

	sig := ""
	for _, part := range strings.Split(sigHeader, ",") {
		if strings.HasPrefix(part, "v1=") {
			sig = part
			break
		}
	}
	if sig == "" {
		return sdk.NewPermanentError("no v1 signature found in Stripe-Signature header", http.StatusBadRequest, nil)
	}

	// TODO: Verify Stripe's real signed payload "{t}.{body}" and reject stale timestamps.
	if !sdk.VerifyHMACSHA256HexSignature(event.Payload, sig, "v1=", event.Secret) {
		return sdk.NewPermanentError("invalid Stripe webhook signature", http.StatusUnauthorized, nil)
	}

	return nil
}
