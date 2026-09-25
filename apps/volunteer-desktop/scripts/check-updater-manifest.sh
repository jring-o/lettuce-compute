#!/usr/bin/env bash
# Check that a desktop updater manifest (latest.json) can update every platform the app
# ships for: each key below must carry a download URL and a signature. The desktop
# workflow runs this before it points the auto-updater at a published release, so a
# release whose build lost a platform's signed updater archive is refused instead of
# silently leaving every install on that platform without updates.
#
# Usage: check-updater-manifest.sh <latest.json>
set -euo pipefail

manifest="${1:?usage: check-updater-manifest.sh <latest.json>}"
required=(darwin-aarch64 darwin-x86_64 linux-x86_64 windows-x86_64)

echo "Manifest covers: $(jq -r '.platforms | keys | join(", ")' "$manifest")"

status=0
for platform in "${required[@]}"; do
  if ! jq -e --arg p "$platform" \
    '((.platforms[$p].url // "") != "") and ((.platforms[$p].signature // "") != "")' \
    "$manifest" > /dev/null; then
    echo "::error::$manifest has no updater entry with a url and a signature for $platform"
    status=1
  fi
done
exit "$status"
