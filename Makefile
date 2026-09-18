GO_FILES = $(shell git ls-files '*.go')

.PHONY: all build run test race vet fmt golden luals check install clean

LUALS ?= lua-language-server

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

fmt:
	gofmt -w $(GO_FILES)

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

check: vet test luals
	@out=$$(gofmt -l $(GO_FILES)); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

install:
	go install .

clean:
	rm -rf bin
