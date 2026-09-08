# gh-mass-clone

BIN     := gh-mass-clone
PREFIX  ?= $(HOME)/.local
BINDIR  := $(DESTDIR)$(PREFIX)/bin

# `git describe` if this is a tagged checkout, else the short sha, else "dev"
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# platforms built by `make dist`
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: all build install uninstall clean check test dist help

all: build

build: $(BIN)

$(BIN): main.go go.mod
	go build -trimpath -ldflags '$(LDFLAGS)' -o $@ .

install: build
	@install -d $(BINDIR)
	install -m 0755 $(BIN) $(BINDIR)/$(BIN)
	@echo "installed $(BINDIR)/$(BIN) ($(VERSION))"
	@case ":$$PATH:" in \
		*":$(PREFIX)/bin:"*) ;; \
		*) echo "note: $(PREFIX)/bin is not on your PATH" ;; \
	esac

uninstall:
	rm -f $(BINDIR)/$(BIN)
	@echo "removed $(BINDIR)/$(BIN)"

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...

test: check
	go test ./...

# cross-compiled binaries for a GitHub release; never commit these
dist: clean
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BIN)-$$os-$$arch . || exit 1; \
	done
	@echo "dist/ built ($(VERSION))"

clean:
	rm -rf $(BIN) dist

help:
	@echo "make build      build ./$(BIN)"
	@echo "make install    install to \$$PREFIX/bin (default $(HOME)/.local/bin)"
	@echo "make uninstall  remove it again"
	@echo "make check      gofmt + go vet"
	@echo "make dist       cross-compile into dist/ for a release"
	@echo "make clean      remove build output"
	@echo
	@echo "override the install location with:  make install PREFIX=/usr/local"
