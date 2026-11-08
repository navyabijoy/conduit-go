// conduit-scaffold generates a Conduit connector skeleton from an OpenAPI spec.
//
// Usage:
//
//	conduit-scaffold -spec <path-or-url> -out <output-dir> [-connector-id <id>]
//
// With GEMINI_API_KEY set it asks Gemini for the connector; otherwise it renders a template.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"google.golang.org/genai"
)

func main() {
	specPath := flag.String("spec", "", "Path to an OpenAPI 3.x JSON spec file (required)")
	outDir := flag.String("out", "./connector_out", "Output directory for the generated connector")
	connID := flag.String("connector-id", "", "Connector ID (defaults to the OpenAPI info.title snake_cased)")
	flag.Parse()

	if *specPath == "" {
		flag.Usage()
		os.Exit(1)
	}

	spec, err := parseOpenAPISpec(*specPath)
	if err != nil {
		fatal("parse spec failed", "path", *specPath, "err", err)
	}

	if *connID == "" {
		*connID = toSnakeCase(spec.Info.Title)
	}

	slog.Info("parsed spec", "component", "scaffold", "title", spec.Info.Title, "paths", len(spec.Paths))

	apiKey := os.Getenv("GEMINI_API_KEY")
	var code string

	if apiKey != "" {
		slog.Info("GEMINI_API_KEY set, using AI-assisted generation", "component", "scaffold")
		code, err = generateWithGenAI(apiKey, spec, *connID)
		if err != nil {
			slog.Warn("AI-assisted generation failed, falling back to the template generator", "component", "scaffold", "err", err)
			code = generateWithTemplate(spec, *connID)
		}
	} else {
		slog.Info("GEMINI_API_KEY not set, using the template generator", "component", "scaffold")
		code = generateWithTemplate(spec, *connID)
	}

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		fatal("create output dir failed", "dir", *outDir, "err", err)
	}

	outFile := filepath.Join(*outDir, *connID+".go")
	if err := os.WriteFile(outFile, []byte(code), 0644); err != nil {
		fatal("write output file failed", "path", outFile, "err", err)
	}

	slog.Info("connector written", "component", "scaffold", "path", outFile)
	fmt.Printf("\nNext steps:\n  1. Move %s into conduit/connectors/%s/\n  2. Implement the Execute function bodies\n  3. Register the connector in gateway/server.go\n\n", outFile, *connID)
}

func fatal(msg string, args ...any) {
	slog.Error(msg, append([]any{"component", "scaffold"}, args...)...)
	os.Exit(1)
}

// OpenAPISpec is the subset of an OpenAPI 3 document the scaffolder reads.
type OpenAPISpec struct {
	OpenAPI string          `json:"openapi"`
	Info    OpenAPIInfo     `json:"info"`
	Paths   OpenAPIPaths    `json:"paths"`
	Servers []OpenAPIServer `json:"servers"`
}

type OpenAPIInfo struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Version     string `json:"version"`
}

type OpenAPIServer struct {
	URL string `json:"url"`
}

// OpenAPIPaths maps path to lowercase HTTP method to operation.
type OpenAPIPaths map[string]map[string]OpenAPIOperation

type OpenAPIOperation struct {
	OperationID string                 `json:"operationId"`
	Summary     string                 `json:"summary"`
	Description string                 `json:"description"`
	Parameters  []OpenAPIParam         `json:"parameters"`
	RequestBody *OpenAPIRequestBody    `json:"requestBody"`
	Responses   map[string]interface{} `json:"responses"`
}

type OpenAPIParam struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
}

type OpenAPIRequestBody struct {
	Required bool `json:"required"`
}

// EndpointSummary is one operation flattened for the code generators.
type EndpointSummary struct {
	Name        string
	Method      string
	Path        string
	Description string
	HasBody     bool
	PathParams  []string
	QueryParams []string
}

func parseOpenAPISpec(path string) (*OpenAPISpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	var spec OpenAPISpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return &spec, nil
}

