.PHONY: build test itest clean

VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
PUBKEY  ?= $(shell cat keys/catalog.pub 2>/dev/null)

build:
	go build -ldflags "-X main.version=$(VERSION) -X github.com/janit/viiwork-parrot/internal/catalog.DefaultPubKey=$(PUBKEY)" -o bin/viiwork-parrot ./cmd/viiwork-parrot

test:
	go test ./...

itest:
	go test -tags=integration -count=1 -v ./test/...

clean:
	rm -rf bin/ dist/
