package main

import (
	"os"
	"strings"
	"testing"
)

func TestToSnakeCase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"MyConnector", "my_connector"},
		{"Petstore API", "petstore_a_p_i"},
		{"hello-world", "hello_world"},
		{"already_snake", "already_snake"},
	}
	for _, c := range cases {
		got := toSnakeCase(c.in)
		if got != c.want {
			t.Errorf("toSnakeCase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestToPascalCase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"my_connector", "MyConnector"},
		{"list_charges", "ListCharges"},
		{"create-issue", "CreateIssue"},
		{"send_message", "SendMessage"},
	}
	for _, c := range cases {
		got := toPascalCase(c.in)
		if got != c.want {
			t.Errorf("toPascalCase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseOpenAPISpec_InvalidFile(t *testing.T) {
	_, err := parseOpenAPISpec("/nonexistent/spec.json")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestParseOpenAPISpec_InvalidJSON(t *testing.T) {
	f, _ := os.CreateTemp("", "spec_*.json")
	f.WriteString("not json")
	f.Close()
	defer os.Remove(f.Name())

	_, err := parseOpenAPISpec(f.Name())
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}

func TestParseAndExtractEndpoints(t *testing.T) {
	specJSON := `{
		"openapi": "3.0.0",
		"info": {"title": "Pet Store", "version": "1.0"},
		"servers": [{"url": "https://api.petstore.com"}],
		"paths": {
			"/pets": {
				"get": {
					"operationId": "list_pets",
					"summary": "List all pets"
				},
				"post": {
					"operationId": "create_pet",
					"summary": "Create a pet",
					"requestBody": {"required": true}
				}
			},
			"/pets/{id}": {
				"get": {
					"operationId": "get_pet",
					"summary": "Get a specific pet",
					"parameters": [{"name": "id", "in": "path", "required": true}]
				}
			}
		}
	}`

	f, _ := os.CreateTemp("", "spec_*.json")
	f.WriteString(specJSON)
	f.Close()
	defer os.Remove(f.Name())

	spec, err := parseOpenAPISpec(f.Name())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.Info.Title != "Pet Store" {
		t.Errorf("want title=Pet Store, got %q", spec.Info.Title)
	}

	eps := extractEndpoints(spec)
	if len(eps) != 3 {
		t.Errorf("want 3 endpoints, got %d", len(eps))
	}

	names := make(map[string]bool)
	for _, ep := range eps {
		names[ep.Name] = true
	}
	if !names["list_pets"] {
		t.Error("list_pets endpoint missing")
	}
	if !names["create_pet"] {
		t.Error("create_pet endpoint missing")
	}
	if !names["get_pet"] {
		t.Error("get_pet endpoint missing")
	}
}

func TestGenerateWithTemplate(t *testing.T) {
	spec := &OpenAPISpec{
		Info: OpenAPIInfo{Title: "Pet Store", Version: "1.0", Description: "A simple pet store API"},
		Paths: OpenAPIPaths{
			"/pets": {
				"get": {OperationID: "list_pets", Summary: "List pets"},
			},
		},
	}

	code := generateWithTemplate(spec, "petstore")

	checks := []string{
		"package petstore",
		"PetstoreConnector",
		"func (c *PetstoreConnector) Metadata()",
		"func (c *PetstoreConnector) AuthConfig()",
		"func (c *PetstoreConnector) Endpoints()",
		"func (c *PetstoreConnector) HandleWebhook(",
		"list_pets",
		"conduit/sdk",
		"sdk.AuthTypeAPIKey",
	}

	for _, check := range checks {
		if !strings.Contains(code, check) {
			t.Errorf("generated code missing %q", check)
		}
	}
}

func TestGenerateWithTemplate_MultipleEndpoints(t *testing.T) {
	spec := &OpenAPISpec{
		Info: OpenAPIInfo{Title: "My API", Version: "2.0"},
		Paths: OpenAPIPaths{
			"/items":      {"get": {OperationID: "list_items", Summary: "List items"}},
			"/items/{id}": {"post": {OperationID: "create_item", Summary: "Create item", RequestBody: &OpenAPIRequestBody{Required: true}}},
		},
	}

	code := generateWithTemplate(spec, "myapi")

	if !strings.Contains(code, "executeListItems") {
		t.Error("expected executeListItems function")
	}
	if !strings.Contains(code, "executeCreateItem") {
		t.Error("expected executeCreateItem function")
	}
}
