# Changelog

## v0.2.1 — 2026-09-21

### Fixes

- **seed_only_existing nodes showed every per-file model as `absent`.**
  A per-file model's catalog entry has a companion `README.md` next to its
  `.gguf`. On a `seed_only_existing` node with an empty data_dir the README
  exists nowhere locally, so its job was (correctly) `absent` — and the
  model's status took the lowest file state, so a model whose `.gguf` had
  been found (via `viiwork_configs` symlinks, `adopt`, or data_dir),
  verified and was seeding in place showed as `absent` at "100.0%", with the
  README's error, and `/ensure` answered 409 instead of the path. For a
  model whose main file is a `.gguf` that is seeding, a missing
  documentation companion (README\*, LICENSE\*, NOTICE\*, \*.md, \*.txt)
  no longer affects the model's state, error, path or percentage (it is
  still listed as absent under the model's files). Anything else missing —
  a split `.gguf` shard, a non-GGUF model's shard or config — still makes
  the model absent. So on a `seed_only_existing` node, `/ensure` 200 means
  "weights complete and verified; a documentation companion may be absent
  on this node". `DONE` is now rounded down, so 100.0% means every
  byte is present.
- **No more "short write" warning on every hashed piece.** Verifying or
  seeding with classic file IO (what the daemon uses) logged
  `finished hashing piece ... correct=true err="short write"` at WARN for
  almost every piece, in folder and per-file models alike. The error was
  spurious: the vendored anacrolix's classic reader ends each per-file read
  by returning `io.ErrShortWrite` once it has passed on exactly the bytes
  asked for. Every piece was always hashed in full and verification results
  were correct; the bogus error also made anacrolix skip smart-ban
  bookkeeping and peer blame for hash failures. Now treated as success
  (patch documented in `third_party/anacrolix-torrent/VIIWORK-PARROT-PATCH.md`).
  With the fix, anacrolix's smart-ban and piece blame work again as designed:
  peers that send bad pieces are banned (by IP, until the downloading
  daemon restarts).
- **`status` shows an idle rate as `0`, not `unlimited`.** `DOWN`/`UP` are
  measured rates; `unlimited` is kept for limits (`limit`).
- **Quieter local peer discovery.** anacrolix's BEP-14 "receiver Ignoring
  own message" (logged for each of our own announces) and "Multicasting on
  …" are now debug-level.
- **Publishing uploads folder models' torrents.** `scripts/catalog-publish.sh`
  now collects a folder model's single model-level torrent along with the
  per-file ones.
- **The UDP home tracker is no longer in default announce lists or
  magnets.** Torrents and catalog magnets were regenerated without it;
  infohashes are unchanged (the announce list is outside the info dict).

## v0.2.0 — 2026-09-21

Changes since v0.1.0, to be released as v0.2.0.

> **Upgrade every node before publishing a catalog with folder models.**
> Publishing the first folder model makes the catalog format version 2.
> v0.1.0 nodes reject it ("unsupported version 2") and stay frozen on their
> cached catalog from then on: they keep seeding what they have, but pick up
> no further catalog changes — new models, new revisions, removals — until
> they are upgraded to v0.2.0.

### Folder models

A catalog model may now be a **folder model** (`layout: dir`), for Hugging
Face safetensors repos that vLLM/SGLang load as a directory (e.g. 57 files /
160 GB, or 418 files / 126 GB). It has exactly one torrent: a v1 multi-file
torrent whose `info.name` is the HF revision and whose files are the HF
paths, with the web-seed `https://huggingface.co/<repo>/resolve/` (trailing
slash), so BEP-19 requests `…/resolve/<revision>/<path>` — any BitTorrent
client can web-seed it straight from Hugging Face. Its files keep `name`,
`size` and `sha256`; the infohash and magnet are model-level. Folder models
may share file content with each other (e.g. the same `tokenizer.json` in
two quantizations) and empty files are exempt, but a per-file model's file
may not duplicate a folder's. An optional
`license_url` (any model) points at license text that isn't in the repo.

