package catalog

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

type Fetcher struct {
	URL      string // https://parrot.lnx.fi/catalog.json
	PubKey   string
	CacheDir string
	HTTP     *http.Client
}

var infohashRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Fetch returns the verified catalog. On failure it returns the last verified
// cached catalog plus a non-nil error; (nil, err) only when no cache exists.
// A verified catalog whose Generated time is older than the cached one's is
// a failure too (rollback), and the cache is kept.
// A successful fetch whose cache write fails also returns (cat, err): the
// catalog is good, but future failures may not be able to fall back to it.
func (f *Fetcher) Fetch(ctx context.Context) (*Catalog, error) {
	body, err := f.get(ctx, f.URL, 32<<20)
	var sig []byte
	if err == nil {
		sig, err = f.get(ctx, f.URL+".sig", 4096)
	}
	if err == nil {
		err = Verify(f.PubKey, body, sig)
	}
	var cat *Catalog
	if err == nil {
		cat, err = Parse(body)
	}
	if err == nil {
		// Rollback protection: a validly signed but older catalog (an old
		// catalog.json+sig replayed by the host or anything in between)
		// must not replace a newer one we already verified.
		if cached, cerr := f.loadCache(); cerr == nil && cat.Generated.Before(cached.Generated) {
			return cached, fmt.Errorf("fetched catalog generated %s is older than the cached one (%s); keeping the cache",
				cat.Generated.Format(time.RFC3339), cached.Generated.Format(time.RFC3339))
		}
		cache := append([]byte(strings.TrimSpace(string(sig))+"\n"), body...)
		if werr := f.writeCache("catalog.cache", cache); werr != nil {
			return cat, fmt.Errorf("caching catalog: %w", werr)
		}
		return cat, nil
	}
	cached, cerr := f.loadCache()
	if cerr != nil {
		return nil, fmt.Errorf("fetch catalog: %v; no usable cache: %w", err, cerr)
	}
	return cached, fmt.Errorf("using cached catalog: %w", err)
}

func (f *Fetcher) loadCache() (*Catalog, error) {
	data, err := os.ReadFile(filepath.Join(f.CacheDir, "catalog.cache"))
	if err != nil {
		return nil, err
	}
	i := bytes.IndexByte(data, '\n')
	if i < 0 {
		return nil, fmt.Errorf("catalog cache: malformed (no signature line)")
	}
	sig, body := data[:i], data[i+1:]
	if err := Verify(f.PubKey, body, sig); err != nil {
		return nil, err
	}
	return Parse(body)
}

func (f *Fetcher) Torrent(ctx context.Context, infohash string) (*metainfo.MetaInfo, error) {
	if !infohashRe.MatchString(infohash) {
		return nil, fmt.Errorf("bad infohash %q", infohash)
	}
	cached := filepath.Join(f.CacheDir, "torrents", infohash+".torrent")
	if mi, err := metainfo.LoadFromFile(cached); err == nil && mi.HashInfoBytes().HexString() == infohash {
		return mi, nil
	}
	base := f.URL[:strings.LastIndex(f.URL, "/")+1]
	data, err := f.get(ctx, base+"torrents/"+infohash+".torrent", 16<<20)
	if err != nil {
		return nil, err
	}
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("torrent %s: %w", infohash, err)
	}
	if got := mi.HashInfoBytes().HexString(); got != infohash {
		return nil, fmt.Errorf("torrent %s: served metainfo has infohash %s", infohash, got)
	}
	if err := f.writeCache(filepath.Join("torrents", infohash+".torrent"), data); err != nil {
		return nil, err
	}
	return mi, nil
}

func (f *Fetcher) get(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	hc := f.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("GET %s: response larger than %d bytes", url, max)
	}
	return data, nil
}

func (f *Fetcher) writeCache(name string, data []byte) error {
	p := filepath.Join(f.CacheDir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
