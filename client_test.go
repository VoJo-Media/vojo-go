package vojo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

var testKey = "vojo_live_" + strings.Repeat("a", 49)

// stub replays scripted responses and records every request.
type stub struct {
	script   []any
	requests []*http.Request
	bodies   []string
}

func (s *stub) RoundTrip(r *http.Request) (*http.Response, error) {
	s.requests = append(s.requests, r)
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	s.bodies = append(s.bodies, body)
	step := s.script[0]
	s.script = s.script[1:]
	switch v := step.(type) {
	case error:
		return nil, v
	case *http.Response:
		return v, nil
	}
	panic("bad script step")
}

func respond(status int, body string, headers ...string) *http.Response {
	h := http.Header{}
	for i := 0; i+1 < len(headers); i += 2 {
		h.Set(headers[i], headers[i+1])
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func problem(status int, code string, headers ...string) *http.Response {
	return respond(status, fmt.Sprintf(`{"title":"t","detail":"why","status":%d,"code":%q,"request_id":"req_1"}`, status, code), headers...)
}

func ok() *http.Response { return respond(200, `{"data":{"id":"scr_1","name":"n"},"meta":{}}`) }

func newStubClient(s *stub, opts ...Option) (*Client, *[]time.Duration) {
	c, err := NewClient(testKey, append([]Option{WithHTTPClient(&http.Client{Transport: s})}, opts...)...)
	if err != nil {
		panic(err)
	}
	slept := &[]time.Duration{}
	c.sleep = func(_ context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		return nil
	}
	return c, slept
}

var ctx = context.Background()

func TestSendsAuthVersionAndOrganisation(t *testing.T) {
	s := &stub{script: []any{ok()}}
	c, _ := newStubClient(s, WithOrganisation("org_1"))
	if _, err := c.Screens.Retrieve(ctx, "scr_1"); err != nil {
		t.Fatal(err)
	}
	h := s.requests[0].Header
	if h.Get("Authorization") != "Bearer "+testKey || h.Get("Vojo-Version") != APIVersion || h.Get("Vojo-Organisation") != "org_1" {
		t.Fatalf("headers: %v", h)
	}
	if h.Get("User-Agent") != "vojo-go/"+Version {
		t.Fatalf("user agent: %q", h.Get("User-Agent"))
	}
}

func TestVersionOverrideAndPerCallOption(t *testing.T) {
	s := &stub{script: []any{ok(), ok()}}
	c, _ := newStubClient(s, WithVersion("2030-01-01"))
	c.Screens.Retrieve(ctx, "a", WithOrganisation("org_2"))
	c.Screens.Retrieve(ctx, "a")
	if s.requests[0].Header.Get("Vojo-Version") != "2030-01-01" || s.requests[0].Header.Get("Vojo-Organisation") != "org_2" {
		t.Fatalf("headers: %v", s.requests[0].Header)
	}
	if s.requests[1].Header.Get("Vojo-Organisation") != "" {
		t.Fatal("a per-call option leaked into the client")
	}
}

func TestTokenSourceIsReadOnEachRequest(t *testing.T) {
	s := &stub{script: []any{ok(), ok()}}
	n := 0
	c, _ := newStubClient(s, WithTokenSource(func(context.Context) (string, error) {
		n++
		return fmt.Sprintf("tok_%d", n), nil
	}))
	c.Screens.Retrieve(ctx, "a")
	c.Screens.Retrieve(ctx, "a")
	if s.requests[1].Header.Get("Authorization") != "Bearer tok_2" {
		t.Fatalf("got %q", s.requests[1].Header.Get("Authorization"))
	}
}

func TestMissingKeyExplainsItself(t *testing.T) {
	t.Setenv("VOJO_SECRET_KEY", "")
	_, err := NewClient("")
	if err == nil || !strings.Contains(err.Error(), "VOJO_SECRET_KEY") {
		t.Fatalf("got %v", err)
	}
}

func TestPathIDsAreEncodedAndEmptyIDsRefused(t *testing.T) {
	s := &stub{script: []any{ok()}}
	c, _ := newStubClient(s)
	c.Screens.Retrieve(ctx, "a/../b")
	if got := s.requests[0].URL.EscapedPath(); got != "/v1/screens/a%2F..%2Fb" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"", ".", ".."} {
		if _, err := c.Screens.Retrieve(ctx, bad); err == nil {
			t.Fatalf("id %q accepted", bad)
		}
	}
}

func TestKeyNeverAppearsInFormattedClientOrErrors(t *testing.T) {
	s := &stub{script: []any{problem(401, "invalid_token")}}
	c, _ := newStubClient(s)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if strings.Contains(fmt.Sprintf(verb, c), testKey) || strings.Contains(fmt.Sprintf(verb, *c), testKey) {
			t.Fatalf("%s exposed the key", verb)
		}
	}
	_, err := c.Screens.Retrieve(ctx, "a")
	if err == nil || strings.Contains(fmt.Sprintf("%+v %#v", err, err), testKey) {
		t.Fatalf("error exposed the key or was nil: %v", err)
	}
}

