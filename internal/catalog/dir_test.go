package catalog

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

func dirMagnet(ih, repo string) string {
	return "magnet:?xt=urn:btih:" + ih + "&tr=udp%3A%2F%2Ft.256.fi%3A6969%2Fannounce&ws=" + url.QueryEscape(DirWebSeed(repo))
}

func sampleDirModel() Model {
	ih := strings.Repeat("9", 40)
	return Model{
		ID: "deepseek-v4-flash", HFRepo: "deepseek-ai/DeepSeek-V4-Flash-0731", Revision: strings.Repeat("d", 40),
		License: "deepseek", LicenseURL: "https://example.org/license", Layout: LayoutDir,
		InfoHash: ih, Magnet: dirMagnet(ih, "deepseek-ai/DeepSeek-V4-Flash-0731"),
		Files: []File{
			{Name: "config.json", Size: 10, SHA256: strings.Repeat("3", 64)},
			{Name: "model-00001-of-00002.safetensors", Size: 100, SHA256: strings.Repeat("4", 64)},
			{Name: "inference/model.py", Size: 20, SHA256: strings.Repeat("5", 64)},
			{Name: "inference/empty.txt", Size: 0, SHA256: strings.Repeat("6", 64)},
			{Name: "copy-of-config.json", Size: 10, SHA256: strings.Repeat("3", 64)}, // same content within one folder is fine
		},
	}
}

func TestDirModelValidates(t *testing.T) {
	models := append(sampleModels(), sampleDirModel())
	if err := Validate(models); err != nil {
		t.Fatal(err)
	}
	m := models[1]
	if !m.IsDir() || m.TotalSize() != 140 {
		t.Fatalf("IsDir %v TotalSize %d", m.IsDir(), m.TotalSize())
	}
	tr, ws, err := m.MagnetSources()
	if err != nil || len(tr) != 1 || len(ws) != 1 || ws[0] != "https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash-0731/resolve/" {
		t.Fatalf("%v %v %v", tr, ws, err)
	}
}

