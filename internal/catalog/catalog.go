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

const FormatVersion = 1

type File struct {
	Name     string `yaml:"name" json:"name"`
	StoreAs  string `yaml:"store_as,omitempty" json:"store_as,omitempty"`
	Size     int64  `yaml:"size" json:"size"`
	SHA256   string `yaml:"sha256" json:"sha256"`
	InfoHash string `yaml:"infohash" json:"infohash"`
	Magnet   string `yaml:"magnet" json:"magnet"`
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
	m, err := metainfo.ParseMagnetUri(f.Magnet)
	if err != nil {
		return nil, nil, fmt.Errorf("magnet: %w", err)
	}
	if got := m.InfoHash.HexString(); got != f.InfoHash {
		return nil, nil, fmt.Errorf("magnet btih %s != infohash %s", got, f.InfoHash)
	}
	return m.Trackers, m.Params["ws"], nil
}

type Model struct {
	ID       string `yaml:"id" json:"id"`
	HFRepo   string `yaml:"hf_repo" json:"hf_repo"`
	Revision string `yaml:"revision" json:"revision"`
	License  string `yaml:"license" json:"license"`
	Files    []File `yaml:"files" json:"files"`
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
	contents := map[string]string{} // sha256 -> model id: one file content, one entry
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
			if u, err := url.Parse(ws[0]); err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("%s: magnet ws %q must be an https URL", fk, ws[0])
			}
			if other, dup := disk[f.DiskName()]; dup {
				return fmt.Errorf("%s: disk name %q already used by %s; set store_as", fk, f.DiskName(), other)
			}
			if other, dup := contents[f.SHA256]; dup {
				return fmt.Errorf("%s: same content (sha256) as a file of %s; a host would store it twice", fk, other)
			}
			contents[f.SHA256] = m.ID
			disk[f.DiskName()] = m.ID
			hashes[f.InfoHash] = true
		}
	}
	return nil
}

func Build(models []Model, now time.Time) ([]byte, error) {
	if err := Validate(models); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(Catalog{Version: FormatVersion, Generated: now.UTC(), Models: models}, "", "  ")
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
	if c.Version != FormatVersion {
		return nil, fmt.Errorf("catalog: unsupported version %d (this build reads %d)", c.Version, FormatVersion)
	}
	if err := Validate(c.Models); err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
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
