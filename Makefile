BIN     := clonecast
PKG     := ./cmd/clonecast
LDFLAGS := -s -w

.PHONY: build linux agent test vet fmt run-mock clean

build:            ## build for the host OS
	go build -ldflags '$(LDFLAGS)' -o bin/$(BIN) $(PKG)

linux:            ## static linux/amd64 binary for Bazzite
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o bin/$(BIN)-linux-amd64 $(PKG)

agent:            ## in-bottle agent (windows/386) for `clonecast agent install`
# -H windowsgui: without it the agent is a CONSOLE binary, so Wine gives it a
# console WINDOW on the desktop — one per prefix, each sitting there saying it
# is waiting for the game window. The source has always assumed it has no
# console (hence the log file beside the exe); the build did not say so.
	GOOS=windows GOARCH=386 CGO_ENABLED=0 go build -ldflags '$(LDFLAGS) -H windowsgui' -o bin/clonecast-agent.exe ./cmd/clonecast-agent

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
