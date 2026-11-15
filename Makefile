BIN := agy-acp-muzzle

.PHONY: build test vet fmt

build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/agy-acp-muzzle

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .
