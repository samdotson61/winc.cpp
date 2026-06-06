#!/usr/bin/env bash
# winc.cpp one-click setup (macOS). Double-click me in Finder.
set -e
cd "$(dirname "$0")"

if [ ! -x ./winc ]; then
  if command -v go >/dev/null 2>&1; then
    echo "Building winc from source..."
    go build -o winc ./cmd/winc
  else
    echo "[x] winc not found and Go is not installed."
    echo "    Download a prebuilt winc release into this folder, or install Go"
    echo "    (brew install go) and re-run."
    exit 1
  fi
fi

./winc setup
echo
echo "Done. Open a new terminal and run:  winc ls"
