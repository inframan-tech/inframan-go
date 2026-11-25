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

// fakeGoogle is a metadata server and Inframan's exchange in one: the
// metadata server's ID tokens are "id-<n>", the exchange's instance tokens
// "at-<n>".
type fakeGoogle struct {
	*httptest.Server
	idTokens  atomic.Int32
	exchanges atomic.Int32
	expiresIn int
	delay     time.Duration
	refuse    string // a code the exchange refuses with
	mu        sync.Mutex
	bodies    []map[string]string
	audiences []string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{expiresIn: 900}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/computeMetadata/v1/instance/service-accounts/default/identity":
			if r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Query().Get("format") != "full" {
				w.WriteHeader(403)
				io.WriteString(w, "Missing Metadata-Flavor:Google header.")
				return
			}
			f.mu.Lock()
			f.audiences = append(f.audiences, r.URL.Query().Get("audience"))
			f.mu.Unlock()
			n := f.idTokens.Add(1)
			io.WriteString(w, "id-"+itoa(int(n))+"\n")
		case "/api" + ExchangePath:
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.bodies = append(f.bodies, body)
			f.mu.Unlock()
			if f.delay > 0 {
				time.Sleep(f.delay)
			}
			if f.refuse != "" {
				w.WriteHeader(403)
				json.NewEncoder(w).Encode(map[string]string{"error": "the service account is not verified", "code": f.refuse})
				return
			}
			n := f.exchanges.Add(1)
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-" + itoa(int(n)), "token_type": "Bearer",
				"expires_in": f.expiresIn, "instance_id": body["instance_id"],
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	t.Setenv(MetadataHostEnv, strings.TrimPrefix(f.URL, "http://"))
	return f
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestGoogleWorkloadIdentityExchangesTheMetadataServersIDToken(t *testing.T) {
	g := newFakeGoogle(t)
	ts := GoogleWorkloadIdentity(g.URL+"/api", "inst-1")
	tok, err := ts.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "at-1" {
		t.Errorf("token %q, want at-1", tok)
	}
	if len(g.audiences) != 1 || g.audiences[0] != "inframan:inst-1" {
		t.Errorf("asked the metadata server for %v, want the audience inframan:inst-1", g.audiences)
	}
	if want := (map[string]string{"id_token": "id-1", "instance_id": "inst-1"}); len(g.bodies) != 1 ||
		g.bodies[0]["id_token"] != want["id_token"] || g.bodies[0]["instance_id"] != want["instance_id"] {
		t.Errorf("exchanged %v, want %v", g.bodies, want)
	}
	if _, ok := ts.(Invalidator); !ok {
		t.Error("the source cannot be told of a 401")
	}
}

func TestTheInstanceTokenIsKeptUntilTwoMinutesBeforeItExpires(t *testing.T) {
	g := newFakeGoogle(t)
	ts := GoogleWorkloadIdentity(g.URL+"/api", "inst-1").(*exchangeSource)
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	ts.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if tok, _ := ts.Token(ctx); tok != "at-1" {
			t.Fatalf("token %q, want the one kept", tok)
		}
	}
	now = now.Add(12*time.Minute + 59*time.Second)
	if tok, _ := ts.Token(ctx); tok != "at-1" || g.exchanges.Load() != 1 {
		t.Errorf("at 12m59s: %q after %d exchanges, want at-1 after one", tok, g.exchanges.Load())
	}
	now = now.Add(time.Second)
	if tok, _ := ts.Token(ctx); tok != "at-2" || g.exchanges.Load() != 2 {
		t.Errorf("at 13m: %q after %d exchanges, want at-2 after two", tok, g.exchanges.Load())
	}

	// A token due but not expired is used while its renewal fails.
	now = now.Add(13*time.Minute + 30*time.Second)
	g.refuse = "not_verified"
	if tok, err := ts.Token(ctx); err != nil || tok != "at-2" {
		t.Errorf("a renewal refused before the token expired: %q, %v", tok, err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := ts.Token(ctx); CodeOf(err) != "not_verified" {
		t.Errorf("a renewal refused after the token expired: %v", err)
	}
}

func TestAShortLivedTokenIsKeptForHalfItsLife(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		15 * time.Minute: 13 * time.Minute,
		4 * time.Minute:  2 * time.Minute,
		3 * time.Minute:  90 * time.Second,
		10 * time.Second: 5 * time.Second,
	} {
		if got := renewAfter(in); got != want {
			t.Errorf("renewAfter(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestOneExchangeAtATime(t *testing.T) {
	g := newFakeGoogle(t)
	g.delay = 50 * time.Millisecond
	ts := GoogleWorkloadIdentity(g.URL+"/api", "inst-1")
	var wg sync.WaitGroup
	toks := make([]string, 20)
	for i := range toks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			toks[i], _ = ts.Token(context.Background())
		}(i)
	}
	wg.Wait()
	if n := g.exchanges.Load(); n != 1 {
		t.Errorf("20 callers at once made %d exchanges, want 1", n)
	}
	for _, tok := range toks {
		if tok != "at-1" {
			t.Fatalf("a caller got %q", tok)
		}
	}

	// A caller waiting for the exchange may give up.
	g.delay = 300 * time.Millisecond
	ts2 := GoogleWorkloadIdentity(g.URL+"/api", "inst-1")
	go ts2.Token(context.Background())
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := ts2.Token(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a caller waiting with an ended context: %v", err)
	}
}

func TestInvalidateDropsOnlyTheTokenRefused(t *testing.T) {
	g := newFakeGoogle(t)
	ts := GoogleWorkloadIdentity(g.URL+"/api", "inst-1").(*exchangeSource)
	ctx := context.Background()
	ts.Token(ctx)
	ts.Invalidate("at-0") // an older token than the one kept
	if tok, _ := ts.Token(ctx); tok != "at-1" {
		t.Errorf("a stale invalidation dropped the token kept: %q", tok)
	}
	ts.Invalidate("at-1")
	if tok, _ := ts.Token(ctx); tok != "at-2" {
		t.Errorf("after the token was refused: %q, want a new one", tok)
	}
}

func TestACallRefusedWith401ExchangesOnceMore(t *testing.T) {
	g := newFakeGoogle(t)
	var seen []string
	var mu sync.Mutex
	api := newFakeAPI(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer at-2" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error": "unauthorized"}`)
			return
		}
		io.WriteString(w, `{"ok": true}`)
	})
	c := newClient(t, api.URL, WithTokenSource(GoogleWorkloadIdentity(g.URL+"/api", "inst-1")), WithInstance("inst-1"))
	if err := c.Call(context.Background(), "test_read", map[string]any{"thing_id": "t"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Bearer at-1" || seen[1] != "Bearer at-2" {
		t.Errorf("signed with %v, want at-1 then at-2", seen)
	}
	if n := g.exchanges.Load(); n != 2 {
		t.Errorf("%d exchanges, want 2", n)
	}
}

func TestARefusedExchangeKeepsItsCode(t *testing.T) {
	g := newFakeGoogle(t)
	g.refuse = "no_instance_access"
	_, err := GoogleWorkloadIdentity(g.URL+"/api", "inst-1").Token(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.Code != "no_instance_access" || e.Status != 403 || e.Capability != "service_account_token" {
		t.Errorf("%v", err)
	}
}

func TestServiceAccountExchangeTakesAnyIDTokens(t *testing.T) {
	g := newFakeGoogle(t)
	ts := ServiceAccountExchange(g.URL+"/api/", "", StaticToken("my-id-token"))
	if tok, err := ts.Token(context.Background()); err != nil || tok != "at-1" {
		t.Fatalf("%q, %v", tok, err)
	}
	if b := g.bodies[0]; b["id_token"] != "my-id-token" || len(b) != 1 {
		t.Errorf("exchanged %v: the ID token given, and no instance where none was named", b)
	}
	if g.idTokens.Load() != 0 {
		t.Error("the metadata server was asked")
	}

	failing := ServiceAccountExchange(g.URL+"/api", "inst-1", TokenFunc(func(context.Context) (string, error) {
		return "", errors.New("no ID token here")
	}))
	if _, err := failing.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "no ID token here") {
		t.Errorf("an ID token source's failure: %v", err)
	}
}

func TestWhatTheSourcesRefuse(t *testing.T) {
	newFakeGoogle(t)
	for name, ts := range map[string]TokenSource{
		"no instance":     GoogleWorkloadIdentity("https://x.example/api", ""),
		"a relative base": ServiceAccountExchange("/api", "inst-1", StaticToken("x")),
		"no ID tokens":    ServiceAccountExchange("https://x.example/api", "inst-1", nil),
	} {
		if _, err := ts.Token(context.Background()); err == nil {
			t.Errorf("%s: gave a token", name)
		}
	}
}

func TestTheMetadataServersRefusalIsSaid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, "service account not found")
	}))
	defer srv.Close()
	t.Setenv(MetadataHostEnv, strings.TrimPrefix(srv.URL, "http://"))
	_, err := MetadataIDTokens("inframan:inst-1").Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "service account not found") {
		t.Errorf("%v", err)
	}
}
