VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
export CGO_ENABLED := 0

.PHONY: all test server agent clean

all: server agent

test:
	go vet ./...
	go test ./...

server:
	go build -trimpath -ldflags "-s -w" -o bin/server ./cmd/server

agent:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/uptime-battle-agent ./cmd/agent

clean:
	rm -rf bin