func TestMalformedKeyErrorDoesNotEchoIt(t *testing.T) {
	_, err := NewClient("vojo_live_hunter2\nx")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("got %v", err)
	}
}

// The API answers a wrong-mode key like any invalid one, so the SDK adds nothing.
func TestTestKey401AddsNoHint(t *testing.T) {
	s := &stub{script: []any{problem(401, "invalid_token")}}
	c, _ := NewClient("vojo_"+"test_abc", WithHTTPClient(&http.Client{Transport: s}))
	_, err := c.Screens.Retrieve(ctx, "a")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "why" {
		t.Fatalf("got %v", err)
	}
}

func TestErrorsMapByStatus(t *testing.T) {
	cases := []struct {
		status   int
		code     string
		sentinel error
	}{
		{400, "invalid_cursor", ErrValidation},
		{401, "invalid_token", ErrAuthentication},
		{402, "entitlement_limit_reached", ErrPermission},
		{403, "permission_denied", ErrPermission},
		{404, "not_found", ErrNotFound},
		{409, "duplicate_name", ErrConflict},
		{413, "payload_too_large", ErrValidation},
		{422, "validation_failed", ErrValidation},
		{429, "rate_limited", ErrRateLimit},
		{500, "internal_error", ErrServer},
	}
	for _, tc := range cases {
		s := &stub{script: []any{problem(tc.status, tc.code)}}
		c, _ := newStubClient(s, WithMaxRetries(0))
		_, err := c.Screens.Retrieve(ctx, "a")
		if !errors.Is(err, tc.sentinel) {
			t.Errorf("%d: not %v: %v", tc.status, tc.sentinel, err)
		}
		if errors.Is(err, ErrPagination) || (tc.sentinel != ErrNotFound && errors.Is(err, ErrNotFound)) {
			t.Errorf("%d matched the wrong sentinel", tc.status)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != tc.status || apiErr.Code != tc.code || apiErr.RequestID != "req_1" || apiErr.Problem["detail"] != "why" {
			t.Errorf("%d: bad error %+v", tc.status, apiErr)
		}
		if !strings.Contains(err.Error(), "req_1") || !strings.Contains(err.Error(), tc.code) {
			t.Errorf("%d: unhelpful message %q", tc.status, err)
		}
	}
}

func TestValidationErrorsAreExposed(t *testing.T) {
	body := `{"code":"validation_failed","status":422,"errors":[{"pointer":"/name","code":"required","detail":"name is required"}]}`
	s := &stub{script: []any{respond(422, body)}}
	c, _ := newStubClient(s)
	_, err := c.Screens.Create(ctx, &CreateScreenRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || len(apiErr.Errors) != 1 || apiErr.Errors[0].Pointer != "/name" {
		t.Fatalf("got %+v", err)
	}
}

func TestNonJSONErrorPageStillMaps(t *testing.T) {
	s := &stub{script: []any{respond(502, "<html>bad gateway</html>")}}
	c, _ := newStubClient(s, WithMaxRetries(0))
	_, err := c.Screens.Retrieve(ctx, "a")
	var apiErr *APIError
	if !errors.Is(err, ErrServer) || !errors.As(err, &apiErr) || apiErr.Code != "" {
		t.Fatalf("got %v", err)
	}
}

func TestEchoedKeysAreRedactedFromMessageAndProblem(t *testing.T) {
	leaked := "vojo_live_" + strings.Repeat("z", 30)
	s := &stub{script: []any{respond(400, `{"code":"invalid_request","detail":"bad `+leaked+`","echo":"`+leaked+`"}`)}}
	c, _ := newStubClient(s)
	_, err := c.Screens.Retrieve(ctx, "a")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || strings.Contains(err.Error(), leaked) || strings.Contains(fmt.Sprint(apiErr.Problem), leaked) {
		t.Fatalf("leaked: %v %v", err, apiErr.Problem)
	}
}

func TestKeysInUnderlyingErrorsAreRedacted(t *testing.T) {
	leaked := "vojo_live_" + strings.Repeat("z", 30)
	s := &stub{script: []any{errors.New("bad header " + leaked)}}
	c, _ := newStubClient(s, WithMaxRetries(0))
	_, err := c.Screens.Retrieve(ctx, "a")
	if err == nil || strings.Contains(err.Error(), leaked) {
		t.Fatalf("got %v", err)
	}
}

func TestNonJSONSuccessIsAClearError(t *testing.T) {
	s := &stub{script: []any{respond(200, "<html>")}}
	c, _ := newStubClient(s)
	_, err := c.Screens.Retrieve(ctx, "a")
	if err == nil || !strings.Contains(err.Error(), "could not be decoded") {
		t.Fatalf("got %v", err)
	}
}

func TestGetRetriesOn5xxThenSucceeds(t *testing.T) {
	s := &stub{script: []any{problem(500, "internal_error"), problem(503, "x"), ok()}}
	c, slept := newStubClient(s)
	screen, err := c.Screens.Retrieve(ctx, "a")
	if err != nil || screen.ID != "scr_1" || len(s.requests) != 3 || len(*slept) != 2 {
		t.Fatalf("%v %v %d %d", screen, err, len(s.requests), len(*slept))
	}
}

func TestRetriesStopAtMaxRetries(t *testing.T) {
	s := &stub{script: []any{problem(500, "x"), problem(500, "x"), problem(500, "x"), problem(500, "x")}}
	c, _ := newStubClient(s, WithMaxRetries(3))
	if _, err := c.Screens.Retrieve(ctx, "a"); !errors.Is(err, ErrServer) || len(s.requests) != 4 {
		t.Fatalf("%v %d", err, len(s.requests))
	}
}

func TestMaxRetriesZeroNeverRetries(t *testing.T) {
	s := &stub{script: []any{problem(500, "x")}}
	c, slept := newStubClient(s, WithMaxRetries(0))
	c.Screens.Retrieve(ctx, "a")
	if len(s.requests) != 1 || len(*slept) != 0 {
		t.Fatal("retried")
	}
}

func Test429HonoursRetryAfter(t *testing.T) {
	s := &stub{script: []any{problem(429, "rate_limited", "Retry-After", "7"), ok()}}
	c, slept := newStubClient(s)
	c.Screens.Retrieve(ctx, "a")
	if len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Fatalf("slept %v", *slept)
	}
}

func Test429IsRetriedEvenForPatch(t *testing.T) {
	s := &stub{script: []any{problem(429, "rate_limited"), ok()}}
	c, _ := newStubClient(s)
	c.Screens.Update(ctx, "a", &UpdateScreenRequest{})
	if len(s.requests) != 2 {
		t.Fatal("not retried")
	}
}

func TestVeryLongRetryAfterIsHandedToTheCaller(t *testing.T) {
	s := &stub{script: []any{problem(429, "rate_limited", "Retry-After", "3600")}}
	c, slept := newStubClient(s)
	_, err := c.Screens.Retrieve(ctx, "a")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || len(*slept) != 0 {
		t.Fatalf("%v %v", err, *slept)
	}
	if wait, ok := apiErr.RetryAfter(); !ok || wait != time.Hour {
		t.Fatalf("retry after %v %v", wait, ok)
	}
}

func TestPatchIsNotRetriedOn5xxOrReadError(t *testing.T) {
	for _, step := range []any{problem(500, "x"), errors.New("connection reset")} {
		s := &stub{script: []any{step}}
		c, _ := newStubClient(s)
		if _, err := c.Screens.Update(ctx, "a", &UpdateScreenRequest{}); err == nil || len(s.requests) != 1 {
			t.Fatalf("%v %d", err, len(s.requests))
		}
	}
}

func TestPatchIsRetriedWhenTheConnectionNeverOpened(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}
	s := &stub{script: []any{dial, ok()}}
	c, _ := newStubClient(s)
	if _, err := c.Screens.Update(ctx, "a", &UpdateScreenRequest{}); err != nil || len(s.requests) != 2 {
		t.Fatalf("%v %d", err, len(s.requests))
	}
}

