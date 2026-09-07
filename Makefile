BIN     := clonecast
PKG     := ./cmd/clonecast
LDFLAGS := -s -w

.PHONY: build linux test vet fmt run-mock clean

build:            ## build for the host OS
	go build -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

linux:            ## static linux/amd64 binary for Bazzite
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o bin/$(BIN)-linux-amd64 $(PKG)

test:
	go test ./...

vet:              ## vet for host and linux
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...

fmt:
	gofmt -w ./cmd ./internal

run-mock:         ## run the TUI with fake windows and keys
	go run $(PKG) --backend mock

clean:
	rm -rf bin