- `viiwork-parrot mktorrent --layout dir … --local <dir> (--all | FILE…)`
  publishes one from an `hf download --local-dir` copy. `--all` is every file
  of the repo at the revision except `.gitattributes`. Every file is checked
  exactly like a per-file model's (size; sha256 against `lfs.oid`, or the git
  blob sha1 against `oid`) in the same single streaming pass that hashes the
  pieces, with per-file progress; nothing is written unless all match. The
  infohash is deterministic for the same files and revision.
- A node handles a folder model as one job. It downloads into
  `data_dir/.incoming/<id>/`, verifies every file's sha256, and moves the
  whole directory to `data_dir/<id>` with no-replace semantics
  (`renameat2(RENAME_NOREPLACE)`): a directory that isn't viiwork-parrot's own
  recorded, unchanged download is never replaced or merged into — the model
  fails with "left untouched". `/ensure` and `/status` return the directory.
- Adoption: `data_dir/<id>`, every directory under `models.adopt`, and every
  viiwork `path:` that is a directory are candidates; the first holding every
  catalog file (relative path, size, sha256) is seeded in place, further full
  copies are reported as duplicates, and partial copies are never adopted.
- `downloaded.json` records a folder download file by file (size and mtime);
  `prune` removes it only while it holds exactly those files, unchanged, and
  a new revision replaces only such an unchanged own download.
- The landing page shows a folder model as one row (file count, total size,
  revision, one magnet and `.torrent`) with its file list collapsed.

**Catalog format version 2.** `catalog build` writes `"version": 2` only when
the catalog contains a folder model; a per-file-only catalog is still
version 1 and byte-compatible with v0.1.0 nodes. A v0.1.0 node refuses a
version-2 catalog ("unsupported version 2") and keeps using its cached
catalog, so it stops picking up catalog updates. **Upgrade every node to
v0.2.0 before publishing a catalog with a folder model.**

### Fixes

- The landing page rendered every magnet link as `href="#ZgotmplZ"`
  (html/template does not trust the `magnet:` scheme); catalog magnets are
  now emitted as trusted URLs.
- A second local patch to the vendored anacrolix
  (`third_party/anacrolix-torrent/VIIWORK-PARROT-PATCH.md`): file storage
  opened zero-length files for every read and write that touched them, so
  with mmap file IO an empty file in a multi-file torrent (mmap of length 0,
  `EINVAL`) stalled the whole download. Zero-length extents are now skipped.
  And opening a torrent no longer re-creates (`O_TRUNC`) existing empty
  files, which rewrote the mtime of files in adopted folders viiwork-parrot
  seeds in place (and could truncate one changed since verification).
- A torrent whose storage anacrolix cannot open is now a failed job instead
  of a daemon panic (anacrolix drops that error and leaves the torrent
  without info).

### Static landing page

