# winc.cpp - build + cross-compile
BINARY  := winc
PKG     := ./cmd/winc
# Stamp the binary with the git tag, minus the "v" prefix so it matches the
# in-source form the update check compares against (falls back to the short
# hash, then to the in-source default if git is unavailable).
# Override: make release VERSION=1.5.0
VERSION ?= $(shell git describe --tags --always 2>/dev/null | sed 's/^v//')
LDFLAGS := -s -w
ifneq ($(VERSION),)
LDFLAGS += -X winc/internal/cli.Version=$(VERSION)
endif
GOFLAGS :=

.PHONY: build test vet tidy release clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY)$(EXT) $(PKG)

test:
	go test ./cmd/... ./internal/...

vet:
	go vet ./cmd/... ./internal/...

tidy:
	go mod tidy

# Cross-compile every supported target into dist/.
release:
	@mkdir -p dist
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/winc-windows-amd64.exe $(PKG)
	GOOS=windows GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/winc-windows-arm64.exe $(PKG)
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/winc-linux-amd64       $(PKG)
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/winc-linux-arm64       $(PKG)
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/winc-darwin-arm64      $(PKG)
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/winc-darwin-amd64      $(PKG)

clean:
	rm -rf dist $(BINARY) $(BINARY).exe
