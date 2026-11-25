package inframan

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorded is one request a fake API was sent.
type recorded struct {
	Method, Path, RawPath, Query, Body string
	Header                             http.Header
}

// fakeAPI answers with answer and records every request.
type fakeAPI struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []recorded
}

func newFakeAPI(t *testing.T, answer func(n int, w http.ResponseWriter, r *http.Request)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery, string(body), r.Header.Clone()})
		n := len(f.reqs)
		f.mu.Unlock()
		answer(n, w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded{}, f.reqs...)
}

func jsonAnswer(status int, body string) func(int, http.ResponseWriter, *http.Request) {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// sleeps records the waits between tries, and waits for none.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return ctx.Err()
}

func newClient(t *testing.T, base string, opts ...Option) *Client {
	t.Helper()
	c, err := New(base+"/api/", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func init() {
	RegisterRoute("test_place", Route{
		Method: "POST", Path: "/v1/things/:thing_id/runs/:thing_id_extra",
		In:       map[string]string{"thing_id": "path", "thing_id_extra": "path", "dry_run": "query", "limit": "query", "input": "body"},
		Required: []string{"thing_id"},
	})
	RegisterRoute("test_read", Route{Method: "GET", Path: "/v1/things/:thing_id", In: map[string]string{"thing_id": "path", "verbose": "query"}})
	RegisterRoute("test_delete", Route{Method: "DELETE", Path: "/v1/things/:thing_id", In: map[string]string{"thing_id": "path"}})
	RegisterRoute("test_check", Route{Method: "POST", Path: "/v1/check", In: map[string]string{"doc": "body"}, ReadOnly: true})
}

func TestCallPutsEachArgumentWhereItsRouteSays(t *testing.T) {
	api := newFakeAPI(t, jsonAnswer(200, `{"run": {"id": "r-1"}}`))
	c := newClient(t, api.URL, WithTokenSource(StaticToken("tok")), WithInstance("inst-1"), WithUserAgent("rak/1"))
	var out struct {
		Run struct{ ID string } `json:"run"`
	}
	err := c.Call(context.Background(), "test_place", map[string]any{
		"thing_id": "a/b", "thing_id_extra": "x y", "dry_run": true, "limit": 7,
		"input": "go", "extra": map[string]any{"k": 1}, "nothing": nil,
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.ID != "r-1" {
		t.Errorf("decoded %+v", out)
	}
	r := api.requests()[0]
	if r.Method != "POST" || r.RawPath != "/api/v1/things/a%2Fb/runs/x%20y" {
		t.Errorf("sent %s %s, want POST /api/v1/things/a%%2Fb/runs/x%%20y: a path value is escaped, a name that begins another is not taken for it", r.Method, r.RawPath)
	}
	if r.Query != "dry_run=true&limit=7" {
		t.Errorf("query %q", r.Query)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["input"] != "go" || body["extra"] == nil {
		t.Errorf("body %v: input and an argument the route does not name, nothing nil", body)
	}
	for k, want := range map[string]string{
		"Authorization": "Bearer tok", InstanceHeader: "inst-1", "User-Agent": "rak/1",
		"Content-Type": "application/json", "Accept": "application/json",
	} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
}

func TestAReadOrADeleteWithNoBodySendsNone(t *testing.T) {
	api := newFakeAPI(t, jsonAnswer(204, ``))
	c := newClient(t, api.URL)
	if err := c.Call(context.Background(), "test_read", map[string]any{"thing_id": "t", "verbose": false}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), "test_delete", map[string]any{"thing_id": "t"}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	for _, r := range api.requests() {
		if r.Body != "" || r.Header.Get("Content-Type") != "" {
			t.Errorf("%s sent a body %q", r.Method, r.Body)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("a client with no token source signed a request")
		}
	}
	if q := api.requests()[0].Query; q != "verbose=false" {
		t.Errorf("query %q", q)
	}
}

func TestCallRefusesWhatItCannotSend(t *testing.T) {
	api := newFakeAPI(t, jsonAnswer(200, `{}`))
	c := newClient(t, api.URL)
	ctx := context.Background()
	for name, args := range map[string]map[string]any{
		"a required parameter left out": {"thing_id_extra": "x"},
		"a required parameter empty":    {"thing_id": "", "thing_id_extra": "x"},
		"a path parameter left out":     {"thing_id": "t"},
		"a path parameter empty":        {"thing_id": "t", "thing_id_extra": ""},
	} {
		if err := c.Call(ctx, "test_place", args, nil); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	err := c.Call(ctx, "test_place", map[string]any{"thing_id_extra": "x"}, nil)
	if !errors.Is(err, ErrMissingParameter) || !strings.Contains(err.Error(), `"thing_id"`) {
		t.Errorf("a missing parameter: %v", err)
	}
	if err := c.Call(ctx, "no_such_capability", nil, nil); err == nil {
		t.Error("an unknown capability was called")
	}
	if n := len(api.requests()); n != 0 {
		t.Errorf("%d requests sent, want none", n)
	}
}

func TestAnAnswerThatIsNot2xxIsAnError(t *testing.T) {
	cases := []struct {
		status            int
		body              string
		code, msg         string
		retryAfterSeconds int
	}{
		{404, `{"error": "This agent has no run of that ID", "code": "not_found"}`, "not_found", "This agent has no run of that ID", 0},
		{401, `{"error": "access_revoked", "message": "Your access has been modified."}`, "access_revoked", "Your access has been modified.", 0},
		{403, `{"error": "service accounts may call agent capabilities alone", "code": "service_account_scope"}`, "service_account_scope", "service accounts may call agent capabilities alone", 0},
		{500, `upstream exploded`, "", "upstream exploded", 0},
		{400, ``, "", "", 0},
	}
	for _, tc := range cases {
		api := newFakeAPI(t, jsonAnswer(tc.status, tc.body))
		c := newClient(t, api.URL, WithMaxTries(1))
		err := c.Call(context.Background(), "test_read", map[string]any{"thing_id": "t"}, nil)
		var e *Error
		if !errors.As(err, &e) {
			t.Fatalf("%d: %v is no *Error", tc.status, err)
		}
		if e.Status != tc.status || e.Code != tc.code || e.Message != tc.msg || e.Capability != "test_read" {
			t.Errorf("%d: %+v", tc.status, e)
		}
		if CodeOf(err) != tc.code || StatusOf(err) != tc.status {
			t.Errorf("%d: CodeOf %q, StatusOf %d", tc.status, CodeOf(err), StatusOf(err))
		}
		if tc.status == 404 && e.Body["code"] != "not_found" {
			t.Errorf("the body is not kept: %v", e.Body)
		}
		if !strings.Contains(e.Error(), "test_read") {
			t.Errorf("%q does not name the capability", e.Error())
		}
	}
	if CodeOf(errors.New("x")) != "" || StatusOf(nil) != 0 {
		t.Error("an error that is no *Error has a code or a status")
	}
}

func TestRetriesFollowWhatTheCallChanges(t *testing.T) {
	cases := []struct {
		name   string
		cap    string
		status int
		sent   int // requests sent when every answer is status
	}{
		{"a read, 503", "test_read", 503, 3},
		{"a read, 502", "test_read", 502, 3},
		{"a read, 504", "test_read", 504, 3},
		{"a read, 429", "test_read", 429, 3},
		{"a read, 500", "test_read", 500, 1},
		{"a read over POST, 502", "test_check", 502, 3},
		{"a mutation, 429", "test_place", 429, 3},
		{"a mutation, 503", "test_place", 503, 3},
		{"a mutation, 502", "test_place", 502, 1},
		{"a mutation, 504", "test_place", 504, 1},
		{"a mutation, 409", "test_place", 409, 1},
		{"a delete, 502", "test_delete", 502, 1},
	}
	args := map[string]any{"thing_id": "t", "thing_id_extra": "x", "doc": "d", "input": "same body"}
	for _, tc := range cases {
		api := newFakeAPI(t, jsonAnswer(tc.status, `{"error": "busy"}`))
		var s sleeps
		c := newClient(t, api.URL, WithSleep(s.sleep))
		err := c.Call(context.Background(), tc.cap, args, nil)
		if StatusOf(err) != tc.status {
			t.Errorf("%s: %v", tc.name, err)
		}
		reqs := api.requests()
		if len(reqs) != tc.sent {
			t.Errorf("%s: sent %d times, want %d", tc.name, len(reqs), tc.sent)
		}
		for _, r := range reqs[1:] {
			if r.Body != reqs[0].Body {
				t.Errorf("%s: sent again with another body %q", tc.name, r.Body)
			}
		}
		if len(s.d) != tc.sent-1 {
			t.Errorf("%s: waited %d times between %d tries", tc.name, len(s.d), tc.sent)
		}
		for i, d := range s.d {
			lo, hi := (500*time.Millisecond<<i)/2, 500*time.Millisecond<<i
			if d < lo || d > hi {
				t.Errorf("%s: wait %d was %s, want a backoff in [%s, %s]", tc.name, i+1, d, lo, hi)
			}
		}
	}
}

func TestARetryEndsAtTheFirstGoodAnswer(t *testing.T) {
	api := newFakeAPI(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, `{"ok": true}`)
	})
	var s sleeps
	c := newClient(t, api.URL, WithSleep(s.sleep))
	var out map[string]bool
	if err := c.Call(context.Background(), "test_read", map[string]any{"thing_id": "t"}, &out); err != nil {
		t.Fatal(err)
	}
	if !out["ok"] || len(api.requests()) != 2 {
		t.Errorf("answer %v after %d requests", out, len(api.requests()))
	}
}

func TestRetryAfterIsWaitedOut(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   time.Duration
		sent   int
	}{
		{"2", 2 * time.Second, 2},
		{time.Now().Add(4 * time.Second).UTC().Format(http.TimeFormat), 4 * time.Second, 2},
		{"3600", 0, 1}, // longer than the client waits itself: the caller's
	} {
		api := newFakeAPI(t, func(n int, w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				w.Header().Set("Retry-After", tc.header)
				w.WriteHeader(429)
				io.WriteString(w, `{"error": "slow down", "code": "rate_limited"}`)
				return
			}
			io.WriteString(w, `{}`)
		})
		var s sleeps
		c := newClient(t, api.URL, WithSleep(s.sleep))
		err := c.Call(context.Background(), "test_place", map[string]any{"thing_id": "t", "thing_id_extra": "x"}, nil)
		if n := len(api.requests()); n != tc.sent {
			t.Errorf("Retry-After %q: sent %d times, want %d", tc.header, n, tc.sent)
		}
		if tc.sent == 1 {
			var e *Error
			if !errors.As(err, &e) || e.RetryAfter != time.Hour || e.Code != "rate_limited" {
				t.Errorf("Retry-After %q: %v, want the 429 with its RetryAfter", tc.header, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Retry-After %q: %v", tc.header, err)
		}
		if len(s.d) != 1 || s.d[0] < tc.want-time.Second || s.d[0] > tc.want {
			t.Errorf("Retry-After %q: waited %v, want about %s", tc.header, s.d, tc.want)
		}
	}
}

func TestMaxTriesAndContext(t *testing.T) {
	api := newFakeAPI(t, jsonAnswer(503, `{}`))
	var s sleeps
	c := newClient(t, api.URL, WithSleep(s.sleep), WithMaxTries(1))
	c.Call(context.Background(), "test_read", map[string]any{"thing_id": "t"}, nil)
	if n := len(api.requests()); n != 1 {
		t.Errorf("WithMaxTries(1) sent %d times", n)
	}

	api2 := newFakeAPI(t, jsonAnswer(503, `{}`))
	ctx, cancel := context.WithCancel(context.Background())
	c2 := newClient(t, api2.URL, WithSleep(func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}))
	err := c2.Call(ctx, "test_read", map[string]any{"thing_id": "t"}, nil)
	if StatusOf(err) != 503 || len(api2.requests()) != 1 {
		t.Errorf("a context ended while waiting: %v after %d requests", err, len(api2.requests()))
	}
}

func TestATransportFailureIsRetriedForAReadAlone(t *testing.T) {
	var s sleeps
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c := newClient(t, dead.URL, WithSleep(s.sleep))
	if err := c.Call(context.Background(), "test_read", map[string]any{"thing_id": "t"}, nil); err == nil {
		t.Fatal("a dead server answered")
	}
	if len(s.d) != 2 {
		t.Errorf("a read to a dead server was tried %d times, want 3", len(s.d)+1)
	}
	s.d = nil
	c.Call(context.Background(), "test_place", map[string]any{"thing_id": "t", "thing_id_extra": "x"}, nil)
	if len(s.d) != 0 {
		t.Error("a mutation whose request failed was sent again")
	}
}

// rotating gives "tok-1", "tok-2", … and records what it was told.
type rotating struct {
	mu          sync.Mutex
	n           int
	invalidated []string
}

func (r *rotating) Token(context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 || len(r.invalidated) >= r.n {
		r.n++
	}
	return "tok-" + string(rune('0'+r.n)), nil
}

func (r *rotating) Invalidate(tok string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invalidated = append(r.invalidated, tok)
}

func TestA401IsSignedInAgainOnce(t *testing.T) {
	api := newFakeAPI(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error": "unauthorized"}`)
			return
		}
		io.WriteString(w, `{}`)
	})
	src := &rotating{}
	c := newClient(t, api.URL, WithTokenSource(src))
	if err := c.Call(context.Background(), "test_place", map[string]any{"thing_id": "t", "thing_id_extra": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(src.invalidated) != 1 || src.invalidated[0] != "tok-1" {
		t.Errorf("invalidated %v, want [tok-1]", src.invalidated)
	}
	if n := len(api.requests()); n != 2 {
		t.Errorf("sent %d times, want 2", n)
	}

	// Refused again, it is the caller's.
	never := newFakeAPI(t, jsonAnswer(401, `{"error": "unauthorized"}`))
	src2 := &rotating{}
	c2 := newClient(t, never.URL, WithTokenSource(src2))
	if err := c2.Call(context.Background(), "test_read", map[string]any{"thing_id": "t"}, nil); StatusOf(err) != 401 {
		t.Errorf("a token refused twice: %v", err)
	}
	if n := len(never.requests()); n != 2 {
		t.Errorf("a token refused twice was sent %d times, want 2", n)
	}

	// A source that cannot be told sends once.
	c3 := newClient(t, never.URL, WithTokenSource(StaticToken("x")))
	before := len(never.requests())
	c3.Call(context.Background(), "test_read", map[string]any{"thing_id": "t"}, nil)
	if n := len(never.requests()) - before; n != 1 {
		t.Errorf("a static token refused was sent %d times", n)
	}
}

func TestATokenSourceThatFailsSendsNothing(t *testing.T) {
	api := newFakeAPI(t, jsonAnswer(200, `{}`))
	var s sleeps
	var asked atomic.Int32
	src := TokenFunc(func(context.Context) (string, error) {
		asked.Add(1)
		return "", &Error{Capability: "service_account_token", Status: 403, Code: "not_verified"}
	})
	c := newClient(t, api.URL, WithTokenSource(src), WithSleep(s.sleep))
	err := c.Call(context.Background(), "test_read", map[string]any{"thing_id": "t"}, nil)
	if CodeOf(err) != "not_verified" || !strings.Contains(err.Error(), "sign in") {
		t.Errorf("%v", err)
	}
	if len(api.requests()) != 0 || asked.Load() != 1 {
		t.Errorf("sent %d requests and asked for a token %d times", len(api.requests()), asked.Load())
	}
}

func TestNewRefusesARelativeBase(t *testing.T) {
	for _, base := range []string{"", "/api", "console.example/api", "::"} {
		if _, err := New(base); err == nil {
			t.Errorf("New(%q) was accepted", base)
		}
	}
}
