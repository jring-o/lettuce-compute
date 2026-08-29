#!/usr/bin/env bash
#
# Builds (or downloads) the lettuce-volunteer sidecar binary that the desktop app bundles.
#
# The desktop app ("Lettuce Compute") is a shell around the volunteer command-line client,
# lettuce-volunteer. The app starts that client as a background daemon and talks to it over the
# daemon's localhost management API. Tauri bundles the client as an "external binary" (a sidecar)
# and expects to find it at
#
#     src-tauri/binaries/lettuce-volunteer-<rust target triple>
#
# for example src-tauri/binaries/lettuce-volunteer-aarch64-apple-darwin.
#
# By default this script compiles the client from this repository's services/volunteer-cli source
# with the same flags the CLI release workflow uses (GOWORK=off, CGO_ENABLED=0, -trimpath) and
# stamps it with the version `git describe --tags` reports for the checkout, leading "v" removed,
# which is how the release workflow derives the version from a release tag. With --from-release
# it instead downloads the already-published client from a GitHub release of this repository and
# verifies the download against the release's .sha256 checksum file.
#
# Usage:
#   scripts/build-sidecar.sh [--target <triple>] [--version <string>]
#   scripts/build-sidecar.sh [--target <triple>] --from-release <tag>
#
#   --target <triple>     Rust target triple to produce the sidecar for. Defaults to the host
#                         triple reported by `rustc -vV`. Supported: x86_64-pc-windows-msvc,
#                         aarch64-pc-windows-msvc, x86_64-apple-darwin, aarch64-apple-darwin,
#                         x86_64-unknown-linux-gnu, aarch64-unknown-linux-gnu.
#   --from-release <tag>  A CLI release tag of this repository (for example v0.11.1). Download
#                         that release's lettuce-volunteer asset instead of compiling.
#   --version <string>    Override the version stamped into a compiled binary.
#
# Examples:
#   scripts/build-sidecar.sh                                  # compile for this machine
#   scripts/build-sidecar.sh --target x86_64-apple-darwin     # Intel Mac build from an Apple Silicon Mac
#   scripts/build-sidecar.sh --from-release v0.11.1           # reuse the published client

set -euo pipefail

app_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo_dir="$(cd "$app_dir/../.." && pwd)"
cli_dir="$repo_dir/services/volunteer-cli"
out_dir="$app_dir/src-tauri/binaries"
release_base="https://github.com/jring-o/lettuce-compute/releases/download"
ldflags_path="github.com/lettuce-compute/volunteer-cli/internal/cli.version"

target=""
from_release=""
version=""

usage() { sed -n '2,/^$/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --target)       target="${2:?--target needs a value}"; shift 2 ;;
    --from-release) from_release="${2:?--from-release needs a value}"; shift 2 ;;
    --version)      version="${2:?--version needs a value}"; shift 2 ;;
    -h|--help)      usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

host_triple() {
  if command -v rustc >/dev/null 2>&1; then
    rustc -vV | sed -n 's/^host: //p'
    return
  fi
  # No Rust toolchain on PATH: infer the triple from the kernel and machine names.
  local os arch
  os="$(uname -s)"; arch="$(uname -m)"
  case "$arch" in x86_64|amd64) arch=x86_64 ;; arm64|aarch64) arch=aarch64 ;; esac
  case "$os" in
    Darwin) echo "$arch-apple-darwin" ;;
    Linux)  echo "$arch-unknown-linux-gnu" ;;
    *) echo "cannot infer a target triple on $os; pass --target" >&2; exit 1 ;;
  esac
}

# Rust target triple -> the Go platform the CLI release workflow publishes for it.
platform_for() {
  case "$1" in
    x86_64-pc-windows-msvc)    echo "windows amd64 .exe" ;;
    aarch64-pc-windows-msvc)   echo "windows arm64 .exe" ;;
    x86_64-apple-darwin)       echo "darwin amd64" ;;
    aarch64-apple-darwin)      echo "darwin arm64" ;;
    x86_64-unknown-linux-gnu)  echo "linux amd64" ;;
    aarch64-unknown-linux-gnu) echo "linux arm64" ;;
    *) echo "unsupported target triple '$1'" >&2; exit 1 ;;
  esac
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'
  fi
}

source_version() {
  # Same derivation as the CLI release workflow (tag "v1.2.3" stamps version "1.2.3"); between
  # releases this yields e.g. "0.11.1-3-g49d0ac7". Only CLI-style "v*" tags are considered, so
  # desktop release tags (desktop-v*) never leak into the client's version.
  git -C "$repo_dir" describe --tags --match 'v[0-9]*' --always | sed 's/^v//'
}

[ -n "$target" ] || target="$(host_triple)"
read -r goos goarch ext <<<"$(platform_for "$target") "
ext="${ext:-}"
out_path="$out_dir/lettuce-volunteer-$target$ext"
mkdir -p "$out_dir"

if [ -n "$from_release" ]; then
  asset="lettuce-volunteer-$goos-$goarch$ext"
  url="$release_base/$from_release/$asset"
  tmp="$out_path.download"

  echo "Downloading $url"
  curl -fsSL --retry 3 -o "$tmp" "$url"
  curl -fsSL --retry 3 -o "$tmp.sha256" "$url.sha256"

  # The checksum file is `sha256sum` output: "<hex digest>  <file name>".
  expected="$(awk '{print tolower($1)}' "$tmp.sha256")"
  actual="$(sha256_of "$tmp")"
  rm -f "$tmp.sha256"
  if ! [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || [ "$expected" != "$actual" ]; then
    rm -f "$tmp"
    echo "SHA-256 mismatch for $asset from $from_release (expected $expected, got $actual)" >&2
    exit 1
  fi
  mv -f "$tmp" "$out_path"
  echo "Verified SHA-256 $actual"
else
  [ -n "$version" ] || version="$(source_version)"
  if ! command -v go >/dev/null 2>&1; then
    echo "Go is not on PATH; install it (see services/volunteer-cli/go.mod for the version) or use --from-release." >&2
    exit 1
  fi
  echo "Building lettuce-volunteer $version for $target ($goos/$goarch)"
  # GOWORK=off: the repo-root go.work lists only the head services, so a standalone build of the
  # client must ignore it. CGO_ENABLED=0 matches the release workflow and keeps the binary
  # self-contained, which is what makes the cross-compile a plain `go build`.
  (
    cd "$cli_dir"
    GOWORK=off CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags "-X $ldflags_path=$version" -o "$out_path" ./cmd/lettuce-volunteer/
  )
fi

chmod +x "$out_path"
echo "Sidecar ready: $out_path"
if [ "$target" = "$(host_triple)" ]; then
  # Same platform as this machine, so the binary can be executed here: prove the stamp.
  "$out_path" --version
fi
