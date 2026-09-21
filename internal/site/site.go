// Package site renders the public landing page (index.html) from a signed
// catalog. It reads only the parsed catalog and the given Options — never
// the filesystem, environment, or any internal host configuration — so the
// page can never leak anything about where it was generated.
package site

import (
	_ "embed"
	"fmt"
	"html/template"
	"io"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/mktorrent"
)

//go:embed template.html
var templateFS string

var page = template.Must(template.New("index").Funcs(template.FuncMap{
	"humanSize": humanSize,
}).Parse(templateFS))

// Options configures page rendering. Every field has a public-safe default,
// so a zero Options renders a usable page.
type Options struct {
	// PubKey is the base64 ed25519 public key shown for catalog verification.
	// Defaults to catalog.DefaultPubKey.
	PubKey string
	// Trackers are the announce URLs listed under "Trackers". Defaults to
	// mktorrent.DefaultAnnounce, flattened.
	Trackers []string
	// RepoURL is the public source repository linked from "How to use".
	// Defaults to https://github.com/janit/viiwork-parrot.
	RepoURL string
}

const defaultRepoURL = "https://github.com/janit/viiwork-parrot"

type fileView struct {
	DiskName    string
	Size        string
	Magnet      string
	TorrentHref string
	SHA256      string
	ShortSHA256 string
}

type modelView struct {
	ID            string
	HFRepo        string
	Revision      string
	ShortRevision string
	HFTreeURL     string
	License       string
	TotalSize     string
	Files         []fileView
}

type pageData struct {
	RepoURL   string
	PubKey    string
	Trackers  []string
	Generated string
	Models    []modelView
}

// Render writes the static landing page for c to w, using opts (with
// defaults filled in for any zero field).
func Render(w io.Writer, c *catalog.Catalog, opts Options) error {
	if opts.PubKey == "" {
		opts.PubKey = catalog.DefaultPubKey
	}
	if opts.RepoURL == "" {
		opts.RepoURL = defaultRepoURL
	}
	if len(opts.Trackers) == 0 {
		for _, tier := range mktorrent.DefaultAnnounce {
			opts.Trackers = append(opts.Trackers, tier...)
		}
	}

	data := pageData{
		RepoURL:  opts.RepoURL,
		PubKey:   opts.PubKey,
		Trackers: opts.Trackers,
	}
	if c != nil {
		data.Generated = c.Generated.UTC().Format("2006-01-02 15:04:05 UTC")
		for _, m := range c.Models {
			data.Models = append(data.Models, newModelView(m))
		}
	}
	return page.Execute(w, data)
}

func newModelView(m catalog.Model) modelView {
	var total int64
	files := make([]fileView, 0, len(m.Files))
	for _, f := range m.Files {
		total += f.Size
		files = append(files, fileView{
			DiskName:    f.DiskName(),
			Size:        humanSize(f.Size),
			Magnet:      f.Magnet,
			TorrentHref: "torrents/" + f.InfoHash + ".torrent",
			SHA256:      f.SHA256,
			ShortSHA256: shortHex(f.SHA256, 12),
		})
	}
	return modelView{
		ID:            m.ID,
		HFRepo:        m.HFRepo,
		Revision:      m.Revision,
		ShortRevision: shortHex(m.Revision, 7),
		HFTreeURL:     fmt.Sprintf("https://huggingface.co/%s/tree/%s", m.HFRepo, m.Revision),
		License:       m.License,
		TotalSize:     humanSize(total),
		Files:         files,
	}
}

func shortHex(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// humanSize formats bytes using binary (KiB/MiB/GiB) units, one decimal
// place above 1 KiB.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n2 := n / unit; n2 >= unit; n2 /= unit {
		div *= unit
		exp++
	}
	units := "KMGTPE"
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), units[exp])
}
