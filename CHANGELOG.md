# Changelog

## Unreleased

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
