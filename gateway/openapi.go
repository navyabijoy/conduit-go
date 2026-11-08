package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
)

func (s *Server) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	spec := map[string]interface{}{
		"openapi": "3.0.0",
		"info": map[string]interface{}{
			"title":       "Conduit API Gateway",
			"description": "Dynamic SaaS integration framework routing and endpoint specifications.",
			"version":     "1.0.0",
		},
		"paths": s.generatePaths(),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(spec)
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(swaggerUIHTML))
}

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Conduit API Documentation</title>
  <link rel="stylesheet" type="text/css" href="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui.css"
    integrity="sha384-+yyzNgM3K92sROwsXxYCxaiLWxWJ0G+v/9A+qIZ2rgefKgkdcmJI+L601cqPD/Ut" crossorigin="anonymous" />
  <link rel="icon" type="image/png" href="https://unpkg.com/swagger-ui-dist@5.11.0/favicon-32x32.png" sizes="32x32" />
  <link rel="icon" type="image/png" href="https://unpkg.com/swagger-ui-dist@5.11.0/favicon-16x16.png" sizes="16x16" />
  <style>
    html {
      box-sizing: border-box;
      overflow-y: scroll;
    }
    *, *:before, *:after {
      box-sizing: inherit;
    }
    body {
      margin: 0;
      background: #fafafa;
    }
    .swagger-ui .topbar {
      display: none;
    }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui-bundle.js" charset="UTF-8"
    integrity="sha384-qn5tagrAjZi8cSmvZ+k3zk4+eDEEUcP9myuR2J6V+/H6rne++v6ChO7EeHAEzqxQ" crossorigin="anonymous"></script>
  <script src="https://unpkg.com/swagger-ui-dist@5.11.0/swagger-ui-standalone-preset.js" charset="UTF-8"
    integrity="sha384-SiLF+uYBf9lVQW98s/XUYP14enXJN31bn0zu3BS1WFqr5hvnMF+w132WkE/v0uJw" crossorigin="anonymous"></script>
  <script>
    window.onload = function() {
      const ui = SwaggerUIBundle({
        url: "/openapi.json",
        dom_id: '#swagger-ui',
        deepLinking: true,
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        plugins: [
          SwaggerUIBundle.plugins.DownloadUrl
        ],
        layout: "StandaloneLayout"
      });
      window.ui = ui;
    };
  </script>
</body>
</html>`

// generatePaths documents each endpoint as POST /v1/instances/{instance_id}/endpoints/{name}.
// Endpoints that share a name across connectors overwrite each other.
func (s *Server) generatePaths() map[string]interface{} {
	paths := make(map[string]interface{})

	for connID, conn := range s.registry {
		for _, ep := range conn.Endpoints() {
			routePath := "/v1/instances/{instance_id}/endpoints/" + ep.Name

			pathItem, exists := paths[routePath]
			var methods map[string]interface{}
			if exists {
				methods = pathItem.(map[string]interface{})
			} else {
				methods = make(map[string]interface{})
				paths[routePath] = methods
			}

			op := map[string]interface{}{
				"summary":     fmt.Sprintf("Execute %s endpoint: %s", connID, ep.Name),
				"description": ep.Description,
				"parameters": []map[string]interface{}{
					{
						"name":        "instance_id",
						"in":          "path",
						"required":    true,
						"description": "ID of the installed integration instance",
						"schema": map[string]interface{}{
							"type": "string",
						},
					},
				},
			}

			if ep.InputSchema != nil {
				schema := reflectToOpenAPISchema(ep.InputSchema)
				if schema != nil {
					op["requestBody"] = map[string]interface{}{
						"required": true,
						"content": map[string]interface{}{
							"application/json": map[string]interface{}{
								"schema": schema,
							},
						},
					}
				}
			}

			respContent := map[string]interface{}{}
			if ep.OutputSchema != nil {
				schema := reflectToOpenAPISchema(ep.OutputSchema)
				if schema != nil {
					respContent["application/json"] = map[string]interface{}{
						"schema": schema,
					}
				}
			}

			op["responses"] = map[string]interface{}{
				"200": map[string]interface{}{
					"description": "Successful execution response",
					"content":     respContent,
				},
				"400": map[string]interface{}{
					"description": "Invalid input payload",
				},
				"429": map[string]interface{}{
					"description": "Rate limit exceeded",
				},
				"502": map[string]interface{}{
					"description": "Integration execution failure",
				},
			}

			methods["post"] = op
		}
	}

	return paths
}

func reflectToOpenAPISchema(val interface{}) map[string]interface{} {
	if val == nil {
		return nil
	}

	t := reflect.TypeOf(val)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	return reflectTypeToSchema(t)
}

// reflectTypeToSchema maps Go kinds to OpenAPI types, naming struct fields by json tag.
func reflectTypeToSchema(t reflect.Type) map[string]interface{} {
	switch t.Kind() {
	case reflect.Struct:
		properties := make(map[string]interface{})
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			jsonTag := field.Tag.Get("json")
			fieldName := field.Name
			if jsonTag != "" {
				parts := strings.Split(jsonTag, ",")
				if parts[0] == "-" {
					continue
				}
				if parts[0] != "" {
					fieldName = parts[0]
				}
			}

			fieldSchema := reflectTypeToSchema(field.Type)
			if fieldSchema != nil {
				properties[fieldName] = fieldSchema
			}
		}
		return map[string]interface{}{
			"type":       "object",
			"properties": properties,
		}

	case reflect.Slice, reflect.Array:
		elemType := t.Elem()
		if elemType.Kind() == reflect.Ptr {
			elemType = elemType.Elem()
		}
		return map[string]interface{}{
			"type":  "array",
			"items": reflectTypeToSchema(elemType),
		}

	case reflect.String:
		return map[string]interface{}{"type": "string"}

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]interface{}{"type": "integer"}

	case reflect.Float32, reflect.Float64:
		return map[string]interface{}{"type": "number"}

	case reflect.Bool:
		return map[string]interface{}{"type": "boolean"}

	default:
		// Maps, interfaces and other kinds have no precise OpenAPI mapping.
		return map[string]interface{}{"type": "string"}
	}
}
