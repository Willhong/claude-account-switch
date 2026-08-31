BINARY  := cas
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local
LDFLAGS := -s -w -X github.com/Willhong/claude-account-switch/internal/app.Version=$(VERSION)

.PHONY: all build install uninstall test vet fmt check clean

all: build

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/cas

install: build
	@mkdir -p $(PREFIX)/bin
	install -m 0755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)
	@echo "installed $(PREFIX)/bin/$(BINARY) ($(VERSION))"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; \
	  *) echo "note: $(PREFIX)/bin is not on your PATH";; esac

uninstall:
	@$(PREFIX)/bin/$(BINARY) daemon uninstall 2>/dev/null || true
	rm -f $(PREFIX)/bin/$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: fmt vet test

clean:
	rm -rf bin