func Test4xxIsNotRetried(t *testing.T) {
	s := &stub{script: []any{problem(404, "not_found")}}
	c, _ := newStubClient(s)
	c.Screens.Retrieve(ctx, "a")
	if len(s.requests) != 1 {
		t.Fatal("retried")
	}
}

func TestTimeoutIsMarkedAndUnwraps(t *testing.T) {
	timeout := fmt.Errorf("slow: %w", context.DeadlineExceeded)
	s := &stub{script: []any{timeout, timeout, timeout}}
	c, _ := newStubClient(s)
	_, err := c.Screens.Retrieve(ctx, "a")
	var conn *ConnectionError
	if !errors.As(err, &conn) || !conn.Timeout || !errors.Is(err, context.DeadlineExceeded) || len(s.requests) != 3 {
		t.Fatalf("%v %d", err, len(s.requests))
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	s := &stub{script: []any{context.Canceled}}
	c, _ := newStubClient(s)
	_, err := c.Screens.Retrieve(cancelled, "a")
	if !errors.Is(err, context.Canceled) || len(s.requests) != 1 {
		t.Fatalf("%v %d", err, len(s.requests))
	}
}

func TestCreateGeneratesOneKeyUsedByEveryAttempt(t *testing.T) {
	s := &stub{script: []any{errors.New("reset"), problem(500, "x"), respond(201, `{"data":{"id":"a"}}`)}}
	c, _ := newStubClient(s)
	if _, err := c.Screens.Create(ctx, &CreateScreenRequest{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	key := s.requests[0].Header.Get("Idempotency-Key")
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(key) {
		t.Fatalf("not a uuid4: %q", key)
	}
	if len(s.requests) != 3 || s.requests[1].Header.Get("Idempotency-Key") != key || s.requests[2].Header.Get("Idempotency-Key") != key {
		t.Fatal("key changed between attempts")
	}
	var body map[string]string
	if json.Unmarshal([]byte(s.bodies[2]), &body) != nil || body["name"] != "a" {
		t.Fatalf("the retry lost its body: %q", s.bodies[2])
	}
}

func TestEachCreateGetsItsOwnKeyAndCallerCanSupplyOne(t *testing.T) {
	s := &stub{script: []any{ok(), ok(), ok()}}
	c, _ := newStubClient(s)
	c.Screens.Create(ctx, &CreateScreenRequest{})
	c.Screens.Create(ctx, &CreateScreenRequest{})
	c.Screens.Create(ctx, &CreateScreenRequest{}, WithIdempotencyKey("my-key-123"))
	if s.requests[0].Header.Get("Idempotency-Key") == s.requests[1].Header.Get("Idempotency-Key") {
		t.Fatal("keys repeated")
	}
	if s.requests[2].Header.Get("Idempotency-Key") != "my-key-123" {
		t.Fatal("caller key ignored")
	}
}

func TestNonCreatesSendNoKey(t *testing.T) {
	s := &stub{script: []any{ok(), ok()}}
	c, _ := newStubClient(s)
	c.Screens.Retrieve(ctx, "a", WithIdempotencyKey("my-key-123"))
	c.Screens.Update(ctx, "a", &UpdateScreenRequest{})
	for _, r := range s.requests {
		if r.Header.Get("Idempotency-Key") != "" {
			t.Fatal("key sent on a non-create")
		}
	}
}

func TestCreateRetriesWhileEarlierAttemptIsRunning(t *testing.T) {
	s := &stub{script: []any{problem(409, "idempotency_request_in_progress"), ok()}}
	c, _ := newStubClient(s)
	if _, err := c.Screens.Create(ctx, &CreateScreenRequest{}); err != nil || len(s.requests) != 2 {
		t.Fatalf("%v %d", err, len(s.requests))
	}
}

func TestOtherConflictsAreNotRetried(t *testing.T) {
	s := &stub{script: []any{problem(409, "idempotency_key_reused")}}
	c, _ := newStubClient(s)
	if _, err := c.Screens.Create(ctx, &CreateScreenRequest{}); !errors.Is(err, ErrConflict) || len(s.requests) != 1 {
		t.Fatalf("%v %d", err, len(s.requests))
	}
}

func TestUnknownFieldsAreIgnored(t *testing.T) {
	s := &stub{script: []any{respond(200, `{"data":{"id":"a","brand_new":{"x":1}}}`)}}
	c, _ := newStubClient(s)
	if screen, err := c.Screens.Retrieve(ctx, "a"); err != nil || screen.ID != "a" {
		t.Fatalf("%v %v", screen, err)
	}
}

func TestDeleteHandles204(t *testing.T) {
	s := &stub{script: []any{respond(204, "")}}
	c, _ := newStubClient(s)
	if err := c.Screens.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
}

// Paging

func page(next string, hasMore bool, ids ...string) *http.Response {
	items := make([]string, len(ids))
	for i, id := range ids {
		items[i] = fmt.Sprintf(`{"id":%q}`, id)
	}
	return respond(200, fmt.Sprintf(`{"data":[%s],"meta":{"next_cursor":%q,"has_more":%t}}`, strings.Join(items, ","), next, hasMore))
}

func collect(c *Client, params *ScreensListQuery) ([]string, error) {
	var ids []string
	for screen, err := range c.Screens.List(ctx, params).All() {
		if err != nil {
			return ids, err
		}
		ids = append(ids, screen.ID)
	}
	return ids, nil
}

func TestIteratesAcrossPagesPassingCursorAndFilters(t *testing.T) {
	s := &stub{script: []any{page("c1", true, "a", "b"), page("c2", true, "c"), page("", false, "d")}}
	c, _ := newStubClient(s)
	ids, err := collect(c, &ScreensListQuery{SiteID: "s1", Limit: 2})
	if err != nil || strings.Join(ids, "") != "abcd" {
		t.Fatalf("%v %v", ids, err)
	}
	for i, want := range []string{"", "c1", "c2"} {
		q := s.requests[i].URL.Query()
		if q.Get("cursor") != want || q.Get("site_id") != "s1" || q.Get("limit") != "2" {
			t.Fatalf("request %d: %v", i, q)
		}
	}
}

func TestNoRequestUntilIterated(t *testing.T) {
	s := &stub{}
	c, _ := newStubClient(s)
	c.Screens.List(ctx, nil).All()
	if len(s.requests) != 0 {
		t.Fatal("eager request")
	}
}

func TestBreakingOutStopsFetching(t *testing.T) {
	s := &stub{script: []any{page("c1", true, "a", "b")}}
	c, _ := newStubClient(s)
	for range c.Screens.List(ctx, nil).All() {
		break
	}
	if len(s.requests) != 1 {
		t.Fatal("fetched past a break")
	}
}

func TestHasMoreFalseWinsOverACursor(t *testing.T) {
	s := &stub{script: []any{page("c1", false, "a")}}
	c, _ := newStubClient(s)
	if ids, err := collect(c, nil); err != nil || len(ids) != 1 || len(s.requests) != 1 {
		t.Fatalf("%v %v", ids, err)
	}
}

func TestHasMoreWithoutACursorErrors(t *testing.T) {
	s := &stub{script: []any{page("", true, "a")}}
	c, _ := newStubClient(s)
	if _, err := collect(c, nil); !errors.Is(err, ErrPagination) || !strings.Contains(err.Error(), "no next_cursor") {
		t.Fatalf("got %v", err)
	}
}

func TestNullCursorEndsTheList(t *testing.T) {
	s := &stub{script: []any{respond(200, `{"data":[{"id":"a"}],"meta":{"next_cursor":null,"has_more":false}}`)}}
	c, _ := newStubClient(s)
	if ids, err := collect(c, nil); err != nil || len(ids) != 1 {
		t.Fatalf("%v %v", ids, err)
	}
}

func TestRepeatedCursorErrorsAtOnceOrAfterAGap(t *testing.T) {
	for _, script := range [][]any{
		{page("c1", true, "a"), page("c1", true, "a")},
		{page("c1", true, "a"), page("c2", true, "b"), page("c1", true, "c")},
	} {
		n := len(script)
		s := &stub{script: script}
		c, _ := newStubClient(s)
		if _, err := collect(c, nil); !errors.Is(err, ErrPagination) || len(s.requests) != n {
			t.Fatalf("%v %d", err, len(s.requests))
		}
	}
}

func TestAPIErrorMidListIsYieldedThenEnds(t *testing.T) {
	s := &stub{script: []any{page("c1", true, "a"), problem(403, "permission_denied")}}
	c, _ := newStubClient(s)
	ids, err := collect(c, nil)
	if !errors.Is(err, ErrPermission) || len(ids) != 1 {
		t.Fatalf("%v %v", ids, err)
	}
}

func TestTheCallersStartingCursorCountsAsFollowed(t *testing.T) {
	s := &stub{script: []any{page("c1", true, "a")}}
	c, _ := newStubClient(s)
	if _, err := collect(c, &ScreensListQuery{Cursor: "c1"}); !errors.Is(err, ErrPagination) {
		t.Fatalf("got %v", err)
	}
}

func TestFirstPage(t *testing.T) {
	s := &stub{script: []any{page("c2", true, "a")}}
	c, _ := newStubClient(s)
	p, err := c.Screens.List(ctx, &ScreensListQuery{Cursor: "c1"}).FirstPage()
	if err != nil || len(p.Data) != 1 || !p.HasMore() || s.requests[0].URL.Query().Get("cursor") != "c1" || p.LastResponse.Status != 200 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestListPagesGivesPageControl(t *testing.T) {
	s := &stub{script: []any{page("c1", true, "a"), page("", false, "b")}}
	c, _ := newStubClient(s)
	var more []bool
	for p, err := range c.Screens.List(ctx, nil).Pages() {
		if err != nil {
			t.Fatal(err)
		}
		more = append(more, p.HasMore())
	}
	if len(more) != 2 || !more[0] || more[1] {
		t.Fatalf("got %v", more)
	}
}

func TestLongRetryAfterIsHandedBackOnEveryRetriedStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{503, "internal_error"}, {429, "rate_limited"}, {409, "idempotency_request_in_progress"}} {
		s := &stub{script: []any{problem(tc.status, tc.code, "Retry-After", "3600")}}
		c, slept := newStubClient(s)
		_, err := c.Sites.Create(ctx, &CreateSiteRequest{Name: "n"})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
			t.Fatalf("%d: %v", tc.status, err)
		}
		if len(s.requests) != 1 || len(*slept) != 0 {
			t.Fatalf("%d: %d requests, slept %v", tc.status, len(s.requests), *slept)
		}
	}
}

func TestValidRetryAfterIsWaitedFor(t *testing.T) {
	for value, want := range map[string]time.Duration{"0": 0, "2.5": 2500 * time.Millisecond, " 3 ": 3 * time.Second, "60": time.Minute} {
		s := &stub{script: []any{problem(503, "internal_error", "Retry-After", value), ok()}}
		c, slept := newStubClient(s)
		if _, err := c.Screens.Retrieve(ctx, "a"); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if len(*slept) != 1 || (*slept)[0] != want {
			t.Fatalf("%q: slept %v", value, *slept)
		}
	}
}

func TestInvalidRetryAfterIsTreatedAsNotGiven(t *testing.T) {
	for _, value := range []string{"", "   ", "-1", "1e3", "Inf", "NaN", "0x10", "1.", "+5", "Wed, 21 Oct 2026 07:28:00 GMT"} {
		s := &stub{script: []any{problem(429, "rate_limited", "Retry-After", value), ok()}}
		c, slept := newStubClient(s)
		if _, err := c.Screens.Retrieve(ctx, "a"); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if got := (*slept)[0]; got < 250*time.Millisecond || got > 500*time.Millisecond {
			t.Fatalf("%q: slept %v, want the computed delay", value, got)
		}
		if _, ok := (&APIError{Header: http.Header{"Retry-After": {value}}}).RetryAfter(); ok {
			t.Fatalf("%q: RetryAfter should report not given", value)
		}
	}
}

func TestDigitsTooLongForANumberAreLongerThanTheCap(t *testing.T) {
	for _, value := range []string{"99999999999", strings.Repeat("9", 400)} {
		s := &stub{script: []any{problem(429, "rate_limited", "Retry-After", value)}}
		c, slept := newStubClient(s)
		if _, err := c.Screens.Retrieve(ctx, "a"); !errors.Is(err, ErrRateLimit) {
			t.Fatalf("%v", err)
		}
		if len(s.requests) != 1 || len(*slept) != 0 {
			t.Fatalf("%d requests, slept %v", len(s.requests), *slept)
		}
	}
}

func TestA3xxIsAnErrorThatPointsAtTheBaseURL(t *testing.T) {
	for _, status := range []int{301, 302, 304, 307, 308} {
		s := &stub{script: []any{respond(status, "")}}
		c, _ := newStubClient(s)
		_, err := c.Screens.Retrieve(ctx, "a")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != status {
			t.Fatalf("%d: %v", status, err)
		}
		if !strings.Contains(apiErr.Message, "WithBaseURL") || len(s.requests) != 1 {
			t.Fatalf("%d: %q after %d requests", status, apiErr.Message, len(s.requests))
		}
	}
}

func TestRedirectsAreNotFollowedEvenIfTheSuppliedClientWould(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/elsewhere" {
			w.Write([]byte(`{"data":{"id":"wrong"},"meta":{}}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()

	followed := false
	supplied := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		followed = true
		return nil
	}}
	for _, client := range []*http.Client{http.DefaultClient, supplied} {
		seen = nil
		c, err := NewClient(testKey, WithBaseURL(server.URL), WithHTTPClient(client))
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Sites.Create(ctx, &CreateSiteRequest{Name: "n"})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 302 {
			t.Fatalf("%v", err)
		}
		if len(seen) != 1 || seen[0] != "POST /v1/sites" {
			t.Fatalf("server saw %v", seen)
		}
	}
	if followed || http.DefaultClient.CheckRedirect != nil {
		t.Fatal("the supplied client should be neither used to follow nor changed")
	}
	// Still the caller's own function, not one swapped in by the SDK.
	if err := supplied.CheckRedirect(nil, nil); err != nil || !followed {
		t.Fatalf("the supplied client's CheckRedirect was replaced: %v", err)
	}
}

func TestWithIdempotencyKeyIsRefusedOnTheClient(t *testing.T) {
	if _, err := NewClient(testKey, WithIdempotencyKey("one-key")); err == nil || !strings.Contains(err.Error(), "WithIdempotencyKey") {
		t.Fatalf("got %v", err)
	}
}

func invalid(t *testing.T, status int, errorsJSON string) *APIError {
	t.Helper()
	body := `{"title":"t","detail":"2 fields failed validation.","code":"validation_failed"`
	if errorsJSON != "" {
		body += `,"errors":` + errorsJSON
	}
	s := &stub{script: []any{respond(status, body+`}`)}}
	c, _ := newStubClient(s, WithMaxRetries(0))
	_, err := c.Sites.Create(ctx, &CreateSiteRequest{Name: "n"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("%v", err)
	}
	return apiErr
}

func TestValidationMessageNamesTheFieldErrors(t *testing.T) {
	got := invalid(t, 422, `[{"pointer":"/name","code":"required","detail":"Name is required"},{"pointer":"/limit","code":"too_large","detail":"Too large"}]`)
	want := "2 fields failed validation. /name (required): Name is required; /limit (too_large): Too large"
	if got.Message != want || !strings.Contains(got.Error(), "/name (required)") {
		t.Fatalf("got %q", got.Message)
	}
}

func TestValidationMessageListsFiveThenCountsTheRest(t *testing.T) {
	var items []string
	for _, name := range strings.Split("a b c d e f g h", " ") {
		items = append(items, `{"pointer":"/`+name+`","code":"bad","detail":"Bad"}`)
	}
	got := invalid(t, 422, "["+strings.Join(items, ",")+"]")
	if !strings.HasSuffix(got.Message, "/e (bad): Bad; and 3 more") || strings.Contains(got.Message, "/f") || len(got.Errors) != 8 {
		t.Fatalf("got %q, %d errors", got.Message, len(got.Errors))
	}
}

func TestValidationMessageLeavesOutMissingPartsAndJunk(t *testing.T) {
	got := invalid(t, 422, `[{"detail":"Only a detail"},{"pointer":"/a"},{"code":"c","detail":"d"},"junk",null,{}]`)
	if want := "2 fields failed validation. Only a detail; /a; (c): d"; got.Message != want {
		t.Fatalf("got %q", got.Message)
	}
}

func TestValidationMessageTreatsNonStringPartsAsMissing(t *testing.T) {
	got := invalid(t, 422, `[{"pointer":5,"code":{"x":1},"detail":"Only text"},{"pointer":"/a","code":7,"detail":["d"]},{"pointer":true,"code":null,"detail":3}]`)
	if want := "2 fields failed validation. Only text; /a"; got.Message != want {
		t.Fatalf("got %q", got.Message)
	}
}

func TestUnusableEntriesDoNotCountTowardsTheFive(t *testing.T) {
	items := []string{`{}`, `"junk"`, `{"pointer":1}`}
	for _, name := range strings.Split("a b c d e f", " ") {
		items = append(items, `{"pointer":"/`+name+`"}`, `null`)
	}
	got := invalid(t, 422, "["+strings.Join(items, ",")+"]")
	if want := "2 fields failed validation. /a; /b; /c; /d; /e; and 1 more"; got.Message != want {
		t.Fatalf("got %q", got.Message)
	}
}

func TestValidationBaseMessageIsUsedAsGiven(t *testing.T) {
	s := &stub{script: []any{respond(422, `{"title":"Invalid request","code":"validation_failed","errors":[{"pointer":"/a","code":"c","detail":"d"}]}`)}}
	c, _ := newStubClient(s, WithMaxRetries(0))
	_, err := c.Sites.Create(ctx, &CreateSiteRequest{Name: "n"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "Invalid request /a (c): d" {
		t.Fatalf("%v", err)
	}
}

func TestValidationMessageIsUnchangedWithoutFieldErrors(t *testing.T) {
	for _, value := range []string{"", "[]", `"nope"`, "null"} {
		if got := invalid(t, 422, value); got.Message != "2 fields failed validation." {
			t.Fatalf("%s: got %q", value, got.Message)
		}
	}
}

func TestFieldErrorsAreAppendedForValidationKindsOnly(t *testing.T) {
	field := `[{"pointer":"/a","code":"c","detail":"d"}]`
	for _, status := range []int{400, 413} {
		if got := invalid(t, status, field); !strings.HasSuffix(got.Message, "/a (c): d") {
			t.Fatalf("%d: got %q", status, got.Message)
		}
	}
	if got := invalid(t, 409, field); got.Message != "2 fields failed validation." {
		t.Fatalf("409: got %q", got.Message)
	}
}

func TestCancelDuringARetryWaitStopsAtOnce(t *testing.T) {
	s := &stub{script: []any{problem(429, "rate_limited", "Retry-After", "60")}}
	c, err := NewClient(testKey, WithHTTPClient(&http.Client{Transport: s}))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	started := time.Now()
	_, err = c.Screens.Retrieve(cancelled, "a")
	var conn *ConnectionError
	if !errors.As(err, &conn) || !errors.Is(err, context.Canceled) || conn.Timeout {
		t.Fatalf("%v", err)
	}
	if time.Since(started) > 2*time.Second || len(s.requests) != 1 {
		t.Fatalf("took %v, %d requests", time.Since(started), len(s.requests))
	}
}

func TestDeadlineDuringARetryWaitIsMarkedAsATimeout(t *testing.T) {
	s := &stub{script: []any{problem(429, "rate_limited", "Retry-After", "60")}}
	c, err := NewClient(testKey, WithHTTPClient(&http.Client{Transport: s}))
	if err != nil {
		t.Fatal(err)
	}
	limited, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = c.Screens.Retrieve(limited, "a")
	var conn *ConnectionError
	if !errors.As(err, &conn) || !errors.Is(err, context.DeadlineExceeded) || !conn.Timeout {
		t.Fatalf("%v", err)
	}
}

type callerKey struct{}

// stall blocks the first stalls requests until their context ends, then answers
// ok. It records whether each request's context descends from the caller's.
type stall struct {
	stalls   int
	requests int
	inherits []bool
}

func (s *stall) RoundTrip(r *http.Request) (*http.Response, error) {
	s.requests++
	s.inherits = append(s.inherits, r.Context().Value(callerKey{}) == "caller")
	if s.requests <= s.stalls {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return ok(), nil
}

func newStallClient(t *testing.T, s *stall, opts ...Option) *Client {
	t.Helper()
	c, err := NewClient(testKey, append([]Option{WithHTTPClient(&http.Client{Transport: s})}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestAttemptTimeoutUsesAChildContextAndRetries(t *testing.T) {
	s := &stall{stalls: 1}
	c := newStallClient(t, s, WithTimeout(20*time.Millisecond))
	caller := context.WithValue(context.Background(), callerKey{}, "caller")
	if _, err := c.Screens.Retrieve(caller, "a"); err != nil || s.requests != 2 {
		t.Fatalf("%v after %d requests", err, s.requests)
	}
	if !s.inherits[0] || !s.inherits[1] {
		t.Fatalf("attempt contexts must descend from the caller's: %v", s.inherits)
	}
}

func TestAttemptTimeoutOnAnUnsafeCallIsNotRetried(t *testing.T) {
	s := &stall{stalls: 1}
	c := newStallClient(t, s, WithTimeout(20*time.Millisecond))
	_, err := c.Screens.Update(ctx, "a", &UpdateScreenRequest{})
	var conn *ConnectionError
	if !errors.As(err, &conn) || !conn.Timeout || !errors.Is(err, context.DeadlineExceeded) || s.requests != 1 {
		t.Fatalf("%v after %d requests", err, s.requests)
	}
}

func TestCallersDeadlineDuringAnAttemptIsNotRetried(t *testing.T) {
	s := &stall{stalls: 3}
	c := newStallClient(t, s)
	limited, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Screens.Retrieve(limited, "a")
	var conn *ConnectionError
	if !errors.As(err, &conn) || !conn.Timeout || !errors.Is(err, context.DeadlineExceeded) || s.requests != 1 {
		t.Fatalf("%v after %d requests", err, s.requests)
	}
}
