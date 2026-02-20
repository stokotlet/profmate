.PHONY: build test race vet demo load

build:
	go build -trimpath -o bin/profmate ./cmd/profmate

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

demo:
	go run ./cmd/demo

load:
	go run ./cmd/load -duration 45s
