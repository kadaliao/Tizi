.PHONY: build test fmt lint install release gui install-gui

VERSION ?= $(shell git describe --tags --dirty 2>/dev/null || printf dev)
BINARY := bin/tizi

build:
	mkdir -p bin
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) ./cmd/tizi

test:
	go test ./...

fmt:
	gofmt -w cmd internal

lint:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

release:
	./scripts/release.sh "$(VERSION)"

install: build
	mkdir -p $(HOME)/.local/bin
	tmp="$(HOME)/.local/bin/.tizi.$$$$"; \
	trap 'rm -f "$$tmp"' EXIT HUP INT TERM; \
	cp $(BINARY) "$$tmp"; \
	chmod 755 "$$tmp"; \
	mv -f "$$tmp" $(HOME)/.local/bin/tizi

# macOS 桌面版：SwiftUI 界面 + 本程序的 _gui-engine 后台引擎，需要 Xcode。
gui: build
	VERSION="$(VERSION)" ENGINE_BIN="$(abspath $(BINARY))" ./macos/scripts/build-app.sh

install-gui: gui
	mkdir -p $(HOME)/Applications
	set -e; tmp="$(HOME)/Applications/.TiziDesktop.$$$$.app"; \
	trap 'rm -rf "$$tmp"' EXIT HUP INT TERM; \
	ditto bin/TiziDesktop.app "$$tmp"; \
	rm -rf $(HOME)/Applications/TiziDesktop.app; \
	mv "$$tmp" $(HOME)/Applications/TiziDesktop.app
