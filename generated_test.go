package vojo

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

type manifestOp struct {
	OperationID string   `json:"operation_id"`
	HTTPMethod  string   `json:"http_method"`
	Path        string   `json:"path"`
	Resource    []string `json:"resource"`
	Method      string   `json:"method"`
	PathParams  []string `json:"path_params"`
	Idempotency bool     `json:"idempotency_key"`
	RetrySafe   bool     `json:"retry_safe"`
	EarlyAccess bool     `json:"available_in_early_access"`
	Response    struct {
		Returns string `json:"returns"`
		IsArray bool   `json:"is_array"`
	} `json:"response"`
}

func readShared(t *testing.T, name string) []byte {
	t.Helper()
	for _, dir := range []string{"shared", "../shared"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return data
		}
	}
	t.Fatalf("%s not found in shared/ or ../shared/: the API files and vectors these tests use must be alongside the package", name)
	return nil
}

func manifest(t *testing.T) []manifestOp {
	t.Helper()
	var file struct {
		Operations []manifestOp `json:"operations"`
	}
	if err := json.Unmarshal(readShared(t, "api/operations.json"), &file); err != nil {
		t.Fatal(err)
	}
	return file.Operations
}

func goExported(name string) string {
	spaced := regexp.MustCompile(`([a-z0-9])([A-Z])`).ReplaceAllString(name, "$1 $2")
	out := ""
	for _, word := range strings.Fields(spaced) {
		out += strings.ToUpper(word[:1]) + word[1:]
	}
	return out
}

// invoke calls an operation's generated method through reflection, by the names in the manifest.
func invoke(t *testing.T, c *Client, op manifestOp) []reflect.Value {
	t.Helper()
	target := reflect.ValueOf(c)
	for _, part := range op.Resource {
		target = target.Elem().FieldByName(goExported(part))
		if !target.IsValid() {
			t.Fatalf("%s: no resource %s", op.OperationID, part)
		}
	}
	method := target.MethodByName(goExported(op.Method))
	if !method.IsValid() {
		t.Fatalf("%s: no method %s", op.OperationID, op.Method)
	}
	kind := method.Type()
	args := []reflect.Value{reflect.ValueOf(ctx)}
	for _, param := range op.PathParams {
		args = append(args, reflect.ValueOf("id_"+param))
	}
	for i := len(args); i < kind.NumIn()-1; i++ {
		args = append(args, reflect.Zero(kind.In(i)))
	}
	results := method.Call(args)
	if op.Response.Returns == "page" {
		first := results[0].MethodByName("FirstPage").Call(nil)
		return first
	}
	return results
}

func responseFor(op manifestOp) *http.Response {
	switch {
	case op.Response.Returns == "void":
		return respond(204, "")
	case op.Response.IsArray:
		return respond(200, `{"data":[{}],"meta":{"next_cursor":null,"has_more":false}}`)
	}
	return respond(200, `{"data":{},"meta":{"scope":{"type":"org"}}}`)
}

func lastError(results []reflect.Value) error {
	last := results[len(results)-1].Interface()
	if last == nil {
		return nil
	}
	return last.(error)
}

func TestEveryOperationMatchesTheManifest(t *testing.T) {
	ops := manifest(t)
	if len(ops) != 72 {
		t.Fatalf("got %d operations", len(ops))
	}
	for _, op := range ops {
		t.Run(op.OperationID, func(t *testing.T) {
			s := &stub{script: []any{responseFor(op)}}
			c, _ := newStubClient(s)
			if err := lastError(invoke(t, c, op)); err != nil {
				t.Fatal(err)
			}
			r := s.requests[0]
			want := regexp.MustCompile(`\{(\w+)\}`).ReplaceAllString(op.Path, "id_$1")
			if r.Method != op.HTTPMethod || r.URL.Path != want {
				t.Fatalf("sent %s %s", r.Method, r.URL.Path)
			}
			if (r.Header.Get("Idempotency-Key") != "") != op.Idempotency {
				t.Fatalf("idempotency header: %q", r.Header.Get("Idempotency-Key"))
			}
		})
	}
}

