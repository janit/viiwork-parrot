package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/janit/viiwork-parrot/internal/node"
	"github.com/janit/viiwork-parrot/internal/schedule"
)

type fakeBackend struct {
	states   map[string]node.ModelStatus
	errs     map[string]error
	override *schedule.Partial
}

func (f *fakeBackend) Status() []node.ModelStatus {
	var out []node.ModelStatus
	for _, s := range f.states {
		out = append(out, s)
	}
	return out
}
func (f *fakeBackend) Ensure(id string) (node.ModelStatus, error) {
	if err := f.errs[id]; err != nil {
		return f.states[id], err
	}
	return f.states[id], nil
}
func (f *fakeBackend) Limits() node.LimitsStatus {
	return node.LimitsStatus{Effective: schedule.Limits{Upload: 1}, Rule: -1, Override: f.override}
}
func (f *fakeBackend) SetOverride(p schedule.Partial) { f.override = &p }
func (f *fakeBackend) ClearOverride()                 { f.override = nil }

func setup(t *testing.T) (*fakeBackend, *Client) {
	fb := &fakeBackend{
		states: map[string]node.ModelStatus{
			"done":    {ID: "done", State: node.StateSeeding, Path: "/models/done.gguf"},
			"busy":    {ID: "busy", State: node.StateDownloading, Percent: 40},
			"broken":  {ID: "broken", State: node.StateFailed, Error: "sha256 mismatch"},
			"nospace": {ID: "nospace", State: node.StatePaused, NoSpace: true},
			"absent":  {ID: "absent", State: node.StateAbsent, Error: "no local copy and models.seed_only_existing is set"},
		},
		errs: map[string]error{"nope": node.ErrUnknownModel, "nospace": node.ErrNoSpace, "early": node.ErrNoCatalog},
	}
	srv := httptest.NewServer(Handler(fb))
	t.Cleanup(srv.Close)
	c := NewClient(strings.TrimPrefix(srv.URL, "http://"))
	return fb, c
}

func TestEnsureCodes(t *testing.T) {
	_, c := setup(t)
	cases := map[string]int{"done": 200, "busy": 202, "broken": 500, "nospace": 507, "nope": 404, "early": 503, "absent": 409}
	for id, want := range cases {
		resp, code, err := c.Ensure(context.Background(), id)
		if err != nil || code != want {
			t.Errorf("%s: code %d err %v, want %d", id, code, err, want)
		}
		if id == "done" && resp.Path != "/models/done.gguf" {
			t.Errorf("done: path %q", resp.Path)
		}
		if (id == "broken" || id == "absent") && resp.Error == "" {
			t.Errorf("%s: error missing", id)
		}
	}
}

func TestLimitsOverride(t *testing.T) {
	fb, c := setup(t)
	up := "20Mbit"
	if err := c.SetOverride(context.Background(), OverrideRequest{Upload: &up}); err != nil {
		t.Fatal(err)
	}
	if fb.override == nil || *fb.override.Upload != 2_500_000 {
		t.Fatalf("%+v", fb.override)
	}
	ls, err := c.Limits(context.Background())
	if err != nil || ls.Override == nil {
		t.Fatalf("%+v %v", ls, err)
	}
	bad := "fast"
	if err := c.SetOverride(context.Background(), OverrideRequest{Upload: &bad}); err == nil {
		t.Fatal("bad rate must 400")
	}
	if err := c.ClearOverride(context.Background()); err != nil || fb.override != nil {
		t.Fatal("clear")
	}
}

func TestStatusAndMethods(t *testing.T) {
	_, c := setup(t)
	st, err := c.Status(context.Background())
	if err != nil || len(st) != 5 {
		t.Fatalf("%v %v", st, err)
	}
	resp, _ := http.Get(c.Base + "/ensure")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /ensure: %d", resp.StatusCode)
	}
}

