.PHONY: build test lint vet docker clean

VERSION ?= dev
BIN_DIR := bin

build:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/agent-opsd ./cmd/agent-opsd
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/agent-ops ./cmd/agent-ops

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run --timeout=5m

docker:
	docker build -t agent-ops:$(VERSION) --build-arg VERSION=$(VERSION) .

clean:
	rm -rf $(BIN_DIR) dist
