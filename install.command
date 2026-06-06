#!/usr/bin/env bash
# winc.cpp one-click setup (macOS). Double-click me in Finder.
# Builds winc from source (installing Go automatically if needed), then runs setup.
set -e
cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is not installed - winc needs it once, to build itself."
  if command -v brew >/dev/null 2>&1; then
    echo "Installing Go via Homebrew..."
    brew install go
  else
    echo "[x] Go not found and Homebrew not installed."
    echo "    Install Homebrew from https://brew.sh (then 'brew install go'),"
    echo "    or install Go from https://go.dev/dl/, and re-run."
    exit 1
  fi
fi

if [ ! -x ./winc ]; then
  echo "Building winc from source..."
  go build -o winc ./cmd/winc
fi

./winc setup
echo
echo "Done. Open a new terminal and run:  winc ls"
