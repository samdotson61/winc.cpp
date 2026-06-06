#!/usr/bin/env bash
# winc.cpp one-click setup (Linux).
# Builds winc from source (installing Go automatically if needed), then runs setup.
set -e
cd "$(dirname "$0")"

ensure_go() {
  command -v go >/dev/null 2>&1 && return
  echo "Go is not installed - winc needs it once, to build itself."
  if   command -v apt-get >/dev/null 2>&1; then sudo apt-get update && sudo apt-get install -y golang-go
  elif command -v dnf     >/dev/null 2>&1; then sudo dnf install -y golang
  elif command -v pacman  >/dev/null 2>&1; then sudo pacman -S --noconfirm go
  elif command -v zypper  >/dev/null 2>&1; then sudo zypper install -y go
  else
    echo "[x] No supported package manager found."
    echo "    Install Go from https://go.dev/dl/ and re-run this script."
    exit 1
  fi
}

if [ ! -x ./winc ]; then
  ensure_go
  echo "Building winc from source..."
  go build -o winc ./cmd/winc
fi

./winc setup
echo
echo "Done. Open a new terminal (or 'source ~/.bashrc') and run:  winc ls"
