#!/bin/sh
# installs claude-go to ~/.local/bin (no sudo). refuses a download whose sha-256
# does not match the release's checksums.txt.
#   curl -fsSL https://raw.githubusercontent.com/Svector-anu/claude-go/main/install.sh | sh
#   ... | sh -s -- v0.1.0     # pin a version
set -eu

repo="Svector-anu/claude-go"
version="${1:-latest}"
bin_dir="${CLAUDE_GO_BIN_DIR:-$HOME/.local/bin}"

fail() { echo "claude-go install: $*" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported os $(uname -s) (macos and linux only)" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) fail "unsupported cpu $(uname -m)" ;;
esac

if [ -n "${CLAUDE_GO_RELEASE_URL:-}" ]; then
  base="$CLAUDE_GO_RELEASE_URL" # for testing against a local server
elif [ "$version" = latest ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/$version"
fi
archive="claude-go_${os}_${arch}.tar.gz"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$base/$archive" -o "$tmp/$archive" || fail "could not download $base/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || fail "could not download checksums.txt"

want="$(grep " $archive\$" "$tmp/checksums.txt" | awk '{print $1}')"
[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
else
  got="$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $archive, refusing to install"

tar -xzf "$tmp/$archive" -C "$tmp" claude-go
mkdir -p "$bin_dir"
# a fresh file, then an atomic rename: overwriting a running or signed binary in place
# can get it killed on macos.
cp "$tmp/claude-go" "$bin_dir/.claude-go.new"
chmod 755 "$bin_dir/.claude-go.new"
mv -f "$bin_dir/.claude-go.new" "$bin_dir/claude-go"

echo "installed $bin_dir/claude-go"
case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) echo "add $bin_dir to your PATH, then open a new terminal" ;;
esac
echo "next: claude-go setup"
