VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vuln install clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o rintel ./cmd/rintel

install:
	go install -ldflags "-X main.version=$(VERSION)" ./cmd/rintel

test:
	go vet ./...
	go test ./...

vuln: # dependency policy: this must print "No vulnerabilities found."
	govulncheck ./...

clean:
	rm -f rintel
