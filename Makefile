.PHONY: build test lint

build:
	go build -o bin/careless-whisper ./cmd/careless-whisper

test:
	go test ./...

lint:
	go vet ./...
