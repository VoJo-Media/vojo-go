package vojo

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/url"
)

// ListResult is a complete list from an operation that does not page.
type ListResult[T any] struct {
	Data         []T
	LastResponse *ResponseInfo
}

// Envelope is data together with meta, for responses whose meta carries information.
type Envelope[T any] struct {
	Data         T
	Meta         json.RawMessage
	LastResponse *ResponseInfo
}

// DecodeMeta unmarshals Meta into v.
func (e *Envelope[T]) DecodeMeta(v any) error { return json.Unmarshal(e.Meta, v) }

// PageMeta is the paging data on a list response.
type PageMeta struct {
	NextCursor *string `json:"next_cursor"`
	HasMore    *bool   `json:"has_more"`
}

// Page is one page of a list.
type Page[T any] struct {
	Data         []T      `json:"data"`
	Meta         PageMeta `json:"meta"`
	LastResponse *ResponseInfo
}

// HasMore reports whether another page exists.
func (p *Page[T]) HasMore() bool { return p.Meta.HasMore != nil && *p.Meta.HasMore }

// nextCursor is the cursor to follow next, or "" at the end. It returns an error
// rather than loop forever.
func nextCursor(meta PageMeta, seen map[string]bool) (string, error) {
	// has_more is authoritative: honour false even if a cursor came with it.
	if meta.HasMore != nil && !*meta.HasMore {
		return "", nil
	}
	cursor := ""
	if meta.NextCursor != nil {
		cursor = *meta.NextCursor
	}
	if cursor == "" {
		if meta.HasMore != nil && *meta.HasMore {
			return "", fmt.Errorf("%w: the API reported more pages but returned no next_cursor", ErrPagination)
		}
		return "", nil
	}
	if seen[cursor] {
		return "", fmt.Errorf("%w: the API repeated the cursor %q; stopping rather than looping forever", ErrPagination, cursor)
	}
	seen[cursor] = true
	return cursor, nil
}

// List is returned by list operations. No request is sent until you use it. A
// list's Limit sets the page size (at most 100), not the number of items returned.
type List[T any] struct {
	ctx    context.Context
	cursor string
	fetch  func(ctx context.Context, cursor string) (*Page[T], error)
}

func newList[T any](ctx context.Context, c *Client, req request, cursor string, opts []Option) *List[T] {
	return &List[T]{ctx: ctx, cursor: cursor, fetch: func(ctx context.Context, cursor string) (*Page[T], error) {
		pageReq := req
		if cursor != "" {
			pageReq.query = cloneValues(req.query)
			pageReq.query.Set("cursor", cursor)
		}
		body, info, err := c.do(ctx, pageReq, opts)
		if err != nil {
			return nil, err
		}
		page := &Page[T]{LastResponse: info}
		if err := json.Unmarshal(body, page); err != nil {
			return nil, fmt.Errorf("vojo: the API returned HTTP %d with a body that could not be decoded: %w", info.Status, err)
		}
		return page, nil
	}}
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, values := range v {
		out[k] = values
	}
	return out
}

// failedList is a list whose every use reports err, for a bad id caught before sending.
func failedList[T any](err error) *List[T] {
	return &List[T]{ctx: context.Background(), fetch: func(context.Context, string) (*Page[T], error) { return nil, err }}
}

// FirstPage fetches one page.
func (l *List[T]) FirstPage() (*Page[T], error) { return l.fetch(l.ctx, l.cursor) }

// Pages iterates page by page. An error is yielded once, then iteration ends.
func (l *List[T]) Pages() iter.Seq2[*Page[T], error] {
	return func(yield func(*Page[T], error) bool) {
		cursor := l.cursor
		seen := map[string]bool{}
		// The caller's own starting cursor counts as followed.
		if cursor != "" {
			seen[cursor] = true
		}
		for {
			page, err := l.fetch(l.ctx, cursor)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(page, nil) {
				return
			}
			if cursor, err = nextCursor(page.Meta, seen); err != nil {
				yield(nil, err)
				return
			}
			if cursor == "" {
				return
			}
		}
	}
}

// All iterates every item across pages, fetching as it goes:
//
//	for screen, err := range client.Screens.List(ctx, nil).All() { ... }
func (l *List[T]) All() iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		for page, err := range l.Pages() {
			if err != nil {
				yield(zero, err)
				return
			}
			for _, item := range page.Data {
				if !yield(item, nil) {
					return
				}
			}
		}
	}
}
