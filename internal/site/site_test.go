package site

import (
	"bytes"
	"fmt"
	"html"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork-parrot/internal/catalog"
)

func realCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	models, err := catalog.LoadYAML("../../catalog.yaml")
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("catalog.yaml has no models; is the repo layout as expected?")
	}
	data, err := catalog.Build(models, time.Now())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c, err := catalog.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func render(t *testing.T, c *catalog.Catalog, opts Options) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, c, opts); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

func TestRenderRealCatalog(t *testing.T) {
	c := realCatalog(t)
	out := render(t, c, Options{})

	for _, m := range c.Models {
		if !strings.Contains(out, m.ID) {
			t.Errorf("output missing model id %q", m.ID)
		}
		if !strings.Contains(out, m.License) {
			t.Errorf("output missing license %q for model %q", m.License, m.ID)
		}
		for _, f := range m.Files {
			// html/template HTML-escapes attribute values (e.g. "&" ->
			// "&amp;"), which is correct output, so compare against the
			// escaped form.
			if !strings.Contains(out, html.EscapeString(f.Magnet)) {
				t.Errorf("output missing magnet href for file %q of model %q", f.Name, m.ID)
			}
			if !strings.Contains(out, `href="magnet:?xt=urn:btih:`+f.InfoHash) {
				t.Errorf("magnet href for file %q of model %q was not rendered as a link", f.Name, m.ID)
			}
			torrentHref := "torrents/" + f.InfoHash + ".torrent"
			if !strings.Contains(out, torrentHref) {
				t.Errorf("output missing torrent href %q", torrentHref)
			}
		}
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	c := &catalog.Catalog{
		Version:   1,
		Generated: time.Now().UTC(),
		Models: []catalog.Model{
			{
				ID:       "evil<script>alert(1)</script>",
				HFRepo:   "owner/name",
				Revision: "0123456789abcdef0123456789abcdef01234567",
				License:  "<script>bad</script>",
				Files: []catalog.File{
					{
						Name:     "f.gguf",
						Size:     123,
						SHA256:   strings.Repeat("a", 64),
						InfoHash: strings.Repeat("b", 40),
						Magnet:   "magnet:?xt=urn:btih:" + strings.Repeat("b", 40) + "&dn=f.gguf&ws=https%3A%2F%2Fexample.com%2Ff.gguf",
					},
				},
			},
		},
	}
	out := render(t, c, Options{})
	if strings.Contains(out, "<script>") {
		t.Fatalf("output contains an unescaped <script> tag:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Fatalf("expected escaped script tag in output")
	}
}

func TestRenderNoSanitizedLinks(t *testing.T) {
	if out := render(t, realCatalog(t), Options{}); strings.Contains(out, "ZgotmplZ") {
		t.Fatal("html/template replaced a link it considered unsafe (magnet: hrefs need template.URL)")
	}
}

func TestRenderNoLeaks(t *testing.T) {
	c := realCatalog(t)
	out := render(t, c, Options{})
	// Generic private-topology markers. Host-specific names are deliberately
	// not listed here: this file is published.
	forbidden := []string{
		"100.", "192.168.", "10.0.", "172.16.",
		"/home/", "/mnt/", "/srv/", ".local", "tailnet",
	}
	for _, f := range forbidden {
		if strings.Contains(out, f) {
			t.Errorf("output leaks forbidden substring %q", f)
		}
	}
}

func TestRenderNoScriptAndOnlyAllowedLinks(t *testing.T) {
	c := realCatalog(t)
	opts := Options{RepoURL: "https://github.com/janit/viiwork-parrot"}
	out := render(t, c, opts)

	if strings.Contains(strings.ToLower(out), "<script") {
		t.Fatal("output contains a <script tag")
	}

	allowedPrefixes := []string{
		"https://huggingface.co/",
		"https://pirateface.co/",
		"https://github.com/janit/viiwork-parrot",
		"magnet:?",
	}
	for _, tok := range []string{"href=\"http://", "href=\"https://"} {
		idx := 0
		for {
			i := strings.Index(out[idx:], tok)
			if i < 0 {
				break
			}
			start := idx + i + len(`href="`)
			rest := out[start:]
			end := strings.IndexByte(rest, '"')
			if end < 0 {
				t.Fatalf("unterminated href near %q", rest[:min(40, len(rest))])
			}
			url := rest[:end]
			ok := false
			for _, p := range allowedPrefixes {
				if strings.HasPrefix(url, p) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("disallowed absolute link: %q", url)
			}
			idx = start + end
		}
	}
}

func TestSiteHost(t *testing.T) {
	cases := []struct {
		trackers []string
		want     string
	}{
		{nil, defaultSiteHost},
		{[]string{"udp://t.256.fi:6969/announce"}, defaultSiteHost},
		{[]string{"udp://t.256.fi:6969/announce", "https://parrot.lnx.fi/announce"}, "parrot.lnx.fi"},
	}
	for _, tc := range cases {
		if got := siteHost(tc.trackers); got != tc.want {
			t.Errorf("siteHost(%v) = %q, want %q", tc.trackers, got, tc.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestRenderPageStructure(t *testing.T) {
	c := realCatalog(t)
	out := render(t, c, Options{})

	if !strings.HasPrefix(out, "<!doctype html>") {
		t.Fatalf("output does not start with <!doctype html>: %q", out[:min(40, len(out))])
	}
	if !strings.Contains(out, "<title>") {
		t.Error("output missing <title>")
	}
	if !strings.Contains(out, "<html lang=") {
		t.Error("output missing lang attribute on <html>")
	}
	if !strings.Contains(out, `name="viewport"`) {
		t.Error("output missing viewport meta tag")
	}
}

func TestRenderSummaryTotals(t *testing.T) {
	c := realCatalog(t)
	out := render(t, c, Options{})

	var wantFiles int
	var wantSize int64
	for _, m := range c.Models {
		wantFiles += len(m.Files)
		wantSize += m.TotalSize()
	}
	if !strings.Contains(out, fmt.Sprintf(">%d<", len(c.Models))) {
		t.Errorf("output missing model count %d in summary", len(c.Models))
	}
	if !strings.Contains(out, fmt.Sprintf(">%d<", wantFiles)) {
		t.Errorf("output missing total file count %d in summary", wantFiles)
	}
	if !strings.Contains(out, humanSize(wantSize)) {
		t.Errorf("output missing total size %q in summary", humanSize(wantSize))
	}
	if !strings.Contains(out, "ed25519") {
		t.Error("output missing \"Signed: ed25519\" in summary")
	}
}

func TestRenderDefaultsAndPubKey(t *testing.T) {
	c := realCatalog(t)
	out := render(t, c, Options{PubKey: "TESTPUBKEY=="})
	if !strings.Contains(out, "TESTPUBKEY==") {
		t.Error("output missing configured pubkey")
	}
	if !strings.Contains(out, "github.com/janit/viiwork-parrot") {
		t.Error("output missing default repo URL")
	}
}
