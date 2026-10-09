#!/bin/sh
# Install portal on Linux, macOS or FreeBSD:
#   curl -fsSL https://raw.githubusercontent.com/KhadeerBasha1232/portal/main/install.sh | sh
set -eu

REPO="KhadeerBasha1232/portal"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin|freebsd) ;;
  *) echo "unsupported OS: $os (on Windows download the .zip from the releases page)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=x86_64 ;;
  aarch64|arm64) arch=arm64 ;;
  armv7*|armv8l) arch=armv7 ;;
  i386|i686) arch=i386 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

tag=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)
[ -n "$tag" ] || { echo "could not find the latest release" >&2; exit 1; }
version=${tag#v}

file="portal_${version}_${os}_${arch}.tar.gz"
url="https://github.com/$REPO/releases/download/$tag"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading portal $tag for $os/$arch..."
curl -fsSL -o "$tmp/$file" "$url/$file"
curl -fsSL -o "$tmp/checksums.txt" "$url/checksums.txt"

expected=$(grep " $file\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$file" | cut -d' ' -f1)
else
  actual=$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1)
fi
[ "$expected" = "$actual" ] || { echo "checksum mismatch, aborting" >&2; exit 1; }

tar -xzf "$tmp/$file" -C "$tmp" portal

if [ -w /usr/local/bin ]; then
  dest=/usr/local/bin
elif command -v sudo >/dev/null 2>&1; then
  dest=/usr/local/bin
  sudo install -m 755 "$tmp/portal" "$dest/portal"
  echo "Installed to $dest/portal"
  exit 0
else
  dest="$HOME/.local/bin"
  mkdir -p "$dest"
fi
install -m 755 "$tmp/portal" "$dest/portal"
echo "Installed to $dest/portal"
case ":$PATH:" in *":$dest:"*) ;; *) echo "Add $dest to your PATH." ;; esac
