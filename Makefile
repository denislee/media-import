# media-import — native Wayland (Gio) media importer
#
#   make            build ./build/media-import (only rebuilds when sources change)
#   make run        build and launch the GUI
#   make test       run unit tests (with the race detector)
#   make install    install the last compiled binary + desktop entry + icon
#                   (default PREFIX=~/.local, no root needed;
#                    system-wide: sudo make install PREFIX=/usr/local)
#   make uninstall  remove installed files

APP      := media-import
PKG      := ./cmd/$(APP)
BUILDDIR := build
BIN      := $(BUILDDIR)/$(APP)

# Gio build tags: Wayland-only, OpenGL/EGL renderer. If you install
# vulkan-headers you may drop "novulkan"; drop "nox11" to also support X11.
TAGS     ?= nox11 novulkan

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS  ?= -trimpath

PREFIX   ?= $(HOME)/.local
DESTDIR  ?=
BINDIR   := $(DESTDIR)$(PREFIX)/bin
APPDIR   := $(DESTDIR)$(PREFIX)/share/applications
ICONDIR  := $(DESTDIR)$(PREFIX)/share/icons/hicolor/scalable/apps

GO       ?= go
SOURCES  := $(shell find . -name '*.go' -not -path './$(BUILDDIR)/*') go.mod go.sum

.DEFAULT_GOAL := build
.PHONY: build run dev test test-v cover vet fmt fmt-check check install uninstall clean deps help

build: $(BIN) ## Build the binary (incremental)

$(BIN): $(SOURCES)
	@mkdir -p $(BUILDDIR)
	$(GO) build $(GOFLAGS) -tags '$(TAGS)' -ldflags '$(LDFLAGS)' -o $@ $(PKG)
	@echo "built $@ ($(VERSION))"

run: $(BIN) ## Build (if needed) and launch the GUI
	./$(BIN)

dev: ## Run straight from source with go run
	$(GO) run -tags '$(TAGS)' $(PKG)

test: ## Run unit tests with the race detector
	$(GO) test -race -tags '$(TAGS)' ./...

test-v: ## Verbose tests
	$(GO) test -race -v -tags '$(TAGS)' ./...

cover: ## Tests with coverage report (build/coverage.html)
	@mkdir -p $(BUILDDIR)
	$(GO) test -tags '$(TAGS)' -coverprofile=$(BUILDDIR)/coverage.out ./...
	$(GO) tool cover -func=$(BUILDDIR)/coverage.out | tail -1
	$(GO) tool cover -html=$(BUILDDIR)/coverage.out -o $(BUILDDIR)/coverage.html

vet: ## go vet
	$(GO) vet -tags '$(TAGS)' ./...

fmt: ## Format sources
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "needs gofmt:"; gofmt -l .; exit 1; }

check: fmt-check vet test ## fmt-check + vet + test

install: $(BIN) ## Install the last compiled binary (rebuilds only if sources changed)
	install -Dm755 $(BIN) $(BINDIR)/$(APP)
	install -d $(APPDIR) $(ICONDIR)
	sed 's|@BINDIR@|$(PREFIX)/bin|g' assets/$(APP).desktop > $(APPDIR)/$(APP).desktop
	chmod 644 $(APPDIR)/$(APP).desktop
	install -m644 assets/$(APP).svg $(ICONDIR)/$(APP).svg
	-@command -v update-desktop-database >/dev/null && update-desktop-database -q $(APPDIR) || true
	-@command -v gtk-update-icon-cache >/dev/null && gtk-update-icon-cache -q -t $(DESTDIR)$(PREFIX)/share/icons/hicolor 2>/dev/null || true
	@echo "installed $(VERSION) to $(BINDIR)/$(APP)"

uninstall: ## Remove installed files
	rm -f $(BINDIR)/$(APP) $(APPDIR)/$(APP).desktop $(ICONDIR)/$(APP).svg

clean: ## Remove build output
	rm -rf $(BUILDDIR)

deps: ## Check build/runtime dependencies (Arch package names)
	@for p in wayland libxkbcommon mesa; do pacman -Q $$p >/dev/null 2>&1 && echo "ok   $$p" || echo "MISSING $$p  (sudo pacman -S $$p)"; done
	@command -v ffprobe >/dev/null && echo "ok   ffprobe (optional)" || echo "opt  ffmpeg not installed: ffprobe date detection disabled"

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-11s %s\n", $$1, $$2}'
