package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Paginator walks a list endpoint page by page: call Next while HasNext is true.
type Paginator interface {
	HasNext() bool
	Next(ctx context.Context, client *HTTPClient) ([]byte, error)
}

// OffsetPaginator pages with offset and limit query parameters.
type OffsetPaginator struct {
	Path        string
	Limit       int
	OffsetParam string
	LimitParam  string
	offset      int
	hasMore     bool
}

// NewOffsetPaginator returns a paginator starting at offset 0.
func NewOffsetPaginator(path string, limit int, offsetParam, limitParam string) *OffsetPaginator {
	return &OffsetPaginator{
		Path:        path,
		Limit:       limit,
		OffsetParam: offsetParam,
		LimitParam:  limitParam,
		hasMore:     true,
	}
}

// HasNext reports whether another page may exist.
func (p *OffsetPaginator) HasNext() bool {
	return p.hasMore
}

// Next fetches the page at the current offset and advances past it.
func (p *OffsetPaginator) Next(ctx context.Context, client *HTTPClient) ([]byte, error) {
	if !p.hasMore {
		return nil, errors.New("no more pages")
	}

	reqURL, err := url.Parse(p.Path)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	q := reqURL.Query()
	q.Set(p.OffsetParam, strconv.Itoa(p.offset))
	q.Set(p.LimitParam, strconv.Itoa(p.Limit))
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Offset APIs rarely report a total, so a page shorter than Limit is taken as the last.
	var list []interface{}
	if err := json.Unmarshal(body, &list); err == nil {
		p.offset += len(list)
		p.hasMore = len(list) >= p.Limit
	} else {
		var obj map[string]interface{}
		if err := json.Unmarshal(body, &obj); err == nil {
			foundArray := false
			for _, val := range obj {
				if subList, ok := val.([]interface{}); ok {
					p.offset += len(subList)
					p.hasMore = len(subList) >= p.Limit
					foundArray = true
					break
				}
			}
			if !foundArray {
				p.hasMore = false
			}
		} else {
			p.hasMore = false
		}
	}

	return body, nil
}

// CursorPaginator pages with an opaque cursor that extractNext reads from each response.
type CursorPaginator struct {
	Path        string
	CursorParam string
	LimitParam  string
	Limit       int
	nextCursor  string
	hasMore     bool
	isFirst     bool
	extractNext func(body []byte, respHeaders http.Header) (string, bool, error)
}

// NewCursorPaginator returns a paginator whose first request has no cursor;
// limitParam may be empty.
func NewCursorPaginator(
	path string,
	cursorParam string,
	limitParam string,
	limit int,
	extractNext func(body []byte, respHeaders http.Header) (string, bool, error),
) *CursorPaginator {
	return &CursorPaginator{
		Path:        path,
		CursorParam: cursorParam,
		LimitParam:  limitParam,
		Limit:       limit,
		hasMore:     true,
		isFirst:     true,
		extractNext: extractNext,
	}
}

// HasNext reports whether another page may exist.
func (p *CursorPaginator) HasNext() bool {
	return p.hasMore
}

// Next fetches the page at the current cursor and stores the cursor for the next.
func (p *CursorPaginator) Next(ctx context.Context, client *HTTPClient) ([]byte, error) {
	if !p.hasMore {
		return nil, errors.New("no more pages")
	}

	reqURL, err := url.Parse(p.Path)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}

	q := reqURL.Query()
	if !p.isFirst && p.nextCursor != "" {
		q.Set(p.CursorParam, p.nextCursor)
	}
	if p.Limit > 0 && p.LimitParam != "" {
		q.Set(p.LimitParam, strconv.Itoa(p.Limit))
	}
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, "GET", reqURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	cursor, hasMore, err := p.extractNext(body, resp.Header)
	if err != nil {
		return nil, fmt.Errorf("failed to extract next cursor: %w", err)
	}

	p.nextCursor = cursor
	p.hasMore = hasMore && cursor != ""
	p.isFirst = false

	return body, nil
}