func TestRetryTableFollowsTheManifest(t *testing.T) {
	for _, op := range manifest(t) {
		t.Run(op.OperationID, func(t *testing.T) {
			s := &stub{script: []any{problem(500, "internal_error"), responseFor(op)}}
			c, _ := newStubClient(s)
			err := lastError(invoke(t, c, op))
			if op.RetrySafe {
				if err != nil || len(s.requests) != 2 {
					t.Fatalf("%v %d", err, len(s.requests))
				}
			} else if !errors.Is(err, ErrServer) || len(s.requests) != 1 {
				t.Fatalf("%v %d", err, len(s.requests))
			}
		})
	}
}

func TestNotFoundHintOnlyOnOperationsNotServedYet(t *testing.T) {
	seen := map[bool]bool{}
	for _, op := range manifest(t) {
		s := &stub{script: []any{problem(404, "not_found")}}
		c, _ := newStubClient(s)
		err := lastError(invoke(t, c, op))
		if strings.Contains(err.Error(), "early access") == op.EarlyAccess {
			t.Fatalf("%s: %v", op.OperationID, err)
		}
		seen[op.EarlyAccess] = true
	}
	if !seen[true] || !seen[false] {
		t.Fatal("did not see both kinds")
	}
}

func TestBodyFieldsLeftOutNullAndSet(t *testing.T) {
	s := &stub{script: []any{ok()}}
	c, _ := newStubClient(s)
	c.Screens.Update(ctx, "a", &UpdateScreenRequest{Name: Ptr("n"), Brand: Null[string](), Notes: Value("x")})
	var body map[string]any
	if err := json.Unmarshal([]byte(s.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 3 || body["name"] != "n" || body["notes"] != "x" {
		t.Fatalf("got %s", s.bodies[0])
	}
	if value, present := body["brand"]; !present || value != nil {
		t.Fatalf("brand should be sent as null: %s", s.bodies[0])
	}
}

func TestQueryValuesLeftOutAreNotSent(t *testing.T) {
	s := &stub{script: []any{page("", false)}}
	c, _ := newStubClient(s)
	c.Screens.List(ctx, &ScreensListQuery{HasHarvester: Ptr(false)}).FirstPage()
	if got := s.requests[0].URL.RawQuery; got != "has_harvester=false" {
		t.Fatalf("got %q", got)
	}
}

func TestResponseDetailsAreExposed(t *testing.T) {
	s := &stub{script: []any{respond(201, `{"data":{"id":"w"},"meta":{"warnings":["screen_id_missing"]}}`,
		"Vojo-Request-Id", "req_9", "Idempotent-Replayed", "true")}}
	c, _ := newStubClient(s)
	w, err := c.Wrappers.Create(ctx, &CreateWrapperRequest{})
	if err != nil {
		t.Fatal(err)
	}
	info := w.LastResponse
	if info.Status != 201 || info.RequestID != "req_9" || !info.IdempotentReplayed || !strings.Contains(string(info.Meta), "screen_id_missing") {
		t.Fatalf("%+v", info)
	}
}

func TestEnvelopeKeepsMeta(t *testing.T) {
	s := &stub{script: []any{respond(200, `{"data":{},"meta":{"generated_at":"t"}}`)}}
	c, _ := newStubClient(s)
	e, err := c.Stats.Plays.Retrieve(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		GeneratedAt string `json:"generated_at"`
	}
	if e.DecodeMeta(&meta) != nil || meta.GeneratedAt != "t" {
		t.Fatalf("%s", e.Meta)
	}
}

func TestBaseURLToleratesTrailingSlashOrV1(t *testing.T) {
	for _, base := range []string{"https://example.test/", "https://example.test/v1", "https://example.test/v1/"} {
		s := &stub{script: []any{ok()}}
		c, _ := newStubClient(s, WithBaseURL(base))
		c.Screens.Retrieve(ctx, "a")
		if got := s.requests[0].URL.String(); got != "https://example.test/v1/screens/a" {
			t.Fatalf("%s gave %s", base, got)
		}
	}
}

func TestRequestIDFallsBackToTheHeader(t *testing.T) {
	s := &stub{script: []any{respond(404, "nope", "Vojo-Request-Id", "req_h")}}
	c, _ := newStubClient(s)
	_, err := c.Screens.Retrieve(ctx, "a")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RequestID != "req_h" {
		t.Fatalf("%v", err)
	}
}