func TestDirModelValidateRejects(t *testing.T) {
	mut := map[string]func(m []Model) []Model{
		"relative ..":       func(m []Model) []Model { m[1].Files[0].Name = "../x"; return m },
		"inner ..":          func(m []Model) []Model { m[1].Files[0].Name = "a/../x"; return m },
		"absolute":          func(m []Model) []Model { m[1].Files[0].Name = "/etc/passwd"; return m },
		"dot component":     func(m []Model) []Model { m[1].Files[0].Name = "./config.json"; return m },
		"empty component":   func(m []Model) []Model { m[1].Files[0].Name = "a//b"; return m },
		"trailing slash":    func(m []Model) []Model { m[1].Files[0].Name = "a/"; return m },
		"backslash":         func(m []Model) []Model { m[1].Files[0].Name = `a\b`; return m },
		"duplicate path":    func(m []Model) []Model { m[1].Files[1].Name = m[1].Files[0].Name; return m },
		"file is a dir":     func(m []Model) []Model { m[1].Files[0].Name = "inference"; return m },
		"per-file infohash": func(m []Model) []Model { m[1].Files[0].InfoHash = strings.Repeat("8", 40); return m },
		"per-file magnet":   func(m []Model) []Model { m[1].Files[0].Magnet = m[1].Magnet; return m },
		"store_as":          func(m []Model) []Model { m[1].Files[0].StoreAs = "x"; return m },
		"no infohash":       func(m []Model) []Model { m[1].InfoHash = ""; return m },
		"btih mismatch": func(m []Model) []Model {
			m[1].Magnet = dirMagnet(strings.Repeat("7", 40), m[1].HFRepo)
			return m
		},
		"ws without trailing slash": func(m []Model) []Model {
			m[1].Magnet = strings.TrimSuffix(m[1].Magnet, "%2F")
			return m
		},
		"ws other repo": func(m []Model) []Model { m[1].Magnet = dirMagnet(m[1].InfoHash, "evil/repo"); return m },
		"two ws": func(m []Model) []Model {
			m[1].Magnet += "&ws=" + url.QueryEscape(DirWebSeed(m[1].HFRepo))
			return m
		},
		"negative size": func(m []Model) []Model { m[1].Files[0].Size = -1; return m },
		"bad sha256":    func(m []Model) []Model { m[1].Files[0].SHA256 = "x"; return m },
		"sha256 shared with another model": func(m []Model) []Model {
			m[1].Files[0].SHA256 = m[0].Files[1].SHA256
			return m
		},
		"dir name collides with a disk name": func(m []Model) []Model { m[1].ID = "m.gguf"; m[0].Files[1].StoreAs = "m.gguf"; return m },
		"disk name collides with a dir name": func(m []Model) []Model {
			m[0], m[1] = m[1], m[0]
			m[0].ID = "x.gguf"
			m[1].Files[1].StoreAs = "x.gguf"
			return m
		},
		"infohash shared with a file": func(m []Model) []Model {
			m[1].InfoHash = m[0].Files[0].InfoHash
			m[1].Magnet = dirMagnet(m[1].InfoHash, m[1].HFRepo)
			return m
		},
		"unknown layout":         func(m []Model) []Model { m[1].Layout = "folder"; return m },
		"per-file with infohash": func(m []Model) []Model { m[0].InfoHash = strings.Repeat("8", 40); return m },
		"license_url http":       func(m []Model) []Model { m[1].LicenseURL = "http://example.org/l"; return m },
		"no files":               func(m []Model) []Model { m[1].Files = nil; return m },
	}
	for name, f := range mut {
		if err := Validate(f(append(sampleModels(), sampleDirModel()))); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestBuildVersion(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	// Per-file only: still version 1, and byte-for-byte free of the new
	// fields, so v0.1.0 nodes keep reading it.
	data, err := Build(sampleModels(), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"layout"`, `"license_url"`, `"version": 2`} {
		if strings.Contains(string(data), k) {
			t.Fatalf("per-file catalog contains %s:\n%s", k, data)
		}
	}
	if c, err := Parse(data); err != nil || c.Version != FormatVersion {
		t.Fatalf("%v %v", c, err)
	}

	data, err = Build(append(sampleModels(), sampleDirModel()), now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(data)
	if err != nil || c.Version != FormatVersionDir {
		t.Fatalf("%v %v", c, err)
	}
	m, ok := c.Model("deepseek-v4-flash")
	if !ok || !m.IsDir() || m.InfoHash == "" || m.LicenseURL == "" || len(m.Files) != 5 || m.Files[0].InfoHash != "" {
		t.Fatalf("%+v", m)
	}
	var raw struct {
		Models []map[string]any `json:"models"`
	}
	json.Unmarshal(data, &raw)
	if _, ok := raw.Models[1]["files"].([]any)[0].(map[string]any)["infohash"]; ok {
		t.Fatal("folder model files must not carry an (empty) infohash key")
	}

	// A version-1 catalog must not carry folder models.
	var cat Catalog
	json.Unmarshal(data, &cat)
	cat.Version = FormatVersion
	v1, _ := json.Marshal(cat)
	if _, err := Parse(v1); err == nil {
		t.Fatal("version 1 with a folder model must be rejected")
	}
	cat.Version = 3
	v3, _ := json.Marshal(cat)
	if _, err := Parse(v3); err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("unknown version: %v", err)
	}
}

func TestDirYAMLRoundTrip(t *testing.T) {
	p := t.TempDir() + "/catalog.yaml"
	models := append(sampleModels(), sampleDirModel())
	if err := SaveYAML(p, models); err != nil {
		t.Fatal(err)
	}
	got, err := LoadYAML(p)
	if err != nil || len(got) != 2 || !got[1].IsDir() || got[1].Files[2].Name != "inference/model.py" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := Validate(got); err != nil {
		t.Fatal(err)
	}
}

// TestDirModelsShareContent: folder models may share files with each other
// (tokenizer.json across quantizations of one model) and every empty file
// hashes alike; a per-file model's file may not duplicate a folder's file,
// in either catalog order.
func TestDirModelsShareContent(t *testing.T) {
	a := sampleDirModel()
	b := sampleDirModel()
	b.ID, b.HFRepo = "deepseek-v4-flash-fp8", "deepseek-ai/DeepSeek-V4-Flash-0731-FP8"
	b.InfoHash = strings.Repeat("8", 40)
	b.Magnet = dirMagnet(b.InfoHash, b.HFRepo)
	b.Files = []File{
		{Name: "tokenizer.json", Size: 10, SHA256: a.Files[0].SHA256}, // shared with a
		{Name: "empty.txt", Size: 0, SHA256: a.Files[3].SHA256},       // empty, like a's
		{Name: "model.safetensors", Size: 100, SHA256: strings.Repeat("7", 64)},
	}
	if err := Validate([]Model{a, b}); err != nil {
		t.Fatalf("folder models sharing content: %v", err)
	}
	if err := Validate(append(sampleModels(), a, b)); err != nil {
		t.Fatal(err)
	}

	perFile := sampleModels()
	perFile[0].Files[1].SHA256 = b.Files[2].SHA256
	if err := Validate(append([]Model{b}, perFile...)); err == nil {
		t.Error("per-file file duplicating a folder file (folder first) must fail")
	}
	if err := Validate(append(perFile, b)); err == nil {
		t.Error("per-file file duplicating a folder file (per-file first) must fail")
	}
}
