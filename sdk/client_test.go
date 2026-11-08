package sdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientAuth(t *testing.T) {
	t.Run("oauth2 sends bearer token", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer sample-token" {
				t.Errorf("expected Bearer sample-token, got %q", auth)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		creds := Credentials{Token: &Token{AccessToken: "sample-token"}}
		client := NewHTTPClient(ts.URL, creds, AuthConfig{Type: AuthTypeOAuth2}, nil)

		req, _ := http.NewRequest("GET", ts.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		resp.Body.Close()
	})

	t.Run("api key sets configured header and prefix", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("X-API-Key")
			if auth != "ApiKey secret-value" {
				t.Errorf("expected ApiKey secret-value, got %q", auth)
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer ts.Close()

		creds := Credentials{APIKey: "secret-value"}
		config := AuthConfig{
			Type: AuthTypeAPIKey,
			APIKey: &APIKeyConfig{
				HeaderName:  "X-API-Key",
				ValuePrefix: "ApiKey ",
			},
		}
		client := NewHTTPClient(ts.URL, creds, config, nil)

		req, _ := http.NewRequest("GET", ts.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		resp.Body.Close()
	})
}

func TestClientRetries(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, Credentials{}, AuthConfig{}, nil)
	client.SetBackoffRange(1*time.Millisecond, 5*time.Millisecond)

	req, _ := http.NewRequest("GET", ts.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected request to succeed eventually, got error: %v", err)
	}
	defer resp.Body.Close()

	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestClientSlackRateLimit(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, Credentials{}, AuthConfig{}, nil)
	// With a backoff far below 1s, the elapsed-time check only passes if the
	// client actually honoured Retry-After.
	client.SetBackoffRange(10*time.Millisecond, 20*time.Millisecond)

	startTime := time.Now()
	req, _ := http.NewRequest("GET", ts.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected request to succeed on retry: %v", err)
	}
	defer resp.Body.Close()

	elapsed := time.Since(startTime)
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected sleep of ~1s due to Retry-After header, but it took only %v", elapsed)
	}
}

func TestClientGitHubRateLimit(t *testing.T) {
	var attempts int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count == 1 {
			resetTime := time.Now().Unix() + 2
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetTime, 10))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, Credentials{}, AuthConfig{}, nil)
	client.SetBackoffRange(10*time.Millisecond, 20*time.Millisecond)

	startTime := time.Now()
	req, _ := http.NewRequest("GET", ts.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected request to succeed on retry: %v", err)
	}
	defer resp.Body.Close()

	elapsed := time.Since(startTime)
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected sleep of ~1s due to X-RateLimit-Reset header, but took %v", elapsed)
	}
}

func TestClientTokenRefresh(t *testing.T) {
	var attempts int32
	var refreshCalls int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		auth := r.Header.Get("Authorization")
		if auth == "Bearer expired-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if auth == "Bearer new-token" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	refreshHook := func(ctx context.Context, token *Token) (*Token, error) {
		atomic.AddInt32(&refreshCalls, 1)
		if token.AccessToken != "expired-token" {
			t.Errorf("expected current token to be 'expired-token', got %q", token.AccessToken)
		}
		return &Token{
			AccessToken:  "new-token",
			RefreshToken: "some-refresh-token",
			Expiry:       time.Now().Add(1 * time.Hour),
		}, nil
	}

	creds := Credentials{Token: &Token{AccessToken: "expired-token"}}
	config := AuthConfig{Type: AuthTypeOAuth2}
	client := NewHTTPClient(ts.URL, creds, config, refreshHook)

	req, _ := http.NewRequest("GET", ts.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected request to succeed after refresh, got %v", err)
	}
	defer resp.Body.Close()

	if refreshCalls != 1 {
		t.Errorf("expected 1 token refresh call, got %d", refreshCalls)
	}
	if client.Credentials().Token.AccessToken != "new-token" {
		t.Errorf("expected client's token to be updated, got %q", client.Credentials().Token.AccessToken)
	}
}

// Do must not hand back an open response with an error: callers return early on error,
// so the body would never be closed.
func TestClientDo_4xxClosesResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, Credentials{}, AuthConfig{}, nil)
	req, _ := http.NewRequest("GET", ts.URL, nil)
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
		t.Fatal("want nil response on a 4xx")
	}
	var ce *ConduitError
	if !errors.As(err, &ce) || ce.Code != http.StatusNotFound || !IsPermanent(err) {
		t.Fatalf("want permanent ConduitError with code 404, got %v", err)
	}
}

// Backoff settings a caller can pass must never panic.
func TestCalculateBackoff_NoPanic(t *testing.T) {
	c := NewHTTPClient("", Credentials{}, AuthConfig{}, nil)

	c.SetBackoffRange(0, 0)
	if got := c.calculateBackoff(3); got != 0 {
		t.Errorf("zero range: got %v, want 0", got)
	}

	c.SetBackoffRange(100*time.Millisecond, 10*time.Second)
	for _, attempt := range []int{0, 40, 63, 1000} {
		got := c.calculateBackoff(attempt)
		if got <= 0 || got > 15*time.Second {
			t.Errorf("attempt %d: got %v, want within (0, maxBackoff*1.5]", attempt, got)
		}
	}
}
