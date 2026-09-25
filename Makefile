# SuperIDM — common development tasks.
#
#   make            build Windows + Linux binaries into ./dist
#   make test       run the test suite (including the integration tests)
#   make bench      run the throughput comparison against a local origin
#   make icons      regenerate every icon from tools/make_icons.py
#   make release    full release bundle (binaries + extension zip + checksums)
#   make clean

SHELL  := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo 1.0.0)
OUT    ?= dist
GO     ?= go
LDFLAGS := -s -w -X main.Version=$(VERSION)

.PHONY: all build windows linux test test-short race bench icons extension release clean fmt vet check

all: build

build: windows linux extension

windows:
	@mkdir -p $(OUT)
	@echo "==> SuperIDM.exe (windows/amd64, GUI)"
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath \
		-ldflags "$(LDFLAGS) -H=windowsgui" -o $(OUT)/SuperIDM.exe ./cmd/superidm
	@echo "==> SuperIDM-cli.exe (windows/amd64, console)"
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath \
		-ldflags "$(LDFLAGS)" -o $(OUT)/SuperIDM-cli.exe ./cmd/superidm
	@echo "==> SuperIDM-arm64.exe (windows/arm64, GUI)"
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -trimpath \
		-ldflags "$(LDFLAGS) -H=windowsgui" -o $(OUT)/SuperIDM-arm64.exe ./cmd/superidm

linux:
	@mkdir -p $(OUT)
	@echo "==> superidm-linux (for testing the API/UI on this machine)"
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT)/superidm-linux ./cmd/superidm

test:
	$(GO) test ./... -timeout 300s

test-short:
	$(GO) test ./... -short -timeout 120s

race:
	$(GO) test ./internal/engine -race -timeout 300s

bench:
	@bash tools/benchmark.sh

fmt:
	gofmt -w cmd internal

vet:
	$(GO) vet ./...

check: fmt vet test

icons:
	python3 tools/make_icons.py

extension:
	@mkdir -p $(OUT)
	@rm -f $(OUT)/SuperIDM-chrome-extension-$(VERSION).zip
	@cd chrome-extension && zip -qr ../$(OUT)/SuperIDM-chrome-extension-$(VERSION).zip . -x '*.DS_Store'
	@echo "==> $(OUT)/SuperIDM-chrome-extension-$(VERSION).zip"

release:
	VERSION=$(VERSION) OUT=$(OUT) bash build.sh

clean:
	rm -rf $(OUT)