// extractEndpoints flattens spec into one summary per path and method, naming operations
// without an operationId from method and path. Order is not stable.
func extractEndpoints(spec *OpenAPISpec) []EndpointSummary {
	var eps []EndpointSummary
	for path, methods := range spec.Paths {
		for method, op := range methods {
			name := op.OperationID
			if name == "" {
				name = toSnakeCase(method + "_" + path)
			}

			ep := EndpointSummary{
				Name:        name,
				Method:      strings.ToUpper(method),
				Path:        path,
				Description: op.Summary,
				HasBody:     op.RequestBody != nil && op.RequestBody.Required,
			}
			for _, p := range op.Parameters {
				if p.In == "path" {
					ep.PathParams = append(ep.PathParams, p.Name)
				} else if p.In == "query" {
					ep.QueryParams = append(ep.QueryParams, p.Name)
				}
			}
			eps = append(eps, ep)
		}
	}
	return eps
}

const connectorTemplate = `// Code generated by conduit-scaffold on {{.Timestamp}}.
// Source: {{.SpecTitle}} {{.SpecVersion}}
// IMPORTANT: Implement the Execute function bodies before using in production.
package {{.PackageName}}

import (
	"context"
	"io"
	"net/http"
	"strings"

	"conduit/sdk"
)

// {{.TypeName}}Connector implements sdk.Connector for {{.SpecTitle}}.
type {{.TypeName}}Connector struct {
	baseURL string
}

// New{{.TypeName}}Connector creates a new {{.SpecTitle}} connector.
// baseURL is the API base URL (e.g. "https://api.example.com").
func New{{.TypeName}}Connector(baseURL string) *{{.TypeName}}Connector {
	return &{{.TypeName}}Connector{baseURL: baseURL}
}

func (c *{{.TypeName}}Connector) Metadata() sdk.ConnectorMetadata {
	return sdk.ConnectorMetadata{
		ID:          "{{.ConnectorID}}",
		Name:        "{{.SpecTitle}}",
		Description: "{{.SpecDescription}}",
		Category:    "Generated",
		Scopes:      []string{},
	}
}

func (c *{{.TypeName}}Connector) AuthConfig() sdk.AuthConfig {
	// TODO: choose the correct auth type and fill in the fields.
	return sdk.AuthConfig{
		Type: sdk.AuthTypeAPIKey,
		APIKey: &sdk.APIKeyConfig{
			HeaderName:  "Authorization",
			ValuePrefix: "Bearer ",
		},
	}
}

func (c *{{.TypeName}}Connector) Endpoints() []sdk.EndpointDefinition {
	return []sdk.EndpointDefinition{
{{- range .Endpoints}}
		{
			Name:        "{{.Name}}",
			Method:      "{{.Method}}",
			Path:        "{{.Path}}",
			Description: "{{.Description}}",
			Execute:     c.execute{{.FuncName}},
		},
{{- end}}
	}
}
{{range .Endpoints}}
func (c *{{$.TypeName}}Connector) execute{{.FuncName}}(ctx context.Context, client *sdk.HTTPClient, req sdk.Request) (sdk.Response, error) {
	// TODO: implement {{.Name}} ({{.Method}} {{.Path}})
	path := "{{.Path}}"
{{- range .PathParams}}
	path = strings.ReplaceAll(path, "{{"{"}}{{.}}{{"}"}}", req.Params["{{.}}"])
{{- end}}
	httpReq, err := http.NewRequestWithContext(ctx, "{{.Method}}", path, nil)
	if err != nil {
		return sdk.Response{}, sdk.NewPermanentError("failed to create request", http.StatusInternalServerError, err)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return sdk.Response{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return sdk.Response{Body: body, StatusCode: resp.StatusCode}, nil
}
{{end}}
func (c *{{.TypeName}}Connector) HandleWebhook(ctx context.Context, event sdk.WebhookEvent) error {
	// TODO: implement webhook signature verification
	return nil
}
`

type templateData struct {
	Timestamp       string
	SpecTitle       string
	SpecVersion     string
	SpecDescription string
	PackageName     string
	TypeName        string
	ConnectorID     string
	Endpoints       []templateEndpoint
}

