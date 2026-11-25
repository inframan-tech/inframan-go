package inframan

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPollChecksUntilDone(t *testing.T) {
	n := 0
	err := Poll(context.Background(), time.Millisecond, func(context.Context) (bool, error) {
		n++
		return n == 3, nil
	})
	if err != nil || n != 3 {
		t.Errorf("%v after %d checks", err, n)
	}

	boom := errors.New("boom")
	if err := Poll(context.Background(), time.Millisecond, func(context.Context) (bool, error) { return false, boom }); err != boom {
		t.Errorf("a check's error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := Poll(ctx, 5*time.Millisecond, func(context.Context) (bool, error) { return false, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a context that ended: %v", err)
	}
	if err := Poll(context.Background(), 0, func(context.Context) (bool, error) { return true, nil }); err == nil {
		t.Error("an interval of zero was taken")
	}
}

func TestPagesFollowsACursor(t *testing.T) {
	api := newFakeAPI(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		next := map[string]string{"": "c1", "c1": "c2", "c2": ""}[r.URL.Query().Get("cursor")]
		json.NewEncoder(w).Encode(map[string]any{"runs": []string{r.URL.Query().Get("cursor")}, "next_cursor": next})
	})
	c := newClient(t, api.URL)
	var pages []string
	err := c.ListAgentRunsPages(context.Background(), ListAgentRunsParams{ServiceID: "agent-1", Limit: Ptr(2)}, func(raw json.RawMessage) error {
		pages = append(pages, string(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 {
		t.Fatalf("%d pages, want 3", len(pages))
	}
	reqs := api.requests()
	for i, want := range []string{"limit=2", "cursor=c1&limit=2", "cursor=c2&limit=2"} {
		if reqs[i].Query != want || reqs[i].Path != "/api/v1/capabilities/agents/agent-1/runs" {
			t.Errorf("page %d asked %s?%s", i+1, reqs[i].Path, reqs[i].Query)
		}
	}

	// StopPaging stops with no error.
	n := 0
	err = c.ListAgentRunsPages(context.Background(), ListAgentRunsParams{ServiceID: "agent-1"}, func(json.RawMessage) error {
		n++
		return StopPaging
	})
	if err != nil || n != 1 {
		t.Errorf("StopPaging: %v after %d pages", err, n)
	}
}

func TestPagesFollowsAnOffset(t *testing.T) {
	// get_agent_run's next_offset is its run's, -1 at the end.
	api := newFakeAPI(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		next := map[string]int{"": 200, "200": 400, "400": -1}[r.URL.Query().Get("offset")]
		json.NewEncoder(w).Encode(map[string]any{"run": map[string]any{"events": []any{}, "next_offset": next}})
	})
	c := newClient(t, api.URL)
	n := 0
	err := c.GetAgentRunPages(context.Background(), GetAgentRunParams{ServiceID: "a", RunID: "r"}, func(json.RawMessage) error {
		n++
		return nil
	})
	if err != nil || n != 3 {
		t.Errorf("%v after %d pages", err, n)
	}
	if q := api.requests()[2].Query; q != "offset=400" {
		t.Errorf("the third page asked %q", q)
	}

	// get_service_vulnerabilities' is at the top, null at the end, beside its
	// repository's.
	vulns := newFakeAPI(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		var next any
		if r.URL.Query().Get("offset") == "" {
			next = 25
		}
		json.NewEncoder(w).Encode(map[string]any{"next_offset": next, "repository": map[string]any{"next_offset": 99}})
	})
	c = newClient(t, vulns.URL)
	n = 0
	err = c.GetServiceVulnerabilitiesPages(context.Background(), GetServiceVulnerabilitiesParams{ServiceID: "s"}, func(json.RawMessage) error {
		n++
		return nil
	})
	if err != nil || n != 2 {
		t.Errorf("%v after %d pages", err, n)
	}

	// An offset that does not move is refused rather than read for ever.
	stuck := newFakeAPI(t, jsonAnswer(200, `{"next_offset": 0}`))
	c = newClient(t, stuck.URL)
	err = c.Pages(context.Background(), "get_service_vulnerabilities", map[string]any{"service_id": "s", "offset": 0}, func(json.RawMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "next_offset") {
		t.Errorf("an offset that does not move: %v", err)
	}
}

func TestNextOfReadsTheTopOrTheOneObjectThatCarriesIt(t *testing.T) {
	for raw, want := range map[string]string{
		`{"next_cursor": "a"}`:                               `"a"`,
		`{"next_cursor": null, "x": {"next_cursor": 1}}`:     `null`,
		`{"run": {"next_cursor": "b"}}`:                      `"b"`,
		`{"a": {"next_cursor": 1}, "b": {"next_cursor": 2}}`: ``,
		`[{"next_cursor": 1}]`:                               ``,
		`{"list": [1, 2]}`:                                   ``,
	} {
		got, ok := nextOf(json.RawMessage(raw), "next_cursor")
		if string(got) != want || ok != (want != "") {
			t.Errorf("nextOf(%s) = %s, %v; want %s", raw, got, ok, want)
		}
	}
}

func TestPagesRefusesWhatIsNotPaged(t *testing.T) {
	c := newClient(t, "https://x.example")
	if err := c.Pages(context.Background(), "get_application", map[string]any{"application_id": "a"}, func(json.RawMessage) error { return nil }); err == nil {
		t.Error("a capability that takes no cursor or offset was paged")
	}

	stuck := newFakeAPI(t, jsonAnswer(200, `{"next_cursor": "same"}`))
	c = newClient(t, stuck.URL)
	err := c.ListAgentRunsPages(context.Background(), ListAgentRunsParams{ServiceID: "a"}, func(json.RawMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "next_cursor") {
		t.Errorf("an answer that repeats its cursor: %v", err)
	}
}

// Every capability of the spec is registered with the route it says, and its
// typed method calls it there.
func TestTheGeneratedMethodsCallTheirRoutes(t *testing.T) {
	if len(Capabilities) == 0 {
		t.Fatal("no capabilities were generated")
	}
	for _, c := range Capabilities {
		r, ok := RouteOf(c.Name)
		if !ok {
			t.Fatalf("%s is not registered", c.Name)
		}
		if r.Method != c.Method || r.Path != c.Path || r.ReadOnly == c.Mutating {
			t.Errorf("%s is registered as %+v", c.Name, r)
		}
	}

	api := newFakeAPI(t, jsonAnswer(200, `{"run": {"id": "r-1", "status": "queued"}}`))
	c := newClient(t, api.URL, WithTokenSource(StaticToken("tok")))
	ctx := context.Background()
	raw, err := c.RunAgent(ctx, RunAgentParams{ServiceID: "agent-1", Input: Ptr("Build the app SPEC.md describes.")})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Run struct{ ID, Status string }
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Run.ID != "r-1" {
		t.Errorf("answer %s: %v", raw, err)
	}
	if _, err := c.GetAgentRun(ctx, GetAgentRunParams{ServiceID: "agent-1", RunID: "r-1", Offset: Ptr(0), Limit: Ptr(50)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateRevisionSetting(ctx, UpdateRevisionSettingParams{ApplicationID: "app-1", Mode: UpdateRevisionSettingModeFixed}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetAgentRun(ctx, GetAgentRunParams{ServiceID: "agent-1"}); !errors.Is(err, ErrMissingParameter) {
		t.Errorf("a typed call with a required parameter empty: %v", err)
	}

	reqs := api.requests()
	if r := reqs[0]; r.Method != "POST" || r.Path != "/api/v1/capabilities/agents/agent-1/runs" || r.Body != `{"input":"Build the app SPEC.md describes."}` {
		t.Errorf("run_agent sent %s %s %s", r.Method, r.Path, r.Body)
	}
	if r := reqs[1]; r.Method != "GET" || r.Path != "/api/v1/capabilities/agents/agent-1/runs/r-1" || r.Body != "" {
		t.Errorf("get_agent_run sent %s %s %q", r.Method, r.Path, r.Body)
	} else if q, _ := url.ParseQuery(r.Query); q.Get("limit") != "50" || q.Get("offset") != "0" {
		t.Errorf("get_agent_run's query %q", r.Query)
	}
	var mode map[string]string
	json.Unmarshal([]byte(reqs[2].Body), &mode)
	if reqs[2].Method != "PATCH" || reqs[2].Path != "/api/v1/capabilities/applications/app-1/revision-setting" || mode["mode"] != "fixed" {
		t.Errorf("update_revision_setting sent %s %s", reqs[2].Method, reqs[2].Body)
	}
	if len(reqs) != 3 {
		t.Errorf("%d requests, want 3", len(reqs))
	}
}

func TestCapabilityNamed(t *testing.T) {
	c, ok := CapabilityNamed("run_agent")
	if !ok || !c.Mutating || !c.HasSurface(SurfaceAgent) || c.Method != "POST" {
		t.Errorf("run_agent: %+v", c)
	}
	if _, ok := CapabilityNamed("nope"); ok {
		t.Error("an unknown capability was found")
	}
	if SpecHash == "" || !strings.HasPrefix(SpecHash, "sha256:") {
		t.Errorf("SpecHash %q", SpecHash)
	}
}
