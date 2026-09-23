// Package catalog is the signed list of models viiwork-parrot distributes.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"gopkg.in/yaml.v3"
)

// FormatVersion is the catalog format of a catalog with only per-file
// models: exactly what v0.1.0 nodes read. FormatVersionDir is the format of
// a catalog that contains at least one folder model (layout: dir); Build
// only emits it then, so a catalog without folder models stays readable by
// v0.1.0 nodes. A v0.1.0 node rejects a version-2 catalog outright
// ("unsupported version 2") and keeps using its cached one, which is
// explicit, instead of failing validation on a model shape it can't know.
const (
	FormatVersion    = 1
	FormatVersionDir = 2
)

// LayoutDir marks a folder model: one multi-file torrent covering all its
// files, stored on disk as data_dir/<model id>/<hf path>.
const LayoutDir = "dir"

// DirWebSeed is the BEP-19 web-seed base URL of a folder model's torrent.
// It ends in "/", so a client appends <info.name>/<file path>, and the
// torrent's info.name is the HF revision: the request is
// https://huggingface.co/<repo>/resolve/<revision>/<path>.
func DirWebSeed(repo string) string {
	return "https://huggingface.co/" + repo + "/resolve/"
}

type File struct {
	Name    string `yaml:"name" json:"name"`
	StoreAs string `yaml:"store_as,omitempty" json:"store_as,omitempty"`
	Size    int64  `yaml:"size" json:"size"`
	SHA256  string `yaml:"sha256" json:"sha256"`
	// InfoHash and Magnet are per file for per-file models only; a folder
	// model's files have neither (the model has one torrent).
	InfoHash string `yaml:"infohash,omitempty" json:"infohash,omitempty"`
	Magnet   string `yaml:"magnet,omitempty" json:"magnet,omitempty"`
}

// DiskName is the file name nodes store a fresh download as.
func (f File) DiskName() string {
	if f.StoreAs != "" {
		return f.StoreAs
	}
	return f.Name[strings.LastIndex(f.Name, "/")+1:]
}

// MagnetSources parses the file's (signed) magnet and returns its trackers
// ("tr") and web-seeds ("ws"). Nodes take their network sources from here,
// never from the .torrent's own announce/url-list, which is not covered by
// the catalog signature (only its info dict is, via the infohash). It fails
// if the magnet's btih is not the file's infohash.
func (f File) MagnetSources() (trackers, webSeeds []string, err error) {
	return magnetSources(f.Magnet, f.InfoHash)
}

func magnetSources(magnet, infohash string) (trackers, webSeeds []string, err error) {
	m, err := metainfo.ParseMagnetUri(magnet)
	if err != nil {
		return nil, nil, fmt.Errorf("magnet: %w", err)
	}
	if got := m.InfoHash.HexString(); got != infohash {
		return nil, nil, fmt.Errorf("magnet btih %s != infohash %s", got, infohash)
	}
	return m.Trackers, m.Params["ws"], nil
}

type Model struct {
	ID       string `yaml:"id" json:"id"`
	HFRepo   string `yaml:"hf_repo" json:"hf_repo"`
	Revision string `yaml:"revision" json:"revision"`
	License  string `yaml:"license" json:"license"`
	// LicenseURL points at the license text when it is not a file of the
	// repo (optional; shown on the landing page).
	LicenseURL string `yaml:"license_url,omitempty" json:"license_url,omitempty"`
	// Layout is "" (per-file: one single-file torrent per file, stored flat
	// in data_dir) or LayoutDir.
	Layout string `yaml:"layout,omitempty" json:"layout,omitempty"`
	// InfoHash and Magnet are the folder model's one torrent (LayoutDir only).
	InfoHash string `yaml:"infohash,omitempty" json:"infohash,omitempty"`
	Magnet   string `yaml:"magnet,omitempty" json:"magnet,omitempty"`
	Files    []File `yaml:"files" json:"files"`
}

// IsDir reports whether m is a folder model (layout: dir).
func (m Model) IsDir() bool { return m.Layout == LayoutDir }

// TotalSize is the sum of m's file sizes.
func (m Model) TotalSize() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// MagnetSources is File.MagnetSources for a folder model's one torrent.
func (m Model) MagnetSources() (trackers, webSeeds []string, err error) {
	return magnetSources(m.Magnet, m.InfoHash)
}

// MainFile is what /ensure returns as the model path: the first .gguf in
// catalog order (llama.cpp finds sibling shards itself), else the first file.
func (m Model) MainFile() File {
	for _, f := range m.Files {
		if strings.HasSuffix(f.Name, ".gguf") {
			return f
		}
	}
	return m.Files[0]
}

