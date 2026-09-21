package site

import (
	"fmt"
	"html"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork-parrot/internal/catalog"
)

// dirCatalog: the real catalog plus a folder model the size of a big
// safetensors repo (418 files).
func dirCatalog(t *testing.T) (*catalog.Catalog, catalog.Model) {
	t.Helper()
	models, err := catalog.LoadYAML("../../catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ih := strings.Repeat("9", 40)
	repo := "example-org/Big-Model-NVFP4"
	m := catalog.Model{
		ID: "big-model-nvfp4", HFRepo: repo, Revision: strings.Repeat("d", 40), License: "other",
		LicenseURL: "https://huggingface.co/" + repo + "/blob/main/LICENSE.txt", Layout: catalog.LayoutDir, InfoHash: ih,
		Magnet: "magnet:?xt=urn:btih:" + ih + "&dn=big-model-nvfp4&ws=" + url.QueryEscape(catalog.DirWebSeed(repo)),
	}
	for i := 0; i < 418; i++ {
		m.Files = append(m.Files, catalog.File{Name: fmt.Sprintf("model-%05d-of-00418.safetensors", i+1), Size: 300 << 20, SHA256: fmt.Sprintf("%064x", i+1)})
	}
	m.Files[0].Name = "configs/sub dir/config.json"
	data, err := catalog.Build(append(models, m), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return c, m
}

func TestRenderDirModel(t *testing.T) {
	c, m := dirCatalog(t)
	out := render(t, c, Options{})
	i := strings.Index(out, "<h3>"+m.ID+"</h3>")
	if i < 0 {
		t.Fatal("folder model missing")
	}
	article := out[i:]
	article = article[:strings.Index(article, "</article>")]
	for _, want := range []string{
		html.EscapeString(m.Magnet),
		`href="magnet:?xt=urn:btih:` + m.InfoHash,
		"torrents/" + m.InfoHash + ".torrent",
		"418 files",
		"122.5 GiB", // human total
		`href="` + m.LicenseURL + `"`,
		"<code>configs/sub dir/config.json</code>",
		`<details class="filelist">`, // collapsed: no "open" attribute
		m.Revision[:7],
	} {
		if !strings.Contains(article, want) {
			t.Errorf("folder model section missing %q", want)
		}
	}
	if n := strings.Count(article, "magnet:?"); n != 2 { // the link and its copy box
		t.Errorf("folder model shows %d magnets, want one torrent (2 occurrences)", n)
	}
	if strings.Contains(article, "<details class=\"filelist\" open") {
		t.Error("file list must be collapsed")
	}
	// Light for a 418-file model: well under 100 bytes of HTML per file.
	if len(article) > 418*260 {
		t.Errorf("folder model section is %d bytes", len(article))
	}
}
