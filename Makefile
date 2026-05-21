.PHONY: build test lint vet docker clean

VERSION ?= dev
BIN := bin/agent-opsd

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/agent-opsd

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run --timeout=5m

docker:
	docker build -t agent-ops:$(VERSION) --build-arg VERSION=$(VERSION) .

clean:
	rm -rf bin dist
