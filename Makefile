.PHONY: build test install

build:
	go build -o bin/muninn ./cmd/muninn

test:
	go test -race ./...

install:
	go install ./cmd/muninn
