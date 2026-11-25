package inframan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// InstanceHeader names the instance a request is for, on a backend that
// serves several.
const InstanceHeader = "X-Inframan-Instance"

// Client calls one Inframan instance's API.
type Client struct {
	base     *url.URL
	http     *http.Client
	tokens   TokenSource
	instance string
	agent    string
	maxTries int
	sleep    func(ctx context.Context, d time.Duration) error
}

// Option configures a Client.
type Option func(*Client)

// WithTokenSource signs every request with the token the source gives.
func WithTokenSource(ts TokenSource) Option { return func(c *Client) { c.tokens = ts } }

// WithHTTPClient replaces the HTTP client (60 s time-out by default).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithInstance names the instance every request is for.
func WithInstance(id string) Option { return func(c *Client) { c.instance = id } }

// WithUserAgent names the program in the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Client) { c.agent = ua } }

// DefaultMaxTries is how many times a request is sent at most, by default.
const DefaultMaxTries = 3

// WithMaxTries sets how many times a request is sent at most: DefaultMaxTries
// unless set, and 1 sends it once, never again.
//
// What is sent again: an answer of 429 or 503, for any call, since neither
// did the work; and 502, 504 or a request that never got an answer, only for a
// call that changes nothing (a GET, or a read over POST), since a gateway that
// timed out may have let a change through.
func WithMaxTries(n int) Option {
	return func(c *Client) {
		if n < 1 {
			n = 1
		}
		c.maxTries = n
	}
}

// WithSleep replaces how the client waits between tries (tests). It returns
// ctx's error when ctx ends first.
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = sleep }
}

// New is a client of the API at base, the console's address followed by /api
// ("https://<console domain>/api").
func New(base string, opts ...Option) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("inframan: base URL %q is not an absolute URL", base)
	}
	c := &Client{
		base:     u,
		http:     &http.Client{Timeout: 60 * time.Second},
		agent:    "inframan-go",
		maxTries: DefaultMaxTries,
		sleep:    sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Route is where a capability is served: its HTTP method and path template
// (":name" segments filled from path parameters), and where each of its
// parameters goes. The generated code registers one per capability.
type Route struct {
	Method string
	Path   string
	// In maps a parameter's name to "path", "query" or "body". An argument
	// the map does not name goes in the body.
	In map[string]string
	// Required names the parameters a call must give: Call refuses one that
	// leaves any of them out, nil or "", as the API would.
	Required []string
	// ReadOnly marks a capability that changes nothing whatever its method
	// (a read over POST): it is sent again on a 502 or a 504 as a GET is.
	ReadOnly bool
}

// readOnly says whether sending the request twice is harmless.
func (r Route) readOnly() bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return r.ReadOnly
}

var (
	routesMu sync.RWMutex
	routes   = map[string]Route{}
)

// RegisterRoute makes a capability callable by name. Generated code calls it
// for every capability of the spec; a program may register one the spec it
// was generated from lacks.
func RegisterRoute(name string, r Route) {
	routesMu.Lock()
	defer routesMu.Unlock()
	routes[name] = r
}

// RouteOf is the route registered for a capability.
func RouteOf(name string) (Route, bool) {
	routesMu.RLock()
	defer routesMu.RUnlock()
	r, ok := routes[name]
	return r, ok
}

// ErrMissingParameter is a call that left out a parameter its capability
// requires. It is refused before anything is sent.
var ErrMissingParameter = errors.New("missing required parameter")

// Call invokes a capability by name with its arguments and decodes the answer
// into out (a pointer, or nil to discard it). An answer that is not 2xx is an
// *Error.
func (c *Client) Call(ctx context.Context, name string, args map[string]any, out any) error {
	r, ok := RouteOf(name)
	if !ok {
		return fmt.Errorf("inframan: no capability %q is known to this client", name)
	}
	for _, p := range r.Required {
		if v, present := args[p]; !present || v == nil || v == "" {
			return fmt.Errorf("inframan: %s: %w %q", name, ErrMissingParameter, p)
		}
	}
	pr, err := c.prepare(r, args)
	if err != nil {
		return fmt.Errorf("inframan: %s: %w", name, err)
	}
	return c.do(ctx, name, r, pr, out)
}

