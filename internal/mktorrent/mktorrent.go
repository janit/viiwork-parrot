// Package mktorrent builds the v1 single-file torrents viiwork-parrot publishes.
package mktorrent

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

var DefaultAnnounce = [][]string{
	{"udp://t.256.fi:6969/announce"},
	{"https://parrot.lnx.fi/announce"},
	{"udp://tracker.pirateface.co:6969/announce"},
	{"udp://tracker.opentrackr.org:1337/announce"},
	{"udp://open.stealth.si:80/announce"},
}

type Options struct {
	Path     string     // local file to hash
	Name     string     // info.name = the disk name nodes store the file as
	WebSeed  string     // exact HF resolve URL for this file (no trailing slash)
	Announce [][]string // nil = DefaultAnnounce
	Comment  string
}

type Result struct {
	MetaInfo *metainfo.MetaInfo
	InfoHash string
	Magnet   string
}

func Build(o Options) (Result, error) {
	fi, err := os.Stat(o.Path)
	if err != nil {
		return Result{}, err
	}
	if !fi.Mode().IsRegular() {
		return Result{}, fmt.Errorf("%s: not a regular file", o.Path)
	}
	if o.WebSeed == "" || strings.HasSuffix(o.WebSeed, "/") {
		return Result{}, errors.New("web-seed must be the exact file URL without a trailing slash")
	}
	info := metainfo.Info{Name: o.Name, Length: fi.Size(), PieceLength: metainfo.ChoosePieceLength(fi.Size())}
	if err := info.GeneratePieces(func(metainfo.FileInfo) (io.ReadCloser, error) { return os.Open(o.Path) }); err != nil {
		return Result{}, fmt.Errorf("hashing pieces: %w", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		return Result{}, err
	}
	ann := o.Announce
	if ann == nil {
		ann = DefaultAnnounce
	}
	mi := &metainfo.MetaInfo{
		InfoBytes:    infoBytes,
		Announce:     ann[0][0],
		AnnounceList: ann,
		UrlList:      metainfo.UrlList{o.WebSeed},
		CreatedBy:    "viiwork-parrot",
		CreationDate: time.Now().Unix(),
		Comment:      o.Comment,
	}
	ih := mi.HashInfoBytes()
	var trackers []string
	for _, tier := range ann {
		trackers = append(trackers, tier...)
	}
	m := metainfo.Magnet{InfoHash: ih, DisplayName: o.Name, Trackers: trackers, Params: url.Values{"ws": {o.WebSeed}}}
	return Result{MetaInfo: mi, InfoHash: ih.HexString(), Magnet: m.String()}, nil
}

// Write stores mi at path atomically: it writes a temp file in the same
// directory and renames it over path, so a reader never sees a partial
// torrent and an existing file is replaced, not truncated in place.
func Write(path string, mi *metainfo.MetaInfo) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := mi.Write(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