type Catalog struct {
	Version   int       `json:"version"`
	Generated time.Time `json:"generated"`
	Models    []Model   `json:"models"`
}

func (c *Catalog) Model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	hex40  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	hex64  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	diskRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
)

func Validate(models []Model) error {
	ids := map[string]bool{}
	disk := map[string]string{}
	hashes := map[string]bool{}
	contents := map[string]string{}    // sha256 -> model id of a per-file model's file: one content, one entry
	dirContents := map[string]string{} // sha256 -> model id of a folder model's (non-empty) file
	for i, m := range models {
		key := fmt.Sprintf("models[%d] (%s)", i, m.ID)
		switch {
		case !idRe.MatchString(m.ID):
			return fmt.Errorf("%s: id must match %s", key, idRe)
		case ids[m.ID]:
			return fmt.Errorf("%s: duplicate id", key)
		case m.HFRepo == "" || !strings.Contains(m.HFRepo, "/"):
			return fmt.Errorf("%s: hf_repo must be owner/name", key)
		case !hex40.MatchString(m.Revision):
			return fmt.Errorf("%s: revision must be a 40-char commit sha, got %q", key, m.Revision)
		case m.License == "":
			return fmt.Errorf("%s: license required", key)
		case len(m.Files) == 0:
			return fmt.Errorf("%s: no files", key)
		}
		ids[m.ID] = true
		if m.LicenseURL != "" {
			if u, err := url.Parse(m.LicenseURL); err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("%s: license_url %q must be an https URL", key, m.LicenseURL)
			}
		}
		switch m.Layout {
		case LayoutDir:
			if err := validateDir(key, m, disk, hashes, contents, dirContents); err != nil {
				return err
			}
			continue
		case "":
		default:
			return fmt.Errorf("%s: unknown layout %q (want %q or none)", key, m.Layout, LayoutDir)
		}
		if m.InfoHash != "" || m.Magnet != "" {
			return fmt.Errorf("%s: model-level infohash/magnet are only for layout: %s", key, LayoutDir)
		}
		for j, f := range m.Files {
			fk := fmt.Sprintf("%s.files[%d] (%s)", key, j, f.Name)
			switch {
			case f.Name == "" || strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, ".."):
				return fmt.Errorf("%s: bad name", fk)
			case !diskRe.MatchString(f.DiskName()):
				return fmt.Errorf("%s: disk name %q must match %s", fk, f.DiskName(), diskRe)
			case f.Size <= 0:
				return fmt.Errorf("%s: size must be > 0", fk)
			case !hex64.MatchString(f.SHA256):
				return fmt.Errorf("%s: sha256 must be 64 lowercase hex", fk)
			case !hex40.MatchString(f.InfoHash):
				return fmt.Errorf("%s: infohash must be 40 lowercase hex", fk)
			case !strings.HasPrefix(f.Magnet, "magnet:?"):
				return fmt.Errorf("%s: magnet must start with magnet:?", fk)
			case hashes[f.InfoHash]:
				return fmt.Errorf("%s: duplicate infohash", fk)
			}
			_, ws, err := f.MagnetSources()
			if err != nil {
				return fmt.Errorf("%s: %v", fk, err)
			}
			if len(ws) != 1 {
				return fmt.Errorf("%s: magnet must carry exactly one ws (web-seed), has %d", fk, len(ws))
			}
			if want := DirWebSeed(m.HFRepo) + m.Revision + "/" + f.Name; ws[0] != want {
				return fmt.Errorf("%s: magnet ws %q must be %q", fk, ws[0], want)
			}
			if other, dup := disk[f.DiskName()]; dup {
				return fmt.Errorf("%s: disk name %q already used by %s; set store_as", fk, f.DiskName(), other)
			}
			if other, dup := contents[f.SHA256]; dup {
				return fmt.Errorf("%s: same content (sha256) as a file of %s; a host would store it twice", fk, other)
			}
			if other, dup := dirContents[f.SHA256]; dup {
				return fmt.Errorf("%s: same content (sha256) as a file of folder model %s; a host would store it twice", fk, other)
			}
			contents[f.SHA256] = m.ID
			disk[f.DiskName()] = m.ID
			hashes[f.InfoHash] = true
		}
	}
	return nil
}