// invoke is Call for the generated methods: the answer as it came.
func (c *Client) invoke(ctx context.Context, name string, args map[string]any) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.Call(ctx, name, args, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// prepared is a request that can be sent again: its URL and its body.
type prepared struct {
	method string
	url    string
	body   []byte
}

func (c *Client) prepare(r Route, args map[string]any) (prepared, error) {
	segs := strings.Split(r.Path, "/")
	query := url.Values{}
	body := map[string]any{}
	for k, v := range args {
		if v == nil {
			continue
		}
		switch r.In[k] {
		case "path":
			found := false
			s := stringOf(v)
			if s == "" {
				return prepared{}, fmt.Errorf("path parameter %q is empty", k)
			}
			for i, seg := range segs {
				if seg == ":"+k {
					segs[i] = url.PathEscape(s)
					found = true
				}
			}
			if !found {
				return prepared{}, fmt.Errorf("path %s has no :%s", r.Path, k)
			}
		case "query":
			query.Set(k, stringOf(v))
		default:
			body[k] = v
		}
	}
	for _, seg := range segs {
		if strings.HasPrefix(seg, ":") {
			return prepared{}, fmt.Errorf("path parameter %q of %s is missing", strings.TrimPrefix(seg, ":"), r.Path)
		}
	}
	// The segments are escaped already (an ID holding a "/" keeps it as
	// %2F), so the URL is given its escaped path and the path it decodes to.
	u := *c.base
	escaped := strings.TrimSuffix(c.base.EscapedPath(), "/") + strings.Join(segs, "/")
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return prepared{}, err
	}
	u.Path, u.RawPath = decoded, escaped
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	p := prepared{method: r.Method, url: u.String()}
	if r.Method != http.MethodGet && r.Method != http.MethodDelete || len(body) > 0 {
		raw, err := json.Marshal(body)
		if err != nil {
			return prepared{}, err
		}
		p.body = raw
	}
	return p, nil
}

// request is p as a new request, its body read from the start.
func (p prepared) request(ctx context.Context) (*http.Request, error) {
	var rd io.Reader
	if p.body != nil {
		rd = bytes.NewReader(p.body)
	}
	req, err := http.NewRequestWithContext(ctx, p.method, p.url, rd)
	if err != nil {
		return nil, err
	}
	if p.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func stringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	s := string(raw)
	if strings.HasPrefix(s, "\"") {
		var out string
		_ = json.Unmarshal(raw, &out)
		return out
	}
	return s
}

// maxRetryAfter is the longest Retry-After the client waits out itself; an
// answer asking for longer is the caller's, its RetryAfter set.
const maxRetryAfter = 30 * time.Second

// backoff is the wait before try n+1 when the answer named none: half a
// second doubled each try, at most five, and a random part of it, so clients
// refused together do not come back together.
func backoff(n int) time.Duration {
	d := 500 * time.Millisecond << (n - 1)
	if d > 5*time.Second || d <= 0 {
		d = 5 * time.Second
	}
	return d/2 + rand.N(d/2+1)
}

// retryable says whether an answer of this status is worth sending again.
func retryable(status int, readOnly bool) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return readOnly
	}
	return false
}

