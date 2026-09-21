package storage

// viiwork-parrot patch: regression test for the spurious "short write" that
// classic file IO reported for every piece ending part-way through a file.
// See VIIWORK-PARROT-PATCH.md.

import (
	"context"
	"crypto/sha1"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-quicktest/qt"

	"github.com/anacrolix/torrent/metainfo"
)

type writeToTestFile struct {
	path   string
	length int64
}

// buildWriteToTorrent writes files under dir and returns a v1 info whose
// piece hashes match them.
func buildWriteToTorrent(t *testing.T, dir string, files []writeToTestFile, pieceLength int64) *metainfo.Info {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	var all []byte
	info := &metainfo.Info{Name: "rev", PieceLength: pieceLength}
	for _, f := range files {
		b := make([]byte, f.length)
		rng.Read(b)
		all = append(all, b...)
		qt.Assert(t, qt.IsNil(os.WriteFile(filepath.Join(dir, f.path), b, 0o644)))
		info.Files = append(info.Files, metainfo.FileInfo{Path: []string{f.path}, Length: f.length})
	}
	if len(files) == 1 {
		info.Name = files[0].path
		info.Length = files[0].length
		info.Files = nil
	}
	for off := int64(0); off < int64(len(all)); off += pieceLength {
		sum := sha1.Sum(all[off:min(off+pieceLength, int64(len(all)))])
		info.Pieces = append(info.Pieces, sum[:]...)
	}
	return info
}

func hashAllPieces(t *testing.T, dir string, info *metainfo.Info) (bad []int) {
	t.Helper()
	s := NewFileOpts(NewFileClientOpts{
		ClientBaseDir:   dir,
		TorrentDirMaker: func(base string, _ *metainfo.Info, _ metainfo.Hash) string { return base },
		FilePathMaker: func(o FilePathMakerOpts) string {
			if len(o.File.BestPath()) == 0 {
				return o.Info.BestName()
			}
			return filepath.Join(o.File.BestPath()...)
		},
		PieceCompletion: NewMapPieceCompletion(),
	})
	defer s.Close()
	ts, err := s.OpenTorrent(context.Background(), info, metainfo.Hash{})
	qt.Assert(t, qt.IsNil(err))
	defer ts.Close()
	for i := 0; i < info.NumPieces(); i++ {
		p := info.Piece(i)
		h := sha1.New()
		n, err := ts.Piece(p).(io.WriterTo).WriteTo(h)
		// The hashing path in torrent.finishHash logs anything but nil/EOF
		// at WARN, so a successful full read must not return an error.
		switch err {
		case nil, io.EOF:
		default:
			t.Errorf("piece %d: WriteTo err = %v (n=%d of %d)", i, err, n, p.Length())
		}
		if n != p.Length() || string(h.Sum(nil)) != string(p.V1Hash().Unwrap().Bytes()) {
			bad = append(bad, i)
		}
	}
	return
}

func withFileIo(t *testing.T, name string) {
	old := defaultFileIo
	t.Cleanup(func() { defaultFileIo = old })
	switch name {
	case "classic":
		defaultFileIo = func() fileIo { return newClassicFileIo() }
	case "mmap":
		defaultFileIo = func() fileIo { return &mmapFileIo{} }
	}
}

func TestFilePieceWriteToNoSpuriousShortWrite(t *testing.T) {
	// Piece length is deliberately not a multiple of io.Copy's 32 KiB
	// buffer, files span piece boundaries, one file is zero-length, and one
	// piece boundary coincides with a file end.
	const pieceLength = 40000
	cases := map[string][]writeToTestFile{
		"multi-file": {
			{"a.bin", 100_000},
			{"empty.txt", 0},
			{"b.bin", 20_000}, // ends exactly at a piece boundary (120000)
			{"c.bin", 257_123},
			{"d.json", 17},
		},
		"single-file": {
			{"model.gguf", 333_333},
		},
	}
	for _, ioName := range []string{"classic", "mmap"} {
		for name, files := range cases {
			t.Run(ioName+"/"+name, func(t *testing.T) {
				withFileIo(t, ioName)
				dir := t.TempDir()
				info := buildWriteToTorrent(t, dir, files, pieceLength)
				qt.Assert(t, qt.HasLen(hashAllPieces(t, dir, info), 0))

				// A corrupted byte must still fail exactly its piece: the last
				// byte of the torrent, and (multi-file) the last byte of piece
				// 0, whose extent ends mid-file (the formerly "short write" case).
				last := files[len(files)-1]
				flipByte(t, filepath.Join(dir, last.path), last.length-1)
				qt.Assert(t, qt.DeepEquals(hashAllPieces(t, dir, info), []int{info.NumPieces() - 1}))
				flipByte(t, filepath.Join(dir, last.path), last.length-1) // restore
				qt.Assert(t, qt.HasLen(hashAllPieces(t, dir, info), 0))
				if name != "multi-file" {
					return
				}
				flipByte(t, filepath.Join(dir, "a.bin"), pieceLength-1)
				qt.Assert(t, qt.DeepEquals(hashAllPieces(t, dir, info), []int{0}))
				flipByte(t, filepath.Join(dir, "a.bin"), pieceLength-1) // restore

				// A truncated file fails the pieces it no longer fills:
				// a.bin (0..100000) cut to 60000 leaves pieces 1 and 2 short.
				qt.Assert(t, qt.IsNil(os.Truncate(filepath.Join(dir, "a.bin"), 60_000)))
				qt.Assert(t, qt.DeepEquals(hashAllPieces(t, dir, info), []int{1, 2}))
			})
		}
	}
}

func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	qt.Assert(t, qt.IsNil(err))
	defer f.Close()
	b := make([]byte, 1)
	_, err = f.ReadAt(b, off)
	qt.Assert(t, qt.IsNil(err))
	b[0] ^= 0xff
	_, err = f.WriteAt(b, off)
	qt.Assert(t, qt.IsNil(err))
}
