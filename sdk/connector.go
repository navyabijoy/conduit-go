package sdk

import "context"

// Connector is implemented by every SaaS integration the gateway serves.
type Connector interface {
	Metadata() ConnectorMetadata
	AuthConfig() AuthConfig
	Endpoints() []EndpointDefinition
	HandleWebhook(ctx context.Context, event WebhookEvent) error
}

// ConnectorMetadata is the catalog entry users see before installing.
type ConnectorMetadata struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Scopes      []string `json:"scopes"`
}

// AuthType selects how HTTPClient authenticates requests.
type AuthType string

const (
	AuthTypeOAuth2 AuthType = "oauth2"
	AuthTypeAPIKey AuthType = "apikey"
)

// AuthConfig describes a connector's auth; set OAuth2 or APIKey to match Type.
type AuthConfig struct {
	Type   AuthType      `json:"type"`
	OAuth2 *OAuth2Config `json:"oauth2,omitempty"`
	APIKey *APIKeyConfig `json:"apikey,omitempty"`
}

// OAuth2Config holds the provider's authorize and token URLs and the install scopes.
type OAuth2Config struct {
	AuthURL  string   `json:"auth_url"`
	TokenURL string   `json:"token_url"`
	Scopes   []string `json:"scopes"`
}

// APIKeyConfig names the header that carries the key and an optional value prefix
// such as "Bearer ".
type APIKeyConfig struct {
	HeaderName  string `json:"header_name"`
	ValuePrefix string `json:"value_prefix"`
}

// Request is the input to Execute: the raw body and the first value of each query
// parameter.
type Request struct {
	Body   []byte
	Params map[string]string
}

// Response is the provider's body and status, passed through unchanged.
type Response struct {
	Body       []byte
	StatusCode int
}

// EndpointDefinition is one operation a connector exposes. The schemas are example
// values reflected into the OpenAPI spec; GetPaginator is nil for unpaged endpoints.
type EndpointDefinition struct {
	Name         string
	Method       string
	Path         string
	Description  string
	InputSchema  interface{}
	OutputSchema interface{}
	Execute      func(ctx context.Context, client *HTTPClient, req Request) (Response, error)
	GetPaginator func(path string, params map[string]string, limit int) Paginator
}

// WebhookEvent is an inbound webhook. Payload must stay byte-for-byte as received
// or signature checks fail; Secret is the instance's signing secret.
type WebhookEvent struct {
	Payload   []byte
	Headers   map[string]string
	EventType string
	Secret    []byte
}
