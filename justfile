default:
    @just --list

build:
    go build -o yawn -ldflags "-s -w -X main.version={{ `git describe --tags --always --dirty 2>/dev/null || echo dev` }}" ./cmd/yawn

test:
    go test ./...

test-race:
    CGO_ENABLED=1 go test -race ./...

lint:
    gofmt -l ./cmd ./internal | tee /dev/stderr | (! read)
    go vet ./...

fmt:
    gofmt -w ./cmd ./internal

check: lint test build

clean:
    rm -f yawn yawn.exe
    rm -rf dist

tidy:
    go mod tidy
