.PHONY: build test race vet run

build:
	CGO_ENABLED=0 go build -trimpath -o gateway ./cmd/gateway

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

run:
	go run ./cmd/gateway
