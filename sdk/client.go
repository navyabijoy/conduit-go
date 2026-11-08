package sdk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPClient sends connector requests with credentials, OAuth2 refresh, jittered
// retries and rate-limit handling.
type HTTPClient struct {
	client           *http.Client
	baseURL          string
	creds            Credentials
	authConfig       AuthConfig
	tokenRefreshHook func(ctx context.Context, token *Token) (*Token, error)
	maxRetries       int
	baseBackoff      time.Duration
	maxBackoff       time.Duration
}

// NewHTTPClient resolves path-only URLs against baseURL. Defaults: 30s timeout,
// 5 retries, 100ms to 10s backoff. A nil tokenRefreshHook disables token refresh.
func NewHTTPClient(
	baseURL string,
	creds Credentials,
	authConfig AuthConfig,
	tokenRefreshHook func(ctx context.Context, token *Token) (*Token, error),
) *HTTPClient {
	return &HTTPClient{
		client:           &http.Client{Timeout: 30 * time.Second},
		baseURL:          baseURL,
		creds:            creds,
		authConfig:       authConfig,
		tokenRefreshHook: tokenRefreshHook,
		maxRetries:       5,
		baseBackoff:      100 * time.Millisecond,
		maxBackoff:       10 * time.Second,
	}
}

// SetMaxRetries sets how many retries follow the first attempt.
func (c *HTTPClient) SetMaxRetries(r int) {
	c.maxRetries = r
}

// SetBackoffRange sets the first retry delay and the cap it doubles up to.
func (c *HTTPClient) SetBackoffRange(base, max time.Duration) {
	c.baseBackoff = base
	c.maxBackoff = max
}

// Credentials returns the current credentials, including a token refreshed during Do.
func (c *HTTPClient) Credentials() Credentials {
	return c.creds
}

// maxRateLimitWait caps provider-requested waits so a request never hangs for minutes.
const maxRateLimitWait = 15 * time.Second

// Do sends req, retrying network errors and 5xx with backoff and waiting out rate limits.
// A 4xx response is closed and reported as a permanent error carrying the status code.
func (c *HTTPClient) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	// Buffered so every attempt can replay it.
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, NewPermanentError("failed to read request body", 0, err)
		}
	}

	refreshed := false
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if !refreshed && c.tokenExpired() {
			if err := c.refreshToken(ctx); err != nil {
				return nil, NewPermanentError("failed to refresh token pre-emptively", 0, err)
			}
			refreshed = true
		}

		attemptReq := cloneRequest(req, body)
		c.applyAuth(attemptReq)

		resp, err := c.client.Do(attemptReq)
		if err != nil {
			if attempt == c.maxRetries {
				return nil, NewTransientError("request failed after retries", 0, err)
			}
			if IsTransient(err) {
				c.sleep(ctx, attempt, 0)
				continue
			}
			return nil, NewPermanentError("network request failed permanently", 0, err)
		}

		if resp.StatusCode == http.StatusUnauthorized && !refreshed && c.canRefresh() {
			resp.Body.Close()
			if err := c.refreshToken(ctx); err != nil {
				return nil, NewPermanentError("failed to refresh token on 401", http.StatusUnauthorized, err)
			}
			refreshed = true
			// A retry after a refresh does not count against maxRetries.
			attempt--
			continue
		}

		if wait, limited := rateLimitWait(resp); limited {
			resp.Body.Close()
			if attempt == c.maxRetries {
				return nil, NewTransientError("rate limit exceeded, maximum retries reached", resp.StatusCode, nil)
			}
			if wait > maxRateLimitWait {
				return nil, NewTransientError(fmt.Sprintf("rate limit exceeded, wait time %s too long", wait), resp.StatusCode, nil)
			}
			c.sleep(ctx, attempt, wait)
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			if attempt == c.maxRetries {
				return nil, NewTransientError(fmt.Sprintf("server error: %d", resp.StatusCode), resp.StatusCode, nil)
			}
			c.sleep(ctx, attempt, 0)
			continue
		}

		if resp.StatusCode >= 400 {
			resp.Body.Close()
			return nil, NewPermanentError(fmt.Sprintf("request failed with status %d", resp.StatusCode), resp.StatusCode, nil)
		}

		return resp, nil
	}

	return nil, NewTransientError("request failed, max retries reached", 0, nil)
}

