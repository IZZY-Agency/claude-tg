VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
PREFIX  ?= $(HOME)/.local
DIST    := dist
RELEASE := tg-$(VERSION)-macos-universal
# Published builds: a Developer ID Application identity and a notarytool
# keychain profile. Empty means no notarization and, for `make release`, an ad
# hoc signature (local test builds).
SIGN_IDENTITY  ?=
NOTARY_PROFILE ?=

.PHONY: build install test live-test vet clean dist release

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/tg ./cmd/tg

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/tg $(PREFIX)/bin/tg

test:
	go test ./...

live-test:
	TG_LIVE=1 go test ./... -run Live -count=1 -v

vet:
	go vet ./...

# dist builds the release archives, one per platform (macOS arm64 and x86_64,
# Linux arm64 and x86_64), each with tg, LICENSE and README.md plus a .sha256
# sidecar, into dist/<version>/. With SIGN_IDENTITY set the macOS binaries are
# signed (hardened runtime, timestamp) and, with NOTARY_PROFILE, notarized
# through scripts/notarize.sh; see scripts/package.sh. Tag main first so
# VERSION is a release number. Published build:
#   make dist SIGN_IDENTITY="Developer ID Application: <team>" NOTARY_PROFILE=<profile>
dist:
	SIGN_IDENTITY="$(SIGN_IDENTITY)" NOTARY_PROFILE="$(NOTARY_PROFILE)" scripts/package.sh $(VERSION) $(DIST)/$(VERSION)

# release builds ONE universal macOS binary (arm64 + amd64) for hand-outs,
# signs it (ad hoc, or with SIGN_IDENTITY plus hardened runtime and
# timestamp), notarizes it when NOTARY_PROFILE is set, and packs it with a
# SHA-256 file. Needs macOS (lipo, codesign, notarytool). The published
# per-platform archives come from `make dist`.
#   make release SIGN_IDENTITY="Developer ID Application: <team>" NOTARY_PROFILE=<profile>
release:
	rm -rf $(DIST)/$(RELEASE) $(DIST)/$(RELEASE).tar.gz $(DIST)/$(RELEASE).tar.gz.sha256
	mkdir -p $(DIST)/$(RELEASE)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $(DIST)/tg-arm64 ./cmd/tg
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $(DIST)/tg-amd64 ./cmd/tg
	lipo -create -output $(DIST)/$(RELEASE)/tg $(DIST)/tg-arm64 $(DIST)/tg-amd64
	rm -f $(DIST)/tg-arm64 $(DIST)/tg-amd64
	chmod 0755 $(DIST)/$(RELEASE)/tg
ifeq ($(SIGN_IDENTITY),)
	codesign --force -s - $(DIST)/$(RELEASE)/tg
else
	codesign --force --timestamp --options runtime -s "$(SIGN_IDENTITY)" $(DIST)/$(RELEASE)/tg
endif
ifneq ($(NOTARY_PROFILE),)
	scripts/notarize.sh $(DIST)/$(RELEASE)/tg "$(NOTARY_PROFILE)" $(RELEASE)
endif
	tar -C $(DIST)/$(RELEASE) -czf $(DIST)/$(RELEASE).tar.gz tg
	cd $(DIST) && shasum -a 256 $(RELEASE).tar.gz > $(RELEASE).tar.gz.sha256
	rm -r $(DIST)/$(RELEASE)
	@echo "release: $(DIST)/$(RELEASE).tar.gz"
	@cat $(DIST)/$(RELEASE).tar.gz.sha256

clean:
	rm -rf bin $(DIST)