// validateDir checks a folder model (layout: dir): one torrent for the
// whole model, file names are relative HF paths (no "..", no absolute or
// unclean paths, no duplicates, no file that is also another's parent
// directory), and the model's directory name (its id) must not collide with
// any per-file disk name. The magnet's one web-seed must be exactly
// DirWebSeed(hf_repo): BEP-19 appends <info.name>/<path>, and info.name is
// the revision. A folder file must not duplicate a per-file model's file
// (that content would be stored twice for no reason), but folders may share
// content with each other and within themselves: a folder needs every path
// (shared tokenizer.json, vocab and merges files across quantizations of one
// model are normal), and all empty files hash alike.
func validateDir(key string, m Model, disk map[string]string, hashes map[string]bool, contents, dirContents map[string]string) error {
	switch {
	case !hex40.MatchString(m.InfoHash):
		return fmt.Errorf("%s: infohash must be 40 lowercase hex", key)
	case !strings.HasPrefix(m.Magnet, "magnet:?"):
		return fmt.Errorf("%s: magnet must start with magnet:?", key)
	case hashes[m.InfoHash]:
		return fmt.Errorf("%s: duplicate infohash", key)
	case !diskRe.MatchString(m.ID):
		return fmt.Errorf("%s: directory name %q must match %s", key, m.ID, diskRe)
	}
	_, ws, err := m.MagnetSources()
	if err != nil {
		return fmt.Errorf("%s: %v", key, err)
	}
	if len(ws) != 1 {
		return fmt.Errorf("%s: magnet must carry exactly one ws (web-seed), has %d", key, len(ws))
	}
	if want := DirWebSeed(m.HFRepo); ws[0] != want {
		return fmt.Errorf("%s: magnet ws %q must be %q", key, ws[0], want)
	}
	if other, dup := disk[m.ID]; dup {
		return fmt.Errorf("%s: directory name %q already used as a disk name by %s", key, m.ID, other)
	}
	names := map[string]bool{}
	for _, f := range m.Files {
		names[f.Name] = true
	}
	for j, f := range m.Files {
		fk := fmt.Sprintf("%s.files[%d] (%s)", key, j, f.Name)
		switch {
		case !validRelPath(f.Name):
			return fmt.Errorf("%s: name must be a clean relative path (no \"..\", \".\", empty or absolute parts)", fk)
		case f.StoreAs != "" || f.InfoHash != "" || f.Magnet != "":
			return fmt.Errorf("%s: store_as/infohash/magnet are per-file fields; a %s model has one model-level torrent", fk, LayoutDir)
		case f.Size < 0:
			return fmt.Errorf("%s: size must be >= 0", fk)
		case !hex64.MatchString(f.SHA256):
			return fmt.Errorf("%s: sha256 must be 64 lowercase hex", fk)
		}
		for i := strings.IndexByte(f.Name, '/'); i >= 0; i = nextSlash(f.Name, i) {
			if names[f.Name[:i]] {
				return fmt.Errorf("%s: %s is both a file and a directory", fk, f.Name[:i])
			}
		}
		if f.Size == 0 {
			continue // every empty file has the same sha256; nothing is stored twice
		}
		if other, dup := contents[f.SHA256]; dup {
			return fmt.Errorf("%s: same content (sha256) as a file of %s; a host would store it twice", fk, other)
		}
		dirContents[f.SHA256] = m.ID
	}
	if len(names) != len(m.Files) {
		return fmt.Errorf("%s: duplicate file name", key)
	}
	disk[m.ID] = m.ID
	hashes[m.InfoHash] = true
	return nil
}

func nextSlash(s string, i int) int {
	j := strings.IndexByte(s[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// validRelPath: a non-empty, clean, relative slash path with no ".", ".."
// or empty components, backslashes or control characters.
func validRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." {
			return false
		}
	}
	return true
}

// Version is the catalog format version Build writes for models:
// FormatVersionDir if any is a folder model, else FormatVersion.
func Version(models []Model) int {
	for _, m := range models {
		if m.IsDir() {
			return FormatVersionDir
		}
	}
	return FormatVersion
}

func Build(models []Model, now time.Time) ([]byte, error) {
	if err := Validate(models); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(Catalog{Version: Version(models), Generated: now.UTC(), Models: models}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func Parse(data []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if c.Version != FormatVersion && c.Version != FormatVersionDir {
		return nil, fmt.Errorf("catalog: unsupported version %d (this build reads %d and %d); upgrade viiwork-parrot", c.Version, FormatVersion, FormatVersionDir)
	}
	if err := Validate(c.Models); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if c.Version < Version(c.Models) {
		return nil, fmt.Errorf("catalog: version %d cannot carry %s models (needs %d)", c.Version, LayoutDir, FormatVersionDir)
	}
	return &c, nil
}

func LoadYAML(path string) ([]Model, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var models []Model
	if err := yaml.Unmarshal(data, &models); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return models, nil
}

func SaveYAML(path string, models []Model) error {
	data, err := yaml.Marshal(models)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
