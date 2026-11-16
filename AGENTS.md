# AGENTS.md

Rules for agents working in this repository.

## What this is

viiwork-parrot is the self-hosted BitTorrent tracker and seeding daemon that
viiwork uses to distribute its model files. The swarm is public. It is one Go
binary (Go 1.27). It relates to viiwork only through the `/ensure` HTTP
contract on its loopback API: no shared code, independent releases.

- `cmd/viiwork-parrot`: the command line (daemon, client, `catalog`,
  `mktorrent`).
- `internal/`: the packages. `node` runs the torrent jobs (download, verify,
  adopt, seed, prune, quarantine); `api` is the loopback API; `catalog`
  builds, signs, verifies and fetches the catalog; `mktorrent` and `hfapi`
  make torrents from Hugging Face repos; `site` renders the static landing
  page; `throttle`, `connbudget` and `schedule` limit bandwidth and
  connections; `config` reads `deploy/viiwork-parrot.yaml.example`'s format.
- `catalog.yaml` and `torrents/`: the published catalog source and its
  torrents. `keys/catalog.pub`: the catalog's public key.
- `third_party/anacrolix-torrent`: a patched copy of
  `github.com/anacrolix/torrent`, used through a `replace` in `go.mod`.
- `test/`: integration tests (build tag `integration`).
- `deploy/`: the systemd unit, the node config example and the tracker
  templates.
- `CHANGELOG.md`: what each release contains.

## Rules

- Do not edit `third_party/anacrolix-torrent` except to fix the patched
  bug; any change there is recorded in its `VIIWORK-PARROT-PATCH.md`.
- Never commit a private key (`*.key`) or operator-filled tracker files
  (`deploy/tracker/Caddyfile`, `deploy/tracker/docker-compose.yaml`).
  Private runbooks stay out of the repository.
- Keep the `/ensure` contract and the catalog format compatible with nodes
  already deployed. A catalog change older nodes refuse needs a new format
  version and a note in `CHANGELOG.md` telling operators to upgrade first.
- New `catalog.yaml` entries and their `torrents/` come from `mktorrent`,
  which checks every file against Hugging Face. Do not write them by hand.
- Tests must not need the network: use `httptest` and local listeners.
- The landing page has no JavaScript and is built only from the public
  catalog.
- User-facing changes go in `CHANGELOG.md`.
- Open a pull request and stop. Never push to the default branch.

## Verify

`.pi/verify-cmd` is the fast gate. Run it before you say a change is done.
It checks that `gofmt -l` finds nothing, then runs `go vet ./...`,
`go test ./...` (`make test`) and the integration tests (`make itest`).

## Commits

One plain subject that lists what changed, separated by semicolons, with the
version in parentheses for a release: "Safer prune, quarantine and shutdown;
uTP policing; stricter web-seed validation (v0.2.2)". A release also adds its
section to `CHANGELOG.md`.
