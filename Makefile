BINARY := switcher
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)

.PHONY: build test test-usage-update vet verify verify-common verify-linux cross-build benchmark-menu install run dev clean

build:
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BINARY) .

test:
	gofmt -l . && go vet ./... && go test ./...

# Fixture-only analytics/update regressions, with downloads and toolchain
# installation disabled. Go caches and HOME are private to this run.
test-usage-update:
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		export HOME="$$tmp" TMPDIR="$$tmp" GOPATH="$$tmp/go" \
			GOMODCACHE="$$tmp/go/pkg/mod" GOCACHE="$$tmp/cache" \
			GOENV=off GOWORK=off GOFLAGS= GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off; \
		go test -mod=readonly ./internal/usage ./internal/update; \
		go test -mod=readonly -race ./internal/usage ./internal/update; \
		go vet -mod=readonly ./internal/usage ./internal/update

vet:
	go vet ./...

# Run the same checks before a tag and in both CI workflows. verify runs on
# a Mac (it also builds the Swift menu bar app); verify-linux runs the same
# Go and web checks on Linux, where the tray and keep awake are native.
verify: verify-common
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		swiftc -parse-as-library -D SWITCHER_LAYOUT_TEST build/macos/menubar.swift \
			build/macos/menubar_layout_test.swift -o "$$tmp/switcher-layout-test"; \
		"$$tmp/switcher-layout-test"
	$(MAKE) --no-print-directory cross-build
	git diff --check HEAD^ HEAD
	git diff --cached --check
	git diff --check

verify-linux: verify-common
	$(MAKE) --no-print-directory cross-build
	git diff --check HEAD^ HEAD
	git diff --cached --check
	git diff --check

# Every released platform must build without cgo: the Mac slices and Linux,
# and the Tailscale add-on for Linux too.
cross-build:
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
			CGO_ENABLED=0 GOOS="$${target%/*}" GOARCH="$${target#*/}" go build -o "$$tmp/switcher-$${target%/*}-$${target#*/}" .; \
		done; \
		for arch in amd64 arm64; do \
			(cd cmd/switcher-tailnet && CGO_ENABLED=0 GOOS=linux GOARCH="$$arch" go build -o "$$tmp/tailnet-linux-$$arch" .); \
		done

verify-common:
	test -z "$$(gofmt -l .)"
	go test ./...
	go vet ./...
	GOOS=linux go vet ./...
	go test -race ./...
	node --check web/app.js
	node --check web/login.js
	node web/app_state_test.mjs
	node web/login_test.mjs
	node web/claude-sync_test.mjs
	node web/account-updates_test.mjs
	node web/account-updates_motion_test.mjs
	node web/themes_test.mjs
	node --check web/desktop-relay.js
	node web/desktop-relay_test.mjs
	node --check web/remote.js
	node web/remote_test.mjs
	node --check web/brand.js
	node --check web/settings-visuals.js
	node web/settings-visuals_test.mjs
	node --check web/phone.js
	node web/phone_test.mjs
	node --check web/phone-settings.js
	node web/phone-settings_test.mjs
	node --check web/update-progress.js
	node web/update-progress_test.mjs
	node --check web/live-state.js
	node web/live-state_test.mjs
	node web/qr_test.mjs
	node --check web/toggles.js
	node web/toggles_test.mjs
	cd cmd/switcher-tailnet && test -z "$$(gofmt -l .)" && go vet . && go test . && go build -o /dev/null .

install: build
	install -m 0755 $(BINARY) "$(shell go env GOPATH)/bin"

# Optimized build with bundled logos and six synthetic accounts. Never starts
# a server, reads real accounts, or invokes Desktop/session-sync actions.
benchmark-menu:
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		app="$$tmp/MenuBench.app"; mkdir -p "$$app/Contents/MacOS" "$$app/Contents/Resources"; \
		cp build/macos/Info.plist "$$app/Contents/Info.plist"; \
		plutil -replace CFBundleIdentifier -string sh.switcher.benchmark "$$app/Contents/Info.plist"; \
		plutil -replace CFBundleExecutable -string MenuBench "$$app/Contents/Info.plist"; \
		cp build/macos/logos/*.svg build/macos/AppIcon.icns build/macos/MenubarTemplate*.png "$$app/Contents/Resources/"; \
		swiftc -O -parse-as-library -D SWITCHER_LAYOUT_TEST build/macos/menubar.swift \
			build/macos/menubar_perf_test.swift -o "$$app/Contents/MacOS/MenuBench"; \
		"$$app/Contents/MacOS/MenuBench"

run: build
	./$(BINARY)

# Dev mode serves web/ from disk: UI changes need only a browser refresh.
dev:
	SWITCHER_DEV=1 go run .

clean:
	rm -f $(BINARY)
