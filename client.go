// Package vojo is the Go client for the Vojo API.
//
//	client, err := vojo.NewClient("vojo_live_...")
//	if err != nil {
//		return err
//	}
//	for screen, err := range client.Screens.List(ctx, nil).All() {
//		if err != nil {
//			return err
//		}
//		fmt.Println(screen.Name)
//	}
package vojo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Version is this library's version.
const Version = "0.1.0"

const (
	// DefaultBaseURL is the production API.
	DefaultBaseURL = "https://api.vojo-media.com"
	// DefaultTimeout applies to each attempt, including reading the response.
	DefaultTimeout = 60 * time.Second
	// DefaultMaxRetries is how many times a failed request is retried when that is safe.
	DefaultMaxRetries = 2

	firstBackoff = 500 * time.Millisecond
	mostBackoff  = 8 * time.Second
	// A longer Retry-After is not slept through: the caller gets the error and decides.
	longestRetryAfter        = 60 * time.Second
	mostFieldErrorsInMessage = 5
)

type config struct {
	secretKey      string
	tokenSource    func(context.Context) (string, error)
	organisation   string
	baseURL        string
	version        string
	timeout        time.Duration
	maxRetries     int
	httpClient     *http.Client
	idempotencyKey string
}

// Option configures a Client, or a single call when passed to a method.
type Option func(*config)

// WithOrganisation sets Vojo-Organisation. OAuth callers must send it; secret
// keys are bound to one organisation and do not need it.
func WithOrganisation(id string) Option { return func(c *config) { c.organisation = id } }

// WithBaseURL points the client somewhere else, such as a local mock.
func WithBaseURL(u string) Option {
	return func(c *config) {
		// Contract paths already include /v1, so a base URL ending in it would double up.
		u = strings.TrimRight(u, "/")
		c.baseURL = strings.TrimRight(strings.TrimSuffix(u, "/v1"), "/")
	}
}

// WithVersion overrides the pinned Vojo-Version.
func WithVersion(date string) Option { return func(c *config) { c.version = date } }

// WithTimeout sets the limit on each attempt. Use the context for an overall limit.
func WithTimeout(d time.Duration) Option { return func(c *config) { c.timeout = d } }

// WithMaxRetries sets how many times a failed request is retried. Zero disables retries.
func WithMaxRetries(n int) Option { return func(c *config) { c.maxRetries = n } }

// WithHTTPClient supplies the HTTP client, for proxies or custom transports.
func WithHTTPClient(h *http.Client) Option { return func(c *config) { c.httpClient = h } }

// WithTokenSource gets the bearer token on each request, for OAuth tokens that
// refresh. Pass an empty key to NewClient with it.
func WithTokenSource(f func(context.Context) (string, error)) Option {
	return func(c *config) { c.tokenSource = f }
}

// WithIdempotencyKey sets the Idempotency-Key on one create call, in place of a
// generated one. It has no effect on other calls. Pass it to the method: NewClient
// refuses it, because one key on every create would replay the first result.
func WithIdempotencyKey(key string) Option { return func(c *config) { c.idempotencyKey = key } }

// Client talks to the Vojo API. It is safe for concurrent use. Its formatted
// form never includes the key.
type Client struct {
	services

	config config
	sleep  func(context.Context, time.Duration) error
}

// NewClient returns a client. An empty secretKey falls back to VOJO_SECRET_KEY.
// Keys are issued by Vojo during early access. OAuth access tokens are passed
// the same way, and those callers must also use WithOrganisation.
func NewClient(secretKey string, opts ...Option) (*Client, error) {
	if secretKey == "" {
		secretKey = os.Getenv("VOJO_SECRET_KEY")
	}
	c := &Client{
		config: config{
			secretKey:  secretKey,
			baseURL:    DefaultBaseURL,
			version:    APIVersion,
			timeout:    DefaultTimeout,
			maxRetries: DefaultMaxRetries,
			httpClient: http.DefaultClient,
		},
		sleep: sleepContext,
	}
	for _, opt := range opts {
		opt(&c.config)
	}
	if c.config.idempotencyKey != "" {
		return nil, errors.New("vojo: WithIdempotencyKey applies to one call: pass it to the method, not NewClient")
	}
	if c.config.secretKey == "" && c.config.tokenSource == nil {
		return nil, errors.New("vojo: no secret key: pass one to NewClient or set VOJO_SECRET_KEY; " +
			"keys are issued by Vojo during early access")
	}
	if c.config.secretKey != "" {
		if err := checkKey(c.config.secretKey); err != nil {
			return nil, err
		}
	}
	c.services = newServices(c)
	return c, nil
}