`viiwork-parrot catalog page` renders `dist/index.html` from the signed
catalog: a manifest of every model (Hugging Face source, license, size,
per-file magnet and `.torrent` links, sha256), how to fetch them (any
BitTorrent client, or `want: [<id>]` in a node's config), the tracker list,
and the ed25519 public key to verify `catalog.json` itself. It's a single
self-contained HTML file (inline CSS, no JavaScript, no external fonts) built
only from the public catalog and CLI flags, so it can never leak deployment
details. `scripts/catalog-publish.sh` now generates and uploads it alongside
`catalog.json`.

## v0.1.0

Initial public release. viiwork-parrot — the BitTorrent tracker and seeding
daemon viiwork uses to distribute its model files; a companion to
[Pirate Face](https://pirateface.co/) — from a signed catalog, so any host can
get an exact copy of a model without waiting on a slow single-origin download
and without the fleet's owner having to run their own file server for it.

### Signed catalog

`catalog.json` lists every published model: its Hugging Face origin, license,
per-file size, SHA-256, BitTorrent infohash and magnet link. It is Ed25519-signed
(`catalog.json.sig`), and a node refuses to act on a catalog whose signature
does not verify against its compiled-in (or configured) public key — the one
thing every node trusts without a TLS chain, since the catalog itself is
served over plain HTTP from behind a tailnet.

### Per-file torrents, with a Hugging Face web-seed

Each catalog file gets its own `.torrent` (BEP-19 web-seeded, `mktorrent`
against Hugging Face's `resolve/<revision>/<path>` URL), so a node's very
first download of a model already has a seeder even with zero peers, and swarm
peers matter more as adoption grows rather than being required from the start.

### Per-connection throttling, with a LAN/tailnet bypass

Upload and download limits apply per connection, not just in aggregate, so one
greedy peer cannot starve the rest of a torrent's swarm. Same-LAN and tailnet
peers (`local.cidrs`, defaulting to RFC1918 + loopback + the Tailscale CGNAT
range and its IPv6 ULA) bypass the limit and the connection cap entirely by
default — model bytes moving between two of your own hosts should saturate
the LAN, not the number picked for strangers on the internet.

### Schedules and overrides

`limits.schedule` is an ordered list of time-of-day/day-of-week rules (first
match wins, local time) that change the active limits — e.g. a lower upload
cap during work hours, unlimited overnight. The loopback API can set a
temporary override on top of whatever the schedule currently says, for a
one-off "let this finish faster right now."

### Single-copy-per-host adoption

A host that already has a model file — under any name, anywhere `models.adopt`
or a referenced `viiwork.yaml`'s model paths point — is seeded in place. No
copy, no rename, no re-download: the existing file is hashed, verified against
the catalog, and adopted as-is. This is what lets a fleet's existing hosts join
the swarm as seeders on day one, and it means a host running both viiwork and
viiwork-parrot never stores a model's bytes twice.

### Verified downloads

A download is promoted into place only after its SHA-256 matches the catalog
entry, via link-then-unlink so a file at the final path is never partially
overwritten: if a download fails, disk usage never overcommits it, and a
model that fails verification is marked `failed` with a reason rather than
silently retried into a bad state forever — it *is* retried, but on the next
catalog update.

### Loopback API and CLI

The daemon exposes a `127.0.0.1`-only HTTP API: `/status`, `/ensure`
(the integration point — `POST /ensure {"id": …}` returns the file's path once
it is present and verified, `202` while it is still in progress), limit
overrides, and catalog management. The CLI is the same binary in a different
mode: `catalog build`/`sign`/`verify` for publishing, and thin wrappers over
the API for day-to-day use.

### Daemon and systemd unit

`viiwork-parrot daemon --config viiwork-parrot.yaml` is the whole node.
`deploy/viiwork-parrot@.service` is a template unit (`viiwork-parrot@$USER`)
with a `StateDirectory`, restart-on-failure, and
`TORRENT_STORAGE_DEFAULT_FILE_IO=classic` set for it (see Known issues).

### Tracker deployment templates

`deploy/tracker/` holds a self-hosted opentracker plus static file server,
templated for the common shape of a tailnet-only tracker host fronted by a
separate public-facing reverse proxy — `Caddyfile.example` and
`docker-compose.example.yaml` with placeholders, and a README covering the
trust boundary a setup like this has to get right (which header to trust,
and from which upstream) regardless of which CDN or tailnet you use.

### Vendored anacrolix patch

`third_party/anacrolix-torrent` carries a local patch to
`github.com/anacrolix/torrent` (via a `replace` directive) fixing a real,
reliably-reproducible panic: dropping a torrent while one of its web-seed
requests is in flight raced `Client.updateWebseedRequests` against
`Torrent.close()` under the same lock, and the assertion in
`webseed-requesting.go` fired. This is exactly what stopping, pruning, or
re-cataloguing a model *during* its web-seed download does, so it needed
fixing rather than working around — see
`third_party/anacrolix-torrent/VIIWORK-PARROT-PATCH.md` for the full trace and
the fix.

### Known issues

- **A `prune: true` revision bump removes the old file before the new
  revision verifies.** When the catalog replaces a file with a new revision
  (new infohash, same disk name) and `prune` is on, the old file is deleted
  as soon as its seeding job stops, rather than being kept until the new
  download verifies. Without `prune`, the old file is instead kept and
  swapped in place only after the new one verifies — the safer sequence,
  just not the default.
- **The upstream `README.md` is shipped as the license file** for catalog
  entries whose Hugging Face repo has no `LICENSE` file (true of the initial
  catalog's GGUF repos) — the model card, on the theory that it states the
  license. This is a deviation from shipping an actual license file and needs
  a human sign-off, per model, that the card's wording is adequate; it is not
  a substitute for reading the model's actual terms.
