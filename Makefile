# Hippocampus — Build System
# Targets: all, binaries, deps, bundle, sign, install, clean
# Platform: autodetects macOS (Darwin), FreeBSD, Linux

.POSIX:
.SUFFIXES:

# --- Configuration ---

# Version from git tags (e.g., v1.0.1 → 1.0.1). Falls back to 0.0.0-dev if no tags.
GIT_TAG       := $(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0-dev")
GIT_DESCRIBE  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
VERSION       := $(patsubst v%,%,$(GIT_TAG))
VERSION_FULL  := $(patsubst v%,%,$(GIT_DESCRIBE))

MODULE        := github.com/ruthlesslypractical/hippocampus
REDIS_VERSION := 8.8.0
BUNDLE_ID     := com.ruthlesslypractical.hippocampus

# Directories
ROOT_DIR  := $(shell pwd)
BIN_DIR   := $(ROOT_DIR)/bin
BUILD_DIR := $(ROOT_DIR)/build
DIST_DIR  := $(ROOT_DIR)/dist
APP_DIR   := $(DIST_DIR)/Hippocampus.app

# Code signing identity (ad-hoc by default; set CODESIGN_IDENTITY for real signing)
SIGN_IDENTITY ?= $(if $(CODESIGN_IDENTITY),$(CODESIGN_IDENTITY),-)

# --- Platform Detection ---

OS       := $(shell uname -s)
ARCH     := $(shell uname -m)
NCPU     := 1

ifeq ($(OS),Darwin)
  PLATFORM   := macos
  NCPU       := $(shell sysctl -n hw.ncpu)
  # CGO flags for macOS app
  CGO_LDFLAGS := -framework UniformTypeIdentifiers
  # Package managers
  MACPORTS   := $(wildcard /opt/local/bin/port)
  HOMEBREW   := $(wildcard /opt/homebrew/bin/brew)$(wildcard /usr/local/bin/brew)
else ifeq ($(OS),FreeBSD)
  PLATFORM   := freebsd
  NCPU       := $(shell sysctl -n hw.ncpu)
  CGO_LDFLAGS :=
else ifeq ($(OS),Linux)
  PLATFORM   := linux
  NCPU       := $(shell nproc 2>/dev/null || echo 1)
  CGO_LDFLAGS :=
else
  PLATFORM   := unknown
  CGO_LDFLAGS :=
endif

# Go build flags
GOFLAGS    := -trimpath
LDFLAGS    := -s -w -X '$(MODULE)/internal/config.Version=$(VERSION_FULL)'
GO_BUILD   := CGO_LDFLAGS="$(CGO_LDFLAGS)" go build $(GOFLAGS) -ldflags "$(LDFLAGS)"

# --- Binary targets ---

BINARIES := \
	$(BIN_DIR)/hippocampus-mcp \
	$(BIN_DIR)/hippocampus-hook \
	$(BIN_DIR)/hippocampus-daemon \
	$(BIN_DIR)/hippocampus-summarize \
	$(BIN_DIR)/hippocampus-admin \
	$(BIN_DIR)/hippocampus-slack \
	$(BIN_DIR)/hippocampus-ingest \
	$(BIN_DIR)/hippocampus-app

# --- Top-level Targets ---

.PHONY: all binaries deps bundle sign install clean distclean help info release export

all: binaries  ## Build all Go binaries (default)

bundle: binaries deps sign-bundle  ## Build complete .app bundle (macOS only)

help:  ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "}; {printf "  %-16s %s\n", $$1, $$2}'

info:  ## Show detected platform info
	@echo "Platform:   $(PLATFORM) ($(OS)/$(ARCH))"
	@echo "CPUs:       $(NCPU)"
	@echo "Go:         $(shell go version 2>/dev/null || echo 'NOT FOUND')"
	@echo "Version:    $(VERSION) (full: $(VERSION_FULL))"
	@echo "Git tag:    $(GIT_TAG)"
	@echo "Sign ID:    $(SIGN_IDENTITY)"
	@echo "MacPorts:   $(if $(MACPORTS),yes,no)"
	@echo "Homebrew:   $(if $(HOMEBREW),yes,no)"

# --- Go Binaries ---

binaries: $(BINARIES)  ## Compile all Go binaries

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

$(BIN_DIR)/hippocampus-mcp: $(BIN_DIR) $(shell find cmd/mcp-server internal -name '*.go' 2>/dev/null)
	@echo "  BUILD  hippocampus-mcp"
	@$(GO_BUILD) -o $@ ./cmd/mcp-server/

$(BIN_DIR)/hippocampus-hook: $(BIN_DIR) $(shell find cmd/hook internal -name '*.go' 2>/dev/null)
	@echo "  BUILD  hippocampus-hook"
	@$(GO_BUILD) -o $@ ./cmd/hook/

$(BIN_DIR)/hippocampus-daemon: $(BIN_DIR) $(shell find cmd/daemon internal -name '*.go' 2>/dev/null)
	@echo "  BUILD  hippocampus-daemon"
	@$(GO_BUILD) -o $@ ./cmd/daemon/

$(BIN_DIR)/hippocampus-summarize: $(BIN_DIR) $(shell find cmd/summarize internal -name '*.go' 2>/dev/null)
	@echo "  BUILD  hippocampus-summarize"
	@$(GO_BUILD) -o $@ ./cmd/summarize/

$(BIN_DIR)/hippocampus-admin: $(BIN_DIR) $(shell find cmd/admin internal -name '*.go' 2>/dev/null)
	@echo "  BUILD  hippocampus-admin"
	@$(GO_BUILD) -o $@ ./cmd/admin/

$(BIN_DIR)/hippocampus-slack: $(BIN_DIR) $(shell find cmd/slack internal -name '*.go' 2>/dev/null)
	@echo "  BUILD  hippocampus-slack"
	@$(GO_BUILD) -o $@ ./cmd/slack/

$(BIN_DIR)/hippocampus-ingest: $(BIN_DIR) $(shell find cmd/ingest internal -name '*.go' 2>/dev/null)
	@echo "  BUILD  hippocampus-ingest"
	@$(GO_BUILD) -o $@ ./cmd/ingest/

$(BIN_DIR)/hippocampus-app: $(BIN_DIR) $(shell find cmd/app internal -name '*.go' 2>/dev/null)
	@echo "  BUILD  hippocampus-app"
	@CGO_LDFLAGS="$(CGO_LDFLAGS)" go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -tags production -o $@ ./cmd/app/

# --- Dependencies ---

deps: $(BUILD_DIR)/redis-server $(BUILD_DIR)/ollama  ## Acquire runtime dependencies
	@$(ROOT_DIR)/scripts/acquire-redisearch.sh "$(BUILD_DIR)"

$(BUILD_DIR)/redis-server:
	@echo "  DEPS   redis-server"
	@mkdir -p $(BUILD_DIR)
	@$(ROOT_DIR)/scripts/acquire-redis.sh "$(BUILD_DIR)" "$(REDIS_VERSION)" "$(NCPU)"

$(BUILD_DIR)/ollama:
	@echo "  DEPS   ollama"
	@mkdir -p $(BUILD_DIR)
	@$(ROOT_DIR)/scripts/acquire-ollama.sh "$(BUILD_DIR)" "$(OS)" "$(ARCH)"

# --- App Bundle (macOS) ---

.PHONY: sign-bundle

sign-bundle: binaries deps
ifneq ($(PLATFORM),macos)
	$(error App bundle is macOS only. Use 'make binaries' for $(PLATFORM).)
endif
	@echo ""
	@echo "Assembling Hippocampus.app..."
	@rm -rf "$(APP_DIR)"
	@mkdir -p "$(APP_DIR)/Contents/MacOS" "$(APP_DIR)/Contents/Resources" "$(APP_DIR)/Contents/Resources/launchd"
	@# Main binary
	@cp "$(BIN_DIR)/hippocampus-app" "$(APP_DIR)/Contents/MacOS/Hippocampus"
	@# Bundled tools
	@cp "$(BIN_DIR)/hippocampus-mcp" "$(APP_DIR)/Contents/Resources/"
	@cp "$(BIN_DIR)/hippocampus-hook" "$(APP_DIR)/Contents/Resources/"
	@cp "$(BIN_DIR)/hippocampus-daemon" "$(APP_DIR)/Contents/Resources/"
	@cp "$(BIN_DIR)/hippocampus-summarize" "$(APP_DIR)/Contents/Resources/"
	@cp "$(BIN_DIR)/hippocampus-admin" "$(APP_DIR)/Contents/Resources/"
	@cp "$(BIN_DIR)/hippocampus-slack" "$(APP_DIR)/Contents/Resources/"
	@cp "$(BIN_DIR)/hippocampus-ingest" "$(APP_DIR)/Contents/Resources/"
	@# Redis + modules
	@[ -f "$(BUILD_DIR)/redis-server" ] && cp "$(BUILD_DIR)/redis-server" "$(APP_DIR)/Contents/Resources/" || true
	@[ -f "$(BUILD_DIR)/redis-cli" ] && cp "$(BUILD_DIR)/redis-cli" "$(APP_DIR)/Contents/Resources/" || true
	@[ -f "$(BUILD_DIR)/redisearch.so" ] && cp "$(BUILD_DIR)/redisearch.so" "$(APP_DIR)/Contents/Resources/" || true
	@[ -f "$(BUILD_DIR)/libssl.3.dylib" ] && cp "$(BUILD_DIR)/libssl.3.dylib" "$(APP_DIR)/Contents/Resources/" || true
	@[ -f "$(BUILD_DIR)/libcrypto.3.dylib" ] && cp "$(BUILD_DIR)/libcrypto.3.dylib" "$(APP_DIR)/Contents/Resources/" || true
	@# Ollama
	@[ -f "$(BUILD_DIR)/ollama" ] && cp "$(BUILD_DIR)/ollama" "$(APP_DIR)/Contents/Resources/" || true
	@# Resources & metadata
	@cp "$(BUILD_DIR)/appicon.icns" "$(APP_DIR)/Contents/Resources/" 2>/dev/null || true
	@sed 's/0\.1\.0/$(VERSION)/g' "$(BUILD_DIR)/Info.plist" > "$(APP_DIR)/Contents/Info.plist"
	@cp $(BUILD_DIR)/launchd/*.plist "$(APP_DIR)/Contents/Resources/launchd/" 2>/dev/null || true
	@# Permissions
	@chmod +x "$(APP_DIR)/Contents/MacOS/Hippocampus"
	@chmod +x "$(APP_DIR)/Contents/Resources/"hippocampus-* 2>/dev/null || true
	@chmod +x "$(APP_DIR)/Contents/Resources/redis-server" 2>/dev/null || true
	@chmod +x "$(APP_DIR)/Contents/Resources/ollama" 2>/dev/null || true
	@# Fix dylib load paths
	@$(ROOT_DIR)/scripts/fix-dylib-paths.sh "$(APP_DIR)/Contents/Resources"
	@# Code signing
	@echo "Signing..."
	@$(ROOT_DIR)/scripts/codesign-bundle.sh "$(APP_DIR)" "$(BUNDLE_ID)" "$(SIGN_IDENTITY)"
	@echo ""
	@echo "Done! → dist/Hippocampus.app"
	@echo "  Size: $$(du -sh "$(APP_DIR)" | cut -f1)"
	@echo ""

# --- Install ---

install: bundle  ## Install to /Applications (macOS)
ifneq ($(PLATFORM),macos)
	$(error install target is macOS only)
endif
	@echo "Installing to /Applications..."
	@rm -rf /Applications/Hippocampus.app
	@cp -r "$(APP_DIR)" /Applications/
	@echo "Installed."

install-bin: binaries  ## Install binaries to /usr/local/bin (any platform)
	@echo "Installing binaries to /usr/local/bin..."
	@install -m 755 $(BIN_DIR)/hippocampus-mcp /usr/local/bin/
	@install -m 755 $(BIN_DIR)/hippocampus-hook /usr/local/bin/
	@install -m 755 $(BIN_DIR)/hippocampus-daemon /usr/local/bin/
	@install -m 755 $(BIN_DIR)/hippocampus-summarize /usr/local/bin/
	@install -m 755 $(BIN_DIR)/hippocampus-admin /usr/local/bin/
	@install -m 755 $(BIN_DIR)/hippocampus-slack /usr/local/bin/
	@install -m 755 $(BIN_DIR)/hippocampus-ingest /usr/local/bin/
	@echo "Installed 7 binaries."

# --- Release ---

.PHONY: release export

release: bundle  ## Create signed DMG (macOS) — set CODESIGN_IDENTITY for notarization
ifneq ($(PLATFORM),macos)
	$(error DMG release is macOS only.)
endif
	@$(ROOT_DIR)/release.sh

export:  ## Sync to export repo and push to GitHub — Usage: make export VERSION=1.0.2
ifndef VERSION
	$(error Usage: make export VERSION=1.0.2)
endif
	@$(ROOT_DIR)/export-release.sh $(VERSION)

# --- Clean ---

clean:  ## Remove build artifacts (keep deps)
	@rm -rf $(BIN_DIR) $(DIST_DIR)
	@echo "Cleaned binaries and dist."

distclean: clean  ## Remove everything (including downloaded deps)
	@rm -f $(BUILD_DIR)/redis-server $(BUILD_DIR)/ollama
	@echo "Cleaned all."

# --- Testing ---

.PHONY: test lint vet

test:  ## Run tests
	@go test ./...

lint:  ## Run staticcheck
	@staticcheck ./...

vet:  ## Run go vet
	@go vet ./...
