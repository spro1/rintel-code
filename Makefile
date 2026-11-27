VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vuln notices install clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o rintel ./cmd/rintel

install:
	go install -ldflags "-X main.version=$(VERSION)" ./cmd/rintel

test:
	go vet ./...
	go test ./...

vuln: # dependency policy: this must print "No vulnerabilities found."
	govulncheck ./...

# The licence texts of every module compiled into the binary on any supported system (the set differs per system); MIT and BSD ask for them wherever a binary is shipped.
# Run after changing a dependency. CI fails when the file is out of date.
notices:
	@{ echo "rintel is built with the Go modules below. Their licence texts follow, as those licences require."; \
	  echo "The bundled TypeScript compiler is covered in NOTICE."; \
	  for os in darwin linux windows; do \
	    GOOS=$$os go list -deps -f '{{with .Module}}{{if .Version}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/rintel; \
	  done | sort -u | \
	  while read path version dir; do \
	    for f in $$(ls "$$dir" | grep -iE '^(LICEN[SC]E|COPYING|NOTICE)' | sort); do \
	      printf '\n%s\n%s %s: %s\n%s\n\n' "================================================================================" "$$path" "$$version" "$$f" "================================================================================"; \
	      cat "$$dir/$$f"; \
	    done; \
	  done; } > THIRD_PARTY_NOTICES

clean:
	rm -f rintel