func (c *Client) do(ctx context.Context, name string, r Route, p prepared, out any) error {
	maxTries := c.maxTries
	if maxTries < 1 {
		maxTries = 1
	}
	sleep := c.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	signedInAgain := false
	for try := 1; ; try++ {
		status, raw, hdr, tok, err := c.send(ctx, name, p)
		if err != nil {
			var signIn *signInError
			if errors.As(err, &signIn) || ctx.Err() != nil || !r.readOnly() || try >= maxTries {
				return err
			}
			if serr := sleep(ctx, backoff(try)); serr != nil {
				return err
			}
			continue
		}
		if status == http.StatusUnauthorized && !signedInAgain {
			// A token the API refused is dropped and one more is asked for,
			// once: a service account's instance token is renewed so.
			if inv, ok := c.tokens.(Invalidator); ok && tok != "" {
				inv.Invalidate(tok)
				signedInAgain = true
				try--
				continue
			}
		}
		if status < 200 || status > 299 {
			e := errorOf(name, status, hdr, raw)
			if !retryable(status, r.readOnly()) || try >= maxTries {
				return e
			}
			wait := backoff(try)
			if e.RetryAfter > 0 {
				if e.RetryAfter > maxRetryAfter {
					return e
				}
				wait = e.RetryAfter
			}
			if serr := sleep(ctx, wait); serr != nil {
				return e
			}
			continue
		}
		if out == nil || len(bytes.TrimSpace(raw)) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("inframan: %s: decode the answer: %w", name, err)
		}
		return nil
	}
}

// signInError is a token source's failure: never sent again by Call, which
// cannot tell a transient one from a refusal.
type signInError struct{ err error }

func (e *signInError) Error() string { return e.err.Error() }
func (e *signInError) Unwrap() error { return e.err }

// send sends p once: the answer's status, body and headers, and the token it
// was signed with.
func (c *Client) send(ctx context.Context, name string, p prepared) (int, []byte, http.Header, string, error) {
	req, err := p.request(ctx)
	if err != nil {
		return 0, nil, nil, "", fmt.Errorf("inframan: %s: %w", name, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.agent)
	if c.instance != "" {
		req.Header.Set(InstanceHeader, c.instance)
	}
	var tok string
	if c.tokens != nil {
		tok, err = c.tokens.Token(ctx)
		if err != nil {
			return 0, nil, nil, "", &signInError{fmt.Errorf("inframan: %s: sign in: %w", name, err)}
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, tok, fmt.Errorf("inframan: %s: %w", name, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, nil, nil, tok, fmt.Errorf("inframan: %s: read the answer: %w", name, err)
	}
	return resp.StatusCode, raw, resp.Header, tok, nil
}

// Error is an answer that is not 2xx: its status and the API's {error, code}.
type Error struct {
	Capability string
	Status     int
	Code       string
	Message    string
	// RetryAfter is what the answer's Retry-After said, or zero.
	RetryAfter time.Duration
	// Body is the answer's JSON, for the fields a refusal carries beyond
	// error and code.
	Body map[string]any
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("inframan: %s: %d %s: %s", e.Capability, e.Status, e.Code, msg)
	}
	return fmt.Sprintf("inframan: %s: %d: %s", e.Capability, e.Status, msg)
}

// CodeOf is an error's API code, or "" where it is no *Error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// StatusOf is an error's HTTP status, or 0 where it is no *Error.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date.
func retryAfter(s string, now time.Time) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if secs, err := strconv.Atoi(s); err == nil {
		if secs > 0 {
			return time.Duration(secs) * time.Second
		}
		return 0
	}
	if t, err := http.ParseTime(s); err == nil {
		if d := t.Sub(now); d > 0 {
			return d.Round(time.Second)
		}
	}
	return 0
}

func errorOf(name string, status int, hdr http.Header, raw []byte) *Error {
	e := &Error{Capability: name, Status: status}
	if hdr != nil {
		e.RetryAfter = retryAfter(hdr.Get("Retry-After"), time.Now())
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) == nil {
		e.Body = body
		if s, ok := body["code"].(string); ok {
			e.Code = s
		}
		if s, ok := body["error"].(string); ok {
			// Some routes put the code in "error" and words in "message".
			if e.Code == "" && !strings.Contains(s, " ") {
				e.Code = s
			} else {
				e.Message = s
			}
		}
		if s, ok := body["message"].(string); ok && e.Message == "" {
			e.Message = s
		}
	} else {
		e.Message = strings.TrimSpace(string(raw))
		if len(e.Message) > 500 {
			e.Message = e.Message[:500]
		}
	}
	return e
}