// Format keeps the key out of every fmt verb, including %+v and %#v. The value
// receiver matters: it covers a Client and a *Client alike.
func (c Client) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "vojo.Client{baseURL: %q, organisation: %q}", c.config.baseURL, c.config.organisation)
}

// ResponseInfo holds the HTTP details of the response behind a result.
type ResponseInfo struct {
	Status             int
	Header             http.Header
	RequestID          string
	Meta               json.RawMessage
	IdempotentReplayed bool
}

// request is one logical API call, which may take several attempts.
type request struct {
	method    string
	path      string
	query     url.Values
	body      any
	retrySafe bool
	// The contract accepts Idempotency-Key on some creates only, so a method opts in.
	keyed bool
	// False for an operation not served during early access: a 404 then says so.
	earlyAccess bool
}

func (c *Client) do(ctx context.Context, req request, opts []Option) ([]byte, *ResponseInfo, error) {
	cfg := c.config
	for _, opt := range opts {
		opt(&cfg)
	}
	// A copy, so the caller's client is left as it was. Following a redirect can
	// turn a POST into a GET that looks like a success.
	httpClient := *cfg.httpClient
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.httpClient = &httpClient

	var payload []byte
	if req.body != nil {
		var err error
		if payload, err = json.Marshal(req.body); err != nil {
			return nil, nil, fmt.Errorf("vojo: could not encode the request: %w", err)
		}
	}
	// Fixed before the first attempt, so every retry of a create carries the same key.
	idempotencyKey := ""
	if req.keyed {
		idempotencyKey = cfg.idempotencyKey
		if idempotencyKey == "" {
			var err error
			if idempotencyKey, err = newUUID(); err != nil {
				return nil, nil, err
			}
		}
	}

	for attempt := 0; ; attempt++ {
		retriesLeft := attempt < cfg.maxRetries
		status, header, body, err := c.attempt(ctx, cfg, req, payload, idempotencyKey)
		if err != nil {
			var conn *ConnectionError
			if !errors.As(err, &conn) || ctx.Err() != nil || !retriesLeft || !shouldRetryError(req, conn.Err) {
				return nil, nil, err
			}
			if err := c.sleep(ctx, backoff(attempt)); err != nil {
				return nil, nil, connectionError(err)
			}
			continue
		}
		if status >= 200 && status < 300 {
			return body, newResponseInfo(status, header, body), nil
		}
		apiErr := newAPIError(status, header, body, req.earlyAccess)
		if !retriesLeft || !shouldRetryStatus(req, apiErr) {
			return nil, nil, apiErr
		}
		wait, ok := apiErr.RetryAfter()
		if !ok {
			wait = backoff(attempt)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, nil, connectionError(err)
		}
	}
}

func (c *Client) attempt(ctx context.Context, cfg config, req request, payload []byte, idempotencyKey string) (int, http.Header, []byte, error) {
	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}
	target := cfg.baseURL + req.path
	if len(req.query) > 0 {
		target += "?" + req.query.Encode()
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, target, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("vojo: could not build the request: %w", err)
	}
	if err := setHeaders(ctx, httpReq, cfg, payload != nil, idempotencyKey); err != nil {
		return 0, nil, nil, err
	}
	resp, err := cfg.httpClient.Do(httpReq)
	if err != nil {
		return 0, nil, nil, connectionError(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, nil, connectionError(err)
	}
	return resp.StatusCode, resp.Header, body, nil
}