type templateEndpoint struct {
	EndpointSummary
	FuncName string
}

// generateWithTemplate renders a compilable skeleton; its Execute functions send no body
// and return the raw response.
func generateWithTemplate(spec *OpenAPISpec, connID string) string {
	eps := extractEndpoints(spec)

	var tEPs []templateEndpoint
	for _, ep := range eps {
		tEPs = append(tEPs, templateEndpoint{
			EndpointSummary: ep,
			FuncName:        toPascalCase(ep.Name),
		})
	}

	typeName := toPascalCase(connID)
	data := templateData{
		Timestamp:       time.Now().Format(time.RFC3339),
		SpecTitle:       spec.Info.Title,
		SpecVersion:     spec.Info.Version,
		SpecDescription: spec.Info.Description,
		PackageName:     strings.ToLower(connID),
		TypeName:        typeName,
		ConnectorID:     connID,
		Endpoints:       tEPs,
	}

	tmpl, err := template.New("connector").Parse(connectorTemplate)
	if err != nil {
		fatal("parse template failed", "err", err)
	}

	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		fatal("execute template failed", "err", err)
	}
	return sb.String()
}

// generateWithGenAI asks Gemini to write the connector from the endpoint summaries; the
// output is not compiled or validated.
func generateWithGenAI(apiKey string, spec *OpenAPISpec, connID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return "", fmt.Errorf("create genai client: %w", err)
	}

	// Only a one-line summary per endpoint goes in the prompt so large specs fit the context window.
	eps := extractEndpoints(spec)
	var epLines []string
	for _, ep := range eps {
		epLines = append(epLines, fmt.Sprintf("  - %s %s (%s): %s", ep.Method, ep.Path, ep.Name, ep.Description))
	}

	serverURL := ""
	if len(spec.Servers) > 0 {
		serverURL = spec.Servers[0].URL
	}

	prompt := fmt.Sprintf(`You are an expert Go developer building a Conduit SaaS integration framework connector.

Generate a complete, production-ready Go connector for the "%s" API (version %s).
Base URL: %s

The connector must implement the following interface:
- Metadata() sdk.ConnectorMetadata
- AuthConfig() sdk.AuthConfig
- Endpoints() []sdk.EndpointDefinition  (each with an Execute function)
- HandleWebhook(ctx context.Context, event sdk.WebhookEvent) error

API Endpoints to implement:
%s

Requirements:
- Package name: %s
- Use only standard library + "conduit/sdk" imports
- Use sdk.NewPermanentError and sdk.NewTransientError for errors
- Include HMAC webhook signature verification in HandleWebhook
- Add clear TODO comments where business logic needs customisation
- Output ONLY valid Go source code, no markdown fences

Generate the complete connector now:`,
		spec.Info.Title, spec.Info.Version, serverURL,
		strings.Join(epLines, "\n"),
		strings.ToLower(connID),
	)

	result, err := client.Models.GenerateContent(ctx, "gemini-2.0-flash", genai.Text(prompt), nil)
	if err != nil {
		return "", fmt.Errorf("generate content: %w", err)
	}

	if result == nil || len(result.Candidates) == 0 {
		return "", errors.New("empty response from GenAI")
	}

	raw := result.Text()
	// Strip markdown fences if the model added them despite the prompt
	raw = strings.TrimPrefix(raw, "```go\n")
	raw = strings.TrimPrefix(raw, "```\n")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw) + "\n", nil
}

// toSnakeCase converts a title like "Pet Store" to pet_store: separators become underscores
// and each capital starts a new word.
func toSnakeCase(s string) string {
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, "/", "_")
	var result []rune
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				result = append(result, '_')
			}
			result = append(result, r+32)
		} else {
			result = append(result, r)
		}
	}
	out := string(result)
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	return strings.Trim(out, "_")
}

// toPascalCase joins the words of s (split on _ - space and /), capitalizing each.
func toPascalCase(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == ' ' || r == '/'
	})
	var sb strings.Builder
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		sb.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return sb.String()
}
