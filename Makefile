GO_FILES = $(shell git ls-files '*.go')

.PHONY: all build run test race vet lint fmt golden luals stylua check install clean reset-db

LUALS ?= lua-language-server
STYLUA ?= stylua
GOLANGCI ?= golangci-lint

all: check build

build:
	go build -o bin/zemi .

run:
	go run .

test:
	go test ./...

race:
	go test -race ./internal/...

vet:
	go vet ./...

# golangci-lint with .golangci.yml; skipped without the binary
lint:
	@if command -v $(GOLANGCI) >/dev/null; then $(GOLANGCI) run ./...; \
	else echo "lint: $(GOLANGCI) not installed, skipped (GOLANGCI=/path/to/golangci-lint to point at one)"; fi

fmt:
	gofmt -w $(GO_FILES)
	@if command -v $(STYLUA) >/dev/null; then $(STYLUA) runtime; fi

# regenerate internal/ui/testdata/*.golden, then review the diff
golden:
	go test ./internal/ui -update

# type-check runtime/ Lua against runtime/types; skipped without the binary
luals:
	@if command -v $(LUALS) >/dev/null; then \
		tmp=$$(mktemp -d); \
		$(LUALS) --check=$(CURDIR)/runtime --configpath=$(CURDIR)/runtime/.luarc.json --checklevel=Warning \
			--check_format=pretty --logpath=$$tmp --metapath=$$tmp/meta; \
		st=$$?; rm -rf $$tmp; exit $$st; \
	else echo "luals: $(LUALS) not installed, skipped (LUALS=/path/to/lua-language-server to point at one)"; fi

# format check for runtime/ Lua; skipped without the binary
stylua:
	@if command -v $(STYLUA) >/dev/null; then $(STYLUA) --check runtime; \
	else echo "stylua: $(STYLUA) not installed, skipped (STYLUA=/path/to/stylua to point at one)"; fi

check: vet test luals stylua lint
	@out=$$(gofmt -l $(GO_FILES)); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

install:
	go install .

# a schema bump refuses the old stores; there are no migrations before the
# first release, so this drops them (sessions, memories, reflector state)
ZEMI_STATE ?= $(or $(ZEMI_STATE_HOME),$(or $(XDG_STATE_HOME),$(HOME)/.local/state)/zemi)
reset-db:
	rm -f $(ZEMI_STATE)/zemi.db* $(ZEMI_STATE)/projects/*/zemi.db*

clean:
	rm -rf bin
