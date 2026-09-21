// Package publish turns HF model files that exist locally into catalog
// entries and torrents, refusing anything that does not match HF's hashes.
package publish

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/janit/viiwork-parrot/internal/catalog"
	"github.com/janit/viiwork-parrot/internal/hashcache"
	"github.com/janit/viiwork-parrot/internal/hfapi"
	"github.com/janit/viiwork-parrot/internal/mktorrent"
)

type Options struct {
	ID, Repo, Revision, License string
	LocalDir                    string
	Files                       []string
	StoreAs                     map[string]string
	TorrentsDir                 string
	HF                          *hfapi.Client
	Announce                    [][]string
}

func DiskName(id, hfPath string, storeAs map[string]string) string {
	if s, ok := storeAs[hfPath]; ok {
		return s
	}
	base := path.Base(hfPath)
	if strings.HasSuffix(base, ".gguf") {
		return base
	}
	return id + "." + base
}

func AddModel(ctx context.Context, o Options) (catalog.Model, error) {
	sha, err := o.HF.ResolveRevision(ctx, o.Repo, o.Revision)
	if err != nil {
		return catalog.Model{}, err
	}
	entries, err := o.HF.Tree(ctx, o.Repo, sha)
	if err != nil {
		return catalog.Model{}, err
	}
	byPath := map[string]hfapi.Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	m := catalog.Model{ID: o.ID, HFRepo: o.Repo, Revision: sha, License: o.License}
	for _, hfPath := range o.Files {
		e, ok := byPath[hfPath]
		if !ok || e.Type != "file" {
			return m, fmt.Errorf("%s: not a file in %s@%s", hfPath, o.Repo, sha)
		}
		local := filepath.Join(o.LocalDir, filepath.FromSlash(hfPath))
		fi, err := os.Stat(local)
		if err != nil {
			return m, err
		}
		size := e.Size
		if e.LFS != nil {
			size = e.LFS.Size
		}
		if fi.Size() != size {
			return m, fmt.Errorf("%s: local size %d, HF size %d", hfPath, fi.Size(), size)
		}
		sum, err := hashcache.HashFile(local)
		if err != nil {
			return m, err
		}
		if e.LFS != nil {
			if sum != e.LFS.OID {
				return m, fmt.Errorf("%s: local sha256 %s != HF lfs oid %s", hfPath, sum, e.LFS.OID)
			}
		} else {
			f, err := os.Open(local)
			if err != nil {
				return m, err
			}
			blob, err := hfapi.GitBlobSHA1(f, fi.Size())
			f.Close()
			if err != nil {
				return m, err
			}
			if blob != e.OID {
				return m, fmt.Errorf("%s: local git blob sha1 %s != HF oid %s", hfPath, blob, e.OID)
			}
		}
		disk := DiskName(o.ID, hfPath, o.StoreAs)
		r, err := mktorrent.Build(mktorrent.Options{
			Path: local, Name: disk, WebSeed: hfapi.ResolveURL(o.Repo, sha, hfPath),
			Announce: o.Announce, Comment: o.Repo + "@" + sha + " " + hfPath,
		})
		if err != nil {
			return m, err
		}
		if err := os.MkdirAll(o.TorrentsDir, 0o755); err != nil {
			return m, err
		}
		if err := mktorrent.Write(filepath.Join(o.TorrentsDir, r.InfoHash+".torrent"), r.MetaInfo); err != nil {
			return m, err
		}
		f := catalog.File{Name: hfPath, Size: fi.Size(), SHA256: sum, InfoHash: r.InfoHash, Magnet: r.Magnet}
		if disk != path.Base(hfPath) {
			f.StoreAs = disk
		}
		m.Files = append(m.Files, f)
	}
	return m, catalog.Validate([]catalog.Model{m})
}