func checkKey(key string) error {
	// An HTTP library may quote a bad header value in its own error, so catch it
	// here, where the message can leave the key out.
	for i := 0; i < len(key); i++ {
		if key[i] <= 0x20 || key[i] >= 0x7f {
			return errors.New("vojo: the secret key contains spaces or control characters; check how it was copied")
		}
	}
	return nil
}

func setHeaders(ctx context.Context, r *http.Request, cfg config, hasBody bool, idempotencyKey string) error {
	key := cfg.secretKey
	if cfg.tokenSource != nil {
		var err error
		if key, err = cfg.tokenSource(ctx); err != nil {
			return fmt.Errorf("vojo: could not get a token: %w", err)
		}
	}
	if err := checkKey(key); err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "vojo-go/"+Version)
	r.Header.Set("Vojo-Version", cfg.version)
	if hasBody {
		r.Header.Set("Content-Type", "application/json")
	}
	if cfg.organisation != "" {
		r.Header.Set("Vojo-Organisation", cfg.organisation)
	}
	if idempotencyKey != "" {
		r.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return nil
}

func connectionError(err error) *ConnectionError {
	var netErr net.Error
	timeout := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
	return &ConnectionError{Timeout: timeout, Err: err}
}

func shouldRetryError(req request, err error) bool {
	// A failed dial never reached the server, so resending is safe for anything.
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return req.retrySafe
}

func shouldRetryStatus(req request, e *APIError) bool {
	// 429 was rejected before the handler ran, whatever the method. A 409 here means
	// our earlier attempt is still running, typically after a dropped connection.
	retryable := e.Status == http.StatusTooManyRequests ||
		(e.Status >= 500 && req.retrySafe) ||
		(e.Status == http.StatusConflict && req.keyed && e.Code == "idempotency_request_in_progress")
	if !retryable {
		return false
	}
	wait, ok := e.RetryAfter()
	return !ok || wait <= longestRetryAfter
}

