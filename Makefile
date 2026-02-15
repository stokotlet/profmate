.PHONY: build test

build:
	go build -trimpath -o bin/profmate ./cmd/profmate

test:
	go test ./...