// cloneRequest gives each attempt a fresh body reader; the original can be read once.
func cloneRequest(r *http.Request, body []byte) *http.Request {
	clone := r.Clone(r.Context())
	if len(body) > 0 {
		clone.Body = io.NopCloser(bytes.NewReader(body))
	}
	return clone
}

func (c *HTTPClient) canRefresh() bool {
	return c.authConfig.Type == AuthTypeOAuth2 && c.tokenRefreshHook != nil
}

func (c *HTTPClient) tokenExpired() bool {
	return c.canRefresh() && c.creds.Token != nil && c.creds.Token.IsExpired()
}

func (c *HTTPClient) refreshToken(ctx context.Context) error {
	token, err := c.tokenRefreshHook(ctx, c.creds.Token)
	if err != nil {
		return err
	}
	c.creds.Token = token
	return nil
}

// rateLimitWait reads provider signals: Retry-After on 429 (Slack, Stripe), or GitHub's
// X-RateLimit-Reset when the quota is spent, which wins if both are present.
func rateLimitWait(resp *http.Response) (time.Duration, bool) {
	var wait time.Duration
	limited := false

	if resp.StatusCode == http.StatusTooManyRequests {
		limited = true
		if h := resp.Header.Get("Retry-After"); h != "" {
			wait = parseRetryAfter(h)
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		limited = true
		if h := resp.Header.Get("X-RateLimit-Reset"); h != "" {
			wait = parseRateLimitReset(h)
		}
	}
	return wait, limited
}

// applyAuth points path-only requests at baseURL and sets the auth header.
func (c *HTTPClient) applyAuth(req *http.Request) {
	if req.URL.Scheme == "" && c.baseURL != "" {
		scheme, host := "http", c.baseURL
		if rest, ok := strings.CutPrefix(c.baseURL, "http://"); ok {
			host = rest
		} else if rest, ok := strings.CutPrefix(c.baseURL, "https://"); ok {
			scheme, host = "https", rest
		}
		req.URL.Scheme = scheme
		req.URL.Host = host
	}

	switch c.authConfig.Type {
	case AuthTypeOAuth2:
		if c.creds.Token != nil {
			req.Header.Set("Authorization", "Bearer "+c.creds.Token.AccessToken)
		}
	case AuthTypeAPIKey:
		if c.authConfig.APIKey != nil && c.creds.APIKey != "" {
			req.Header.Set(c.authConfig.APIKey.HeaderName, c.authConfig.APIKey.ValuePrefix+c.creds.APIKey)
		}
	}
}

// calculateBackoff doubles per attempt up to maxBackoff, plus up to 50% jitter so
// clients that failed together do not retry in lockstep.
func (c *HTTPClient) calculateBackoff(attempt int) time.Duration {
	// Doubling in a loop that stops at the cap cannot overflow, unlike 1<<attempt.
	backoff := c.baseBackoff
	for i := 0; i < attempt && backoff < c.maxBackoff; i++ {
		backoff *= 2
	}
	backoff = min(backoff, c.maxBackoff)
	if backoff <= 0 {
		return 0
	}
	if half := int64(backoff / 2); half > 0 {
		backoff += time.Duration(rand.Int63n(half))
	}
	return backoff
}

// sleep waits d, or the backoff for attempt when d is zero, and returns early if ctx is done.
func (c *HTTPClient) sleep(ctx context.Context, attempt int, d time.Duration) {
	duration := d
	if duration == 0 {
		duration = c.calculateBackoff(attempt)
	}
	select {
	case <-ctx.Done():
	case <-time.After(duration):
	}
}

// parseRetryAfter accepts seconds or an HTTP date, the two forms RFC 9110 allows.
func parseRetryAfter(h string) time.Duration {
	if secs, err := strconv.Atoi(h); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}

// parseRateLimitReset turns GitHub's Unix-timestamp reset header into a wait.
func parseRateLimitReset(h string) time.Duration {
	if epoch, err := strconv.ParseInt(h, 10, 64); err == nil {
		t := time.Unix(epoch, 0)
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return d
	}
	return 0
}
