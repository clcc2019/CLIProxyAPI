# Makefile for CLIProxyAPI — size-optimized build profiles.
#
# Targets:
#   make              -> dev build (default, 82 MB, debuggable)
#   make slim         -> strip + trim path (~60 MB, no debug)
#   make minimal      -> slim + exclude optional backends (~40 MB)
#   make compressed   -> minimal + UPX (~20 MB, same runtime behaviour)
#   make all-variants -> build every target with a distinct output name
#
# Build tags (composable):
#   has_postgres   include Postgres token store (pgx + ugorji)
#   has_minio      include Minio/S3 object store
#   has_git        include go-git gitstore
#   no_redis       exclude Redis state persistence from explicit minimal builds
#   has_tui        include bubbletea / lipgloss dashboard
#
# Common combinations:
#   tags=has_postgres              Postgres + default Redis, no Git/Minio/TUI
#   tags=has_tui                   CLI dashboard + default Redis
#   tags=no_redis                  explicitly disable Redis for smallest builds
#
# Version metadata is injected via -X ldflags from git state.

BINARY      := CLIProxyAPI
MAIN_PKG    := ./cmd/server
GO          ?= go
GOFLAGS     ?=
UPX         ?= upx

VERSION     := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Inject version + strip symbols. `-trimpath` strips the build machine's
# absolute path from the binary (useful for reproducible builds and also
# saves a few hundred KB).
VERSION_LDFLAGS := -X 'main.Version=$(VERSION)' -X 'main.Commit=$(COMMIT)' -X 'main.BuildDate=$(BUILD_DATE)'
STRIP_LDFLAGS   := -s -w
TRIMPATH        := -trimpath

# Full backend tags for slim builds. Add/remove members via `TAGS=` override:
#   make minimal TAGS=has_postgres
TAGS          ?= has_postgres,has_minio,has_git,has_tui
MINIMAL_TAGS  ?= no_redis

.PHONY: help dev slim minimal compressed all-variants clean check-upx

help:
	@echo "Targets:"
	@echo "  make dev         Dev build (~82 MB, debuggable)"
	@echo "  make slim        Stripped build (~60 MB, release)"
	@echo "  make minimal     Stripped + no optional backends (~40 MB)"
	@echo "  make compressed  minimal + UPX (~20 MB, needs upx)"
	@echo ""
	@echo "Override tags with TAGS=has_postgres,has_tui or MINIMAL_TAGS=no_redis etc."

# Default: a plain debuggable build — includes every optional backend so
# developers have the full feature surface locally. Release builds should
# pick `slim` / `minimal` / `compressed` based on their needs.
dev:
	$(GO) build $(GOFLAGS) -tags=$(TAGS) -o $(BINARY) -ldflags="$(VERSION_LDFLAGS)" $(MAIN_PKG)
	@echo "Built $(BINARY) (dev):"; ls -lh $(BINARY) | awk '{print "  size:", $$5}'

# Production build: strip symbols, trim paths, include every optional backend.
slim:
	$(GO) build $(GOFLAGS) $(TRIMPATH) -tags=$(TAGS) \
		-o $(BINARY) -ldflags="$(STRIP_LDFLAGS) $(VERSION_LDFLAGS)" $(MAIN_PKG)
	@echo "Built $(BINARY) (slim):"; ls -lh $(BINARY) | awk '{print "  size:", $$5}'

# Minimal: strip + no optional backends. Produces the smallest uncompressed
# binary. Good for single-model proxies that don't need remote storage.
minimal:
	$(GO) build $(GOFLAGS) $(TRIMPATH) -tags='$(MINIMAL_TAGS)' \
		-o $(BINARY) -ldflags="$(STRIP_LDFLAGS) $(VERSION_LDFLAGS)" $(MAIN_PKG)
	@echo "Built $(BINARY) (minimal):"; ls -lh $(BINARY) | awk '{print "  size:", $$5}'

# UPX-compressed minimal build. Expect ~70% size reduction; startup takes
# ~20ms longer on first invocation because UPX decompresses in-memory.
compressed: check-upx minimal
	$(UPX) --best --lzma -q $(BINARY) >/dev/null
	@echo "Compressed $(BINARY):"; ls -lh $(BINARY) | awk '{print "  size:", $$5}'

# Build every variant side-by-side for size comparison.
all-variants:
	$(GO) build $(GOFLAGS) -tags=$(TAGS) -o $(BINARY)-dev -ldflags="$(VERSION_LDFLAGS)" $(MAIN_PKG)
	$(GO) build $(GOFLAGS) $(TRIMPATH) -tags=$(TAGS) \
		-o $(BINARY)-slim -ldflags="$(STRIP_LDFLAGS) $(VERSION_LDFLAGS)" $(MAIN_PKG)
	$(GO) build $(GOFLAGS) $(TRIMPATH) -tags='$(MINIMAL_TAGS)' \
		-o $(BINARY)-minimal -ldflags="$(STRIP_LDFLAGS) $(VERSION_LDFLAGS)" $(MAIN_PKG)
	@echo "Sizes:"; ls -lh $(BINARY)-dev $(BINARY)-slim $(BINARY)-minimal 2>/dev/null | awk '{printf "  %-40s %s\n", $$NF, $$5}'

check-upx:
	@command -v $(UPX) >/dev/null 2>&1 || { \
		echo "upx not found. Install with: brew install upx (macOS) or apt install upx (Linux)"; \
		exit 1; \
	}

clean:
	rm -f $(BINARY) $(BINARY)-dev $(BINARY)-slim $(BINARY)-minimal