func backoff(attempt int) time.Duration {
	ceiling := mostBackoff
	if attempt < 5 {
		ceiling = min(mostBackoff, firstBackoff<<attempt)
	}
	half := int64(ceiling / 2)
	jitter, err := rand.Int(rand.Reader, big.NewInt(half))
	if err != nil {
		return ceiling
	}
	return time.Duration(half + jitter.Int64())
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newResponseInfo(status int, header http.Header, body []byte) *ResponseInfo {
	info := &ResponseInfo{
		Status:             status,
		Header:             header,
		RequestID:          header.Get("Vojo-Request-Id"),
		IdempotentReplayed: header.Get("Idempotent-Replayed") == "true",
	}
	var envelope struct {
		Meta json.RawMessage `json:"meta"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		info.Meta = envelope.Meta
	}
	return info
}

func newAPIError(status int, header http.Header, body []byte, earlyAccess bool) *APIError {
	e := &APIError{Status: status, Header: header}
	// Redacted before parsing, so an echoed key is gone from Problem as well as Message.
	body = []byte(redact(string(body)))
	// A proxy's HTML error page is not JSON; it still maps by status, without a code.
	if json.Unmarshal(body, &e.Problem) == nil {
		e.Code, _ = e.Problem["code"].(string)
		e.RequestID, _ = e.Problem["request_id"].(string)
		e.Message, _ = e.Problem["detail"].(string)
		if e.Message == "" {
			e.Message, _ = e.Problem["title"].(string)
		}
		var fields struct {
			Errors []FieldError `json:"errors"`
		}
		if json.Unmarshal(body, &fields) == nil {
			e.Errors = fields.Errors
		}
	}
	if e.RequestID == "" {
		e.RequestID = header.Get("Vojo-Request-Id")
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("the API returned HTTP %d", status)
	}
	if isValidationStatus(status) {
		if fields := describeFieldErrors(e.Problem["errors"]); fields != "" {
			e.Message += " " + fields
		}
	}
	if status >= 300 && status < 400 {
		e.Message += "; the API does not redirect, so this client does not follow redirects: check WithBaseURL"
	}
	if status == http.StatusNotFound && !earlyAccess {
		e.Message += " This operation may not be available yet during early access."
	}
	return e
}

func isValidationStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge || status == http.StatusUnprocessableEntity
}

func describeFieldError(value any) string {
	field, _ := value.(map[string]any)
	pointer, _ := field["pointer"].(string)
	code, _ := field["code"].(string)
	detail, _ := field["detail"].(string)
	if code != "" {
		code = "(" + code + ")"
	}
	where := strings.TrimSpace(pointer + " " + code)
	if where != "" && detail != "" {
		return where + ": " + detail
	}
	return where + detail
}

// describeFieldErrors reads the parsed body rather than APIError.Errors, so one
// malformed entry does not lose the rest.
func describeFieldErrors(value any) string {
	items, _ := value.([]any)
	var described []string
	for _, item := range items {
		if text := describeFieldError(item); text != "" {
			described = append(described, text)
		}
	}
	if extra := len(described) - mostFieldErrorsInMessage; extra > 0 {
		described = append(described[:mostFieldErrorsInMessage], fmt.Sprintf("and %d more", extra))
	}
	return strings.Join(described, "; ")
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("vojo: could not generate an idempotency key: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// expandPath fills each {name} in a path from name, value pairs. An empty id or a
// dot segment would silently hit a different route, so those are refused.
func expandPath(path string, pairs ...string) (string, error) {
	for i := 0; i+1 < len(pairs); i += 2 {
		name, value := pairs[i], pairs[i+1]
		if value == "" || value == "." || value == ".." {
			return "", fmt.Errorf("vojo: %s must be a real id, got %q", name, value)
		}
		path = strings.Replace(path, "{"+name+"}", url.PathEscape(value), 1)
	}
	return path, nil
}

// responseSetter is implemented by every generated type returned from a call.
type responseSetter interface{ setResponse(*ResponseInfo) }

// callData sends a call and decodes its data member into a new T.
func callData[T any, P interface {
	*T
	responseSetter
}](ctx context.Context, c *Client, req request, opts []Option) (P, error) {
	body, info, err := c.do(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	out := P(new(T))
	if err := decodeData(info, body, out); err != nil {
		return nil, err
	}
	out.setResponse(info)
	return out, nil
}

// callList sends a call whose data member is a complete list.
func callList[T any](ctx context.Context, c *Client, req request, opts []Option) (*ListResult[T], error) {
	body, info, err := c.do(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	out := &ListResult[T]{LastResponse: info}
	if err := decodeData(info, body, &out.Data); err != nil {
		return nil, err
	}
	return out, nil
}

// callEnvelope sends a call whose meta carries information, keeping data and meta.
func callEnvelope[T any](ctx context.Context, c *Client, req request, opts []Option) (*Envelope[T], error) {
	body, info, err := c.do(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	out := &Envelope[T]{Meta: info.Meta, LastResponse: info}
	if err := decodeData(info, body, &out.Data); err != nil {
		return nil, err
	}
	return out, nil
}

func callVoid(ctx context.Context, c *Client, req request, opts []Option) error {
	_, _, err := c.do(ctx, req, opts)
	return err
}

func decodeData(info *ResponseInfo, body []byte, out any) error {
	envelope := struct {
		Data any `json:"data"`
	}{Data: out}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("vojo: the API returned HTTP %d with a body that could not be decoded: %w", info.Status, err)
	}
	return nil
}

// Ptr returns a pointer to v, for optional fields: vojo.Ptr("Platform 1").
func Ptr[T any](v T) *T { return &v }

// Nullable is an optional body field that can also be sent as null. A nil
// pointer leaves the field out; vojo.Null clears it; vojo.Value sets it.
type Nullable[T any] struct {
	value *T
}

// Null sends the field as null.
func Null[T any]() *Nullable[T] { return &Nullable[T]{} }

// Value sends the field with this value.
func Value[T any](v T) *Nullable[T] { return &Nullable[T]{value: &v} }

// MarshalJSON writes the value, or null.
func (n Nullable[T]) MarshalJSON() ([]byte, error) { return json.Marshal(n.value) }
