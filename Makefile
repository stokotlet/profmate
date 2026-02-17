.PHONY: build test demo

build:
	go build -trimpath -o bin/profmate ./cmd/profmate

test:
	go test ./...

demo:
	go run ./cmd/demo
