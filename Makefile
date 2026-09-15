GO_FILES = $(shell git ls-files '*.go')

.PHONY: all build run test race vet fmt golden check install clean

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

check: vet test
	@out=$$(gofmt -l $(GO_FILES)); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

install:
	go install .

clean:
	rm -rf bin
