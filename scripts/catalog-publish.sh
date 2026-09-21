#!/usr/bin/env bash
# Build, sign and upload the catalog. Torrents go up before the catalog that
# references them, so nodes never see a catalog entry without its torrent.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${VIIWORK_PARROT_KEY:=$HOME/.config/viiwork-parrot/catalog.key}"
if [[ -z "${PUBLISH_DEST:-}" ]]; then
  echo "Error: PUBLISH_DEST must be set, e.g. user@host:/path/to/public/" >&2
  echo "  (rsync destination for catalog.json, its .sig, index.html, and torrents/)." >&2
  echo "  For this project's own deployment, see the runbook under docs/private/." >&2
  exit 1
fi
# RSYNC_RSH, if the destination needs a non-default ssh identity, e.g.:
#   RSYNC_RSH="ssh -i ~/.ssh/some_key -o IdentitiesOnly=yes"
command -v jq >/dev/null || { echo "jq required" >&2; exit 1; }

make build
rm -rf dist && mkdir -p dist/torrents
bin/viiwork-parrot catalog build --in catalog.yaml --out dist/catalog.json
bin/viiwork-parrot catalog sign --key "$VIIWORK_PARROT_KEY" --in dist/catalog.json
bin/viiwork-parrot catalog verify --in dist/catalog.json
bin/viiwork-parrot catalog page --in dist/catalog.json --out dist/index.html

for ih in $(jq -r '.models[] | (.infohash // empty), (.files[].infohash // empty)' dist/catalog.json); do
    cp "torrents/$ih.torrent" dist/torrents/
done

rsync -av dist/torrents/ "${PUBLISH_DEST%/}/torrents/"
rsync -av dist/catalog.json dist/catalog.json.sig dist/index.html "${PUBLISH_DEST%/}/"
echo "published $(jq '.models | length' dist/catalog.json) models"
