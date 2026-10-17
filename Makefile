GO_FILES = $(shell git ls-files -co --exclude-standard '*.go')

.PHONY: all build run site first-run mcp-auth test race vet lint fmt golden luals stylua check install clean reset-db

LUALS ?= lua-language-server
STYLUA ?= stylua
GOLANGCI ?= golangci-lint

all: check build

build:
	go build -o bin/zemi .

run:
	go run .

# the site (docs/): plain files, served; opening docs/index.html as a file
# blocks its module scripts and shows a still frame instead
site:
	python3 -m http.server 1314 --directory docs

# regenerate docs/og.png: a 1200x630 shot of the page itself
CHROME ?= /Applications/Google Chrome.app/Contents/MacOS/Google Chrome
og:
	python3 -m http.server 1315 --directory docs & pid=$$!; sleep 0.5; \
	"$(CHROME)" --headless=new --disable-gpu --hide-scrollbars --window-size=1200,630 \
		--virtual-time-budget=3600 --screenshot=docs/og.png http://localhost:1315/; \
	kill $$pid

# the MCP sign-in callback page (internal/mcp/callback.html) in a browser,
# ok and failed states, no provider; /fail?reason=... tries other reasons
mcp-auth:
	go run ./internal/mcp/callback_preview.go

# zemi as a newcomer sees it: a fresh HOME (no ~/.agents/skills, no zemi
# dirs) and a new git repo, or DIR. Provider keys pass through unless
# KEYS=0; ARGS are zemi's flags. The tree is deleted on exit.
# NOTE: your real zemi dirs sit outside this HOME; the run's own commands
# can read whatever HOME holds during the run.
first-run: build
	@t=$$(mktemp -d); trap 'chmod -R u+w "$$t"; rm -rf "$$t"' EXIT; \
	mkdir "$$t/project" && git -C "$$t/project" init -q; \
	if [ "$(KEYS)" = 0 ]; then for v in $$(env | sed -n 's/^\([A-Za-z0-9_]*\)=.*/\1/p'); do \
		case $$v in *_API_KEY|*_TOKEN|*_SECRET_KEY) unset $$v;; esac; done; fi; \
	cd "$(or $(DIR),$$t/project)" && env -u XDG_CONFIG_HOME -u XDG_DATA_HOME -u XDG_STATE_HOME -u XDG_CACHE_HOME \
		-u ZEMI_CONFIG_HOME -u ZEMI_DATA_HOME -u ZEMI_STATE_HOME -u ZEMI_CACHE_HOME HOME="$$t" "$(CURDIR)/bin/zemi" $(ARGS)

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

# golangci-lint with .golangci.yml; skipped without the binary
lint:
	@if command -v $(GOLANGCI) >/dev/null; then $(GOLANGCI) run ./...; \
	else echo "lint: $(GOLANGCI) not installed, skipped (GOLANGCI=/path/to/golangci-lint to point at one)"; fi

# golangci-lint's formatters (gofumpt, goimports, gci); gofmt alone without it
fmt:
	@if command -v $(GOLANGCI) >/dev/null; then $(GOLANGCI) fmt ./...; else gofmt -w $(GO_FILES); fi
	@if command -v $(STYLUA) >/dev/null; then $(STYLUA) runtime testdata; fi

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

# format check for the Lua in runtime/ and testdata/; skipped without the binary
stylua:
	@if command -v $(STYLUA) >/dev/null; then $(STYLUA) --check runtime testdata; \
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
