#!/usr/bin/env bash
# winc.cpp one-click setup (macOS). Double-click me in Finder.
# Builds winc from source (installing Go automatically if needed), then runs setup.
set -e
cd "$(dirname "$0")"

ensure_go() {
  command -v go >/dev/null 2>&1 && return
  echo "Go is not installed - winc needs it once, to build itself."
  # 1) Homebrew.
  if command -v brew >/dev/null 2>&1; then
    echo "Installing Go via Homebrew..."
    brew install go || true
  fi
  command -v go >/dev/null 2>&1 && return
  # 2) Fallback: official tarball from go.dev (no admin needed).
  echo "Installing Go from the official tarball (go.dev)..."
  local arch
  case "$(uname -m)" in
    arm64) arch=arm64 ;;
    x86_64) arch=amd64 ;;
    *) arch=arm64 ;;
  esac
  local ver
  ver="$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -n1)"
  curl -fSL "https://go.dev/dl/${ver}.darwin-${arch}.tar.gz" -o /tmp/winc-go.tgz
  rm -rf "$HOME/.winc-go" && mkdir -p "$HOME/.winc-go"
  tar -C "$HOME/.winc-go" -xzf /tmp/winc-go.tgz
  export PATH="$HOME/.winc-go/go/bin:$PATH"
}

if [ ! -x ./winc ]; then
  ensure_go
  command -v go >/dev/null 2>&1 || { echo "[x] Could not install Go. Install it from https://go.dev/dl/ and re-run."; exit 1; }
  echo "Building winc from source..."
  go build -o winc ./cmd/winc
fi

./winc setup
echo
echo "Done. Open a new terminal and run:  winc ls"
