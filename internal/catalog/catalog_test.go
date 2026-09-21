package catalog

import (
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func magnet(ih, ws string) string {
	m := "magnet:?xt=urn:btih:" + ih + "&tr=udp%3A%2F%2Ft.256.fi%3A6969%2Fannounce"
	if ws != "" {
		m += "&ws=" + url.QueryEscape(ws)
	}
	return m
}

func sampleModels() []Model {
	return []Model{{
		ID: "gemma4-31b-qat-q4kxl", HFRepo: "unsloth/gemma-4-31B-it-qat-GGUF",
		Revision: strings.Repeat("a", 40), License: "gemma",
		Files: []File{
			{Name: "LICENSE", StoreAs: "gemma4-31b-qat-q4kxl.LICENSE", Size: 10, SHA256: strings.Repeat("1", 64), InfoHash: strings.Repeat("b", 40), Magnet: magnet(strings.Repeat("b", 40), "https://huggingface.co/o/r/resolve/x/LICENSE")},
			{Name: "gemma-4-31B-it-qat-UD-Q4_K_XL.gguf", Size: 100, SHA256: strings.Repeat("2", 64), InfoHash: strings.Repeat("c", 40), Magnet: magnet(strings.Repeat("c", 40), "https://huggingface.co/o/r/resolve/x/m.gguf")},
		},
	}}
}

func TestBuildParseRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	data, err := Build(sampleModels(), now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := c.Model("gemma4-31b-qat-q4kxl")
	if !ok || c.Version != FormatVersion || !c.Generated.Equal(now) {
		t.Fatalf("%+v", c)
	}
	if mf := m.MainFile(); mf.Name != "gemma-4-31B-it-qat-UD-Q4_K_XL.gguf" {
		t.Fatalf("main file should be the first .gguf, got %s", mf.Name)
	}
	if m.Files[0].DiskName() != "gemma4-31b-qat-q4kxl.LICENSE" || m.Files[1].DiskName() != m.Files[1].Name {
		t.Fatal("DiskName")
	}
}

func TestValidate(t *testing.T) {
	mut := map[string]func(m []Model) []Model{
		"id":       func(m []Model) []Model { m[0].ID = "Bad ID"; return m },
		"revision": func(m []Model) []Model { m[0].Revision = "main"; return m },
		"sha256":   func(m []Model) []Model { m[0].Files[0].SHA256 = "xyz"; return m },
		"infohash": func(m []Model) []Model { m[0].Files[0].InfoHash = "short"; return m },
		"magnet":   func(m []Model) []Model { m[0].Files[0].Magnet = "http://x"; return m },
		"magnet btih mismatch": func(m []Model) []Model {
			m[0].Files[0].Magnet = magnet(strings.Repeat("f", 40), "https://huggingface.co/o/r/resolve/x/LICENSE")
			return m
		},
		"magnet without ws": func(m []Model) []Model { m[0].Files[0].Magnet = magnet(strings.Repeat("b", 40), ""); return m },
		"magnet two ws": func(m []Model) []Model {
			m[0].Files[0].Magnet += "&ws=" + url.QueryEscape("https://evil.example/x")
			return m
		},
		"magnet http ws": func(m []Model) []Model {
			m[0].Files[0].Magnet = magnet(strings.Repeat("b", 40), "http://huggingface.co/o/r/resolve/x/LICENSE")
			return m
		},
		"size":         func(m []Model) []Model { m[0].Files[0].Size = 0; return m },
		"no files":     func(m []Model) []Model { m[0].Files = nil; return m },
		"disk name":    func(m []Model) []Model { m[0].Files[0].StoreAs = "../escape"; return m },
		"duplicate id": func(m []Model) []Model { return append(m, m[0]) },
		"duplicate sha256": func(m []Model) []Model {
			m2 := sampleModels()[0]
			m2.ID = "copy"
			m2.Files = []File{{Name: "copy.gguf", Size: 100, SHA256: m[0].Files[1].SHA256, InfoHash: strings.Repeat("e", 40), Magnet: magnet(strings.Repeat("e", 40), "https://huggingface.co/o/r/resolve/x/copy.gguf")}}
			return append(m, m2)
		},
		"duplicate disk name": func(m []Model) []Model {
			m2 := sampleModels()[0]
			m2.ID = "other"
			m2.Files = m2.Files[1:]
			m2.Files[0].InfoHash = strings.Repeat("d", 40)
			return append(m, m2)
		},
	}
	for name, f := range mut {
		if err := Validate(f(sampleModels())); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	if err := Validate(sampleModels()); err != nil {
		t.Fatal(err)
	}
}

func TestYAMLRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "catalog.yaml")
	if m, err := LoadYAML(p); err != nil || m != nil {
		t.Fatalf("missing file: %v %v", m, err)
	}
	if err := SaveYAML(p, sampleModels()); err != nil {
		t.Fatal(err)
	}
	m, err := LoadYAML(p)
	if err != nil || len(m) != 1 || m[0].Files[0].StoreAs != "gemma4-31b-qat-q4kxl.LICENSE" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestMagnetSources(t *testing.T) {
	f := sampleModels()[0].Files[1]
	tr, ws, err := f.MagnetSources()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tr, []string{"udp://t.256.fi:6969/announce"}) || !reflect.DeepEqual(ws, []string{"https://huggingface.co/o/r/resolve/x/m.gguf"}) {
		t.Fatalf("trackers %v web-seeds %v", tr, ws)
	}
	f.InfoHash = strings.Repeat("d", 40)
	if _, _, err := f.MagnetSources(); err == nil {
		t.Fatal("magnet btih != infohash must be an error")
	}
}

// TestRepoCatalogValidates: the committed catalog.yaml passes Validate
// (including the magnet checks), so it can still be built and signed.
func TestRepoCatalogValidates(t *testing.T) {
	m, err := LoadYAML("../../catalog.yaml")
	if err != nil || len(m) == 0 {
		t.Fatalf("%v %d", err, len(m))
	}
	if err := Validate(m); err != nil {
		t.Fatal(err)
	}
}