func rawRequest(t *testing.T, method, url, host, ctype, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestMutationsRequireJSONContentType: a browser can send a "simple"
// cross-origin POST (text/plain, form) to the loopback API without a
// preflight; requiring application/json on every mutating method stops that.
func TestMutationsRequireJSONContentType(t *testing.T) {
	_, c := setup(t)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/ensure", `{"id":"done"}`},
		{"PUT", "/limits/override", `{"upload":"1MB"}`},
		{"DELETE", "/limits/override", ``},
	} {
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			if got := rawRequest(t, tc.method, c.Base+tc.path, "", ct, tc.body); got != http.StatusUnsupportedMediaType {
				t.Errorf("%s %s with Content-Type %q: %d, want 415", tc.method, tc.path, ct, got)
			}
		}
		if got := rawRequest(t, tc.method, c.Base+tc.path, "", "application/json; charset=utf-8", tc.body); got >= 400 {
			t.Errorf("%s %s with application/json: %d", tc.method, tc.path, got)
		}
	}
	if got := rawRequest(t, "GET", c.Base+"/status", "", "", ""); got != http.StatusOK {
		t.Errorf("GET needs no content type: %d", got)
	}
}

// TestRejectsNonLoopbackHost: DNS rebinding — a page on evil.example whose
// name later resolves to 127.0.0.1 reaches the API with Host: evil.example.
func TestRejectsNonLoopbackHost(t *testing.T) {
	_, c := setup(t)
	for _, h := range []string{"evil.example", "evil.example:7950", "10.0.0.1:7950", "localhost.evil.example"} {
		if got := rawRequest(t, "GET", c.Base+"/status", h, "", ""); got != http.StatusMisdirectedRequest {
			t.Errorf("Host %q: %d, want 421", h, got)
		}
	}
	for _, h := range []string{"127.0.0.1:7950", "localhost:7950", "LOCALHOST", "[::1]:7950", "127.0.0.2"} {
		if got := rawRequest(t, "GET", c.Base+"/status", h, "", ""); got != http.StatusOK {
			t.Errorf("Host %q: %d, want 200", h, got)
		}
	}
}

func TestEnsureBadRequestsAndErrors(t *testing.T) {
	fb := &fakeBackend{states: map[string]node.ModelStatus{}, errs: map[string]error{"io": errors.New("boom")}}
	srv := httptest.NewServer(Handler(fb))
	t.Cleanup(srv.Close)
	do := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for name, body := range map[string]string{
		"empty object": `{}`,
		"empty id":     `{"id":""}`,
		"not json":     `not json`,
		"oversized":    `{"id":"` + strings.Repeat("x", 1<<16) + `"}`,
	} {
		if code, _ := do("POST", "/ensure", body); code != http.StatusBadRequest {
			t.Errorf("ensure %s: %d, want 400", name, code)
		}
	}
	if code, body := do("POST", "/ensure", `{"id":"io"}`); code != http.StatusInternalServerError || !strings.Contains(body, "boom") {
		t.Errorf("backend error: %d %s", code, body)
	}
	if code, _ := do("PUT", "/limits/override", `{"max_conns":-1}`); code != http.StatusBadRequest {
		t.Errorf("negative max_conns: %d", code)
	}
	if code, _ := do("PUT", "/limits/override", `{"upload":"fast"}`); code != http.StatusBadRequest {
		t.Errorf("bad rate: %d", code)
	}
	if fb.override != nil {
		t.Error("a rejected override was applied")
	}
}

func TestLoopbackHost(t *testing.T) {
	for h, want := range map[string]bool{
		"127.0.0.1:7950": true, "localhost:7950": true, "LOCALHOST": true, "[::1]:7950": true,
		"[::ffff:127.0.0.1]:7950": true, "127.8.9.10": true,
		"example.com:7950": false, "10.0.0.1:7950": false, "[::ffff:10.0.0.1]:1": false, "localhost.evil.com": false, "": false,
	} {
		if got := loopbackHost(h); got != want {
			t.Errorf("loopbackHost(%q) = %v", h, got)
		}
	}
}
