// Package github is the GitHub connector: OAuth2 install, issues with Link-header
// pagination, and X-Hub-Signature-256 webhooks.
package github

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

// Connector is the GitHub connector.
type Connector struct {
	mockBaseURL string
}

// New returns a GitHub connector; an empty mockBaseURL means the mock provider at
// localhost:8081.
func New(mockBaseURL string) *Connector {
	if mockBaseURL == "" {
		mockBaseURL = "localhost:8081"
	}
	return &Connector{mockBaseURL: mockBaseURL}
}

// CreateIssueInput is the body for create_issue; Owner and Repo may instead come as
// query parameters, which win.
type CreateIssueInput struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Issue is the subset of GitHub's issue object the connector documents.
type Issue struct {
	ID     int    `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (c *Connector) Metadata() sdk.ConnectorMetadata {
	return sdk.ConnectorMetadata{
		ID:          "github",
		Name:        "GitHub",
		Description: "Create issues and list issue history in repositories.",
		Category:    "Developer Tools",
		Scopes:      []string{"repo", "user"},
	}
}

func (c *Connector) AuthConfig() sdk.AuthConfig {
	return sdk.AuthConfig{
		Type: sdk.AuthTypeOAuth2,
		OAuth2: &sdk.OAuth2Config{
			AuthURL:  fmt.Sprintf("http://%s/mock/oauth/authorize", c.mockBaseURL),
			TokenURL: fmt.Sprintf("http://%s/services/github/login/oauth/access_token", c.mockBaseURL),
			Scopes:   []string{"repo", "user"},
		},
	}
}

func (c *Connector) Endpoints() []sdk.EndpointDefinition {
	return []sdk.EndpointDefinition{
		{
			Name:         "create_issue",
			Method:       "POST",
			Path:         "/services/github/repos/{owner}/{repo}/issues",
			Description:  "Create a new issue in a GitHub repository.",
			InputSchema:  &CreateIssueInput{},
			OutputSchema: &Issue{},
			Execute:      c.executeCreateIssue,
		},
		{
			Name:         "list_issues",
			Method:       "GET",
			Path:         "/services/github/repos/{owner}/{repo}/issues",
			Description:  "List issues for a GitHub repository.",
			OutputSchema: &[]Issue{},
			Execute:      c.executeListIssues,
			GetPaginator: c.listIssuesPaginator,
		},
	}
}

// issuesPath escapes owner and repo, which come from the caller, so values like "../x"
// cannot change the requested path.
func issuesPath(owner, repo string) string {
	return "/services/github/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/issues"
}

func (c *Connector) executeCreateIssue(ctx context.Context, client *sdk.HTTPClient, req sdk.Request) (sdk.Response, error) {
	var input CreateIssueInput
	if err := json.Unmarshal(req.Body, &input); err != nil {
		return sdk.Response{}, sdk.NewPermanentError("invalid JSON body", http.StatusBadRequest, err)
	}

	owner := req.Params["owner"]
	repo := req.Params["repo"]
	if owner == "" {
		owner = input.Owner
	}
	if repo == "" {
		repo = input.Repo
	}

	if owner == "" || repo == "" || input.Title == "" {
		return sdk.Response{}, sdk.NewPermanentError("owner, repo, and title are required", http.StatusBadRequest, nil)
	}

	path := issuesPath(owner, repo)

	payloadBytes, err := json.Marshal(map[string]string{
		"title": input.Title,
		"body":  input.Body,
	})
	if err != nil {
		return sdk.Response{}, sdk.NewPermanentError("failed to marshal request", http.StatusInternalServerError, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", path, strings.NewReader(string(payloadBytes)))
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

func (c *Connector) executeListIssues(ctx context.Context, client *sdk.HTTPClient, req sdk.Request) (sdk.Response, error) {
	owner := req.Params["owner"]
	repo := req.Params["repo"]
	if owner == "" || repo == "" {
		return sdk.Response{}, sdk.NewPermanentError("owner and repo are required params", http.StatusBadRequest, nil)
	}

	path := issuesPath(owner, repo)
	reqURL, err := url.Parse(path)
	if err != nil {
		return sdk.Response{}, sdk.NewPermanentError("invalid path", http.StatusInternalServerError, err)
	}
	q := reqURL.Query()
	for k, v := range req.Params {
		if k != "owner" && k != "repo" {
			q.Set(k, v)
		}
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

	return sdk.Response{
		Body:       body,
		StatusCode: resp.StatusCode,
	}, nil
}

func (c *Connector) listIssuesPaginator(path string, params map[string]string, limit int) sdk.Paginator {
	owner := params["owner"]
	repo := params["repo"]
	resolvedPath := issuesPath(owner, repo)
	reqURL, _ := url.Parse(resolvedPath)
	q := reqURL.Query()
	for k, v := range params {
		if k != "owner" && k != "repo" && k != "cursor" && k != "limit" {
			q.Set(k, v)
		}
	}
	reqURL.RawQuery = q.Encode()

	return sdk.NewCursorPaginator(
		reqURL.String(),
		"cursor",
		"limit",
		limit,
		extractGitHubNextLink,
	)
}

// extractGitHubNextLink returns the rel="next" URI from the Link header, made relative so
// HTTPClient resolves it against its base URL.
func extractGitHubNextLink(_ []byte, headers http.Header) (string, bool, error) {
	linkHeader := headers.Get("Link")
	if linkHeader == "" {
		return "", false, nil
	}

	links := strings.Split(linkHeader, ",")
	for _, link := range links {
		parts := strings.Split(link, ";")
		if len(parts) < 2 {
			continue
		}
		urlPart := strings.TrimSpace(parts[0])
		relPart := strings.TrimSpace(parts[1])

		if strings.Contains(relPart, `rel="next"`) {
			if len(urlPart) > 2 && urlPart[0] == '<' && urlPart[len(urlPart)-1] == '>' {
				nextURL := urlPart[1 : len(urlPart)-1]
				u, err := url.Parse(nextURL)
				if err != nil {
					return "", false, err
				}
				return u.RequestURI(), true, nil
			}
		}
	}
	return "", false, nil
}

func (c *Connector) HandleWebhook(ctx context.Context, event sdk.WebhookEvent) error {
	sig := event.Headers["X-Hub-Signature-256"]
	if sig == "" {
		return sdk.NewPermanentError("missing GitHub signature header", http.StatusBadRequest, nil)
	}

	if !sdk.VerifyHMACSHA256HexSignature(event.Payload, sig, "sha256=", event.Secret) {
		return sdk.NewPermanentError("invalid webhook signature", http.StatusUnauthorized, nil)
	}

	return nil
}
