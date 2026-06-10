#!/usr/bin/env bash
# claude-p installer — downloads the prebuilt binary for your OS/arch from the
# latest GitHub release and installs it.
#
#   curl -fsSL https://raw.githubusercontent.com/Rong-Tao/claude-p/main/install.sh | bash
#
# Override install dir:  INSTALL_DIR=/usr/local/bin   (default: ~/.local/bin)
set -euo pipefail

REPO="Rong-Tao/claude-p"
BIN_NAME="claude-p"

os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
  Linux)  os="linux" ;;
  Darwin) os="darwin" ;;
  *) echo "claude-p: unsupported OS: $os (Linux/macOS only; on Windows use WSL)" >&2; exit 1 ;;
esac
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "claude-p: unsupported architecture: $arch" >&2; exit 1 ;;
esac

asset="${BIN_NAME}-${os}-${arch}"
url="https://github.com/${REPO}/releases/latest/download/${asset}"

dest="${INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dest"

echo "claude-p: downloading ${asset} ..."
tmp="$(mktemp)"
if ! curl -fsSL "$url" -o "$tmp"; then
  echo "claude-p: download failed: $url" >&2
  echo "  (is there a release with that asset? see https://github.com/${REPO}/releases )" >&2
  rm -f "$tmp"; exit 1
fi
chmod +x "$tmp"
mv "$tmp" "$dest/$BIN_NAME"
echo "claude-p: installed to $dest/$BIN_NAME"

# PATH hint
case ":$PATH:" in
  *":$dest:"*) ;;
  *) echo "claude-p: note — $dest is not on your PATH; add it, e.g.:"
     echo "         echo 'export PATH=\"$dest:\$PATH\"' >> ~/.bashrc" ;;
esac

# runtime dependency check
miss=0
for dep in claude tmux; do
  command -v "$dep" >/dev/null 2>&1 || { echo "claude-p: warning — runtime dependency '$dep' not found on PATH"; miss=1; }
done
[ "$miss" = 0 ] && echo "claude-p: ready. Try:  $BIN_NAME \"hello\""
