PREFIX ?= $(HOME)/.local

.PHONY: build install hooks skill test

build:
	go build -o bin/tower .

install: build
	mkdir -p $(PREFIX)/bin
	ln -sf $(CURDIR)/bin/tower $(PREFIX)/bin/tower

hooks: install
	scripts/install-hooks.sh $(PREFIX)/bin/tower

skill:
	ln -sfn $(CURDIR)/skill $(HOME)/.claude/skills/tower

test:
	go vet ./... && go test ./...
