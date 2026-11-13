VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test lint clean tidy

build:
	go build -ldflags "$(LDFLAGS)" -o bin/mbx ./cmd/mbx

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/mbx

test:
	go test -race -cover ./...

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/
