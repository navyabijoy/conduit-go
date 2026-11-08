package sdk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestOffsetPaginator(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsetStr := r.URL.Query().Get("offset")
		limitStr := r.URL.Query().Get("limit")
		offset, _ := strconv.Atoi(offsetStr)
		limit, _ := strconv.Atoi(limitStr)

		if limit != 2 {
			t.Errorf("expected limit to be 2, got %d", limit)
		}

		// The collection holds 5 items, so limit 2 yields pages of 2, 2 and 1.
		var items []int
		for i := offset; i < offset+limit && i < 5; i++ {
			items = append(items, i)
		}

		respBytes, _ := json.Marshal(items)
		w.Header().Set("Content-Type", "application/json")
		w.Write(respBytes)
	}))
	defer ts.Close()

	client := NewHTTPClient(ts.URL, Credentials{}, AuthConfig{}, nil)
	paginator := NewOffsetPaginator(ts.URL, 2, "offset", "limit")

	var allItems []int
	pages := 0
	for paginator.HasNext() {
		pages++
		body, err := paginator.Next(context.Background(), client)
		if err != nil {
			t.Fatalf("unexpected error on page %d: %v", pages, err)
		}

		var pageItems []int
		if err := json.Unmarshal(body, &pageItems); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}
		allItems = append(allItems, pageItems...)
	}

	if pages != 3 {
		t.Errorf("expected 3 pages, got %d", pages)
	}
	if len(allItems) != 5 {
		t.Errorf("expected 5 items in total, got %d", len(allItems))
	}
	for i, val := range allItems {
		if val != i {
			t.Errorf("expected item %d to be %d, got %d", i, i, val)
		}
	}
}

func TestCursorPaginator(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		limitStr := r.URL.Query().Get("limit")
		limit, _ := strconv.Atoi(limitStr)

		if limit != 3 {
			t.Errorf("expected limit to be 3, got %d", limit)
		}

		type Item struct {
			ID string `json:"id"`
		}
		type Response struct {
			Items      []Item `json:"items"`
			NextCursor string `json:"next_cursor,omitempty"`
		}

		var resp Response
		if cursor == "" {
			resp.Items = []Item{{ID: "1"}, {ID: "2"}, {ID: "3"}}
			resp.NextCursor = "page2"
		} else if cursor == "page2" {
			resp.Items = []Item{{ID: "4"}, {ID: "5"}}
			resp.NextCursor = "" // terminal page
		}

		respBytes, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.Write(respBytes)
	}))
	defer ts.Close()

	extractor := func(body []byte, headers http.Header) (string, bool, error) {
		var resp struct {
			NextCursor string `json:"next_cursor"`
			Items      []interface{}
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return "", false, err
		}
		return resp.NextCursor, len(resp.Items) > 0, nil
	}

	client := NewHTTPClient(ts.URL, Credentials{}, AuthConfig{}, nil)
	paginator := NewCursorPaginator(ts.URL, "cursor", "limit", 3, extractor)

	var items []string
	pages := 0
	for paginator.HasNext() {
		pages++
		body, err := paginator.Next(context.Background(), client)
		if err != nil {
			t.Fatalf("unexpected error on page %d: %v", pages, err)
		}

		var resp struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		json.Unmarshal(body, &resp)
		for _, item := range resp.Items {
			items = append(items, item.ID)
		}
	}

	if pages != 2 {
		t.Errorf("expected 2 pages, got %d", pages)
	}
	if len(items) != 5 {
		t.Errorf("expected 5 items, got %d", len(items))
	}
	expected := []string{"1", "2", "3", "4", "5"}
	for i, val := range items {
		if val != expected[i] {
			t.Errorf("expected item %d to be %q, got %q", i, expected[i], val)
		}
	}
}
