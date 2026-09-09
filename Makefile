# Copyright (c) 2026 Michael D Henderson.
#
# The development gate. "make check" is what CI runs and what you run before
# opening a PR; everything else here is a piece of it, or the release recipe
# nobody should reach for by accident.

GO      ?= go
COMMANDS := cmsdb cmsd earl
RELEASE_DIR := deploy/linux/amd64

# The local database directory. It is git-ignored, and no target creates it:
# --db names a directory that must already exist (invariant 19).
DEV_DB  ?= ./var

.DEFAULT_GOAL := check
.PHONY: check build test race vet fmt lint no-mkdir no-dev-routes one-state-writer tagged dev init seed bootstrap status release clean help

## check: the full gate. Run this before opening a PR.
check: fmt vet lint build test race tagged
	@echo "check: ok"

## build: compile everything, including the three commands.
build:
	$(GO) build ./...

## test: run the tests.
test:
	$(GO) test ./...

## race: run the tests under the race detector.
race:
	$(GO) test -race ./...

## tagged: run the production half of the build/environment interlock.
##
## The tag gates no routes and no features; it exists so that a release binary
## refuses to run anywhere but production (DESIGN.md 14). Half of that
## behaviour only compiles under the tag, so half of its test only runs here.
tagged:
	$(GO) test -tags production ./internal/buildenv/

## vet: go vet.
vet:
	$(GO) vet ./...

## fmt: fail if anything is unformatted. It does not rewrite; the diff is
## yours to make.
fmt:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: these files need formatting:"; echo "$$unformatted"; exit 1; \
	fi

## lint: the greps that catch the rules nobody notices breaking.
lint: no-mkdir no-dev-routes one-state-writer

## no-mkdir: nothing in this system creates a directory (invariant 19).
##
## --db names a directory that must already exist. A tool that creates what it
## cannot find turns a typo into a plausible-looking, empty system, and the
## mistake surfaces hours later as "where did everything go".
no-mkdir:
	@if grep -rn 'os\.MkdirAll\|os\.Mkdir(' ./cmd ./internal; then \
		echo "lint: nothing may create a directory (invariant 19, DESIGN.md 13.1)"; exit 1; \
	fi

## no-dev-routes: the /__development/* routes are registered only when the
## resolved environment is exactly "development" (invariant 16).
##
## The grep catches a second switch being introduced; the test catches the gate
## being wrong. Both are cheap and neither replaces the other.
no-dev-routes:
	@if grep -rn 'Handle\(Func\)\?(.*__development' ./cmd ./internal --include='*.go' \
		| grep -v '^\./internal/web/devroutes/'; then \
		echo "lint: only internal/web/devroutes may register a /__development/ pattern (invariant 16)"; exit 1; \
	fi
	@callers=$$(grep -rl 'devroutes\.Register' ./cmd ./internal --include='*.go' | grep -v '_test\.go$$'); \
	if [ "$$callers" != "./internal/server/routes.go" ]; then \
		echo "lint: devroutes.Register has one call site, gated on the resolved environment; found:"; \
		echo "$$callers"; exit 1; \
	fi
	$(GO) test -run 'TestDevRoutes|TestRouteTable' ./internal/server/ ./internal/web/devroutes/

## one-state-writer: internal/workflow is the only writer of documents.state
## (invariant 4), and all SQL lives in internal/store (invariant 2).
##
## Both hold at once because the one statement that writes the column is inside
## store.ApplyTransition, which cannot run without the decision function the
## engine hands it. The first grep finds a second statement; the second finds a
## second caller. Neither replaces the test, which walks the tree and also
## covers the INSERT that places a new document.
one-state-writer:
	@if grep -rnE 'SET[[:space:]]+state[[:space:]]*=' ./cmd ./internal --include='*.go' \
		| grep -v '_test\.go:' | grep -v '^\./internal/store/workflow\.go:'; then \
		echo "lint: documents.state is written by one statement, in internal/store/workflow.go (invariants 2 and 4)"; exit 1; \
	fi
	@callers=$$(grep -rl 'ApplyTransition' ./cmd ./internal --include='*.go' \
		| grep -v '_test\.go$$' | grep -v '^\./internal/store/'); \
	if [ "$$callers" != "./internal/workflow/engine.go" ]; then \
		echo "lint: ApplyTransition has one caller, in internal/workflow (invariant 4, DESIGN.md 6.3); found:"; \
		echo "$$callers"; exit 1; \
	fi
	$(GO) test -run 'TestOnlyOneStatementWritesDocumentState|TestApplyTransitionHasOneCaller' ./internal/workflow/

## dev: run cmsd in development, behind the Caddy service, against ./var.
##
## This target does NOT start Caddy. Caddy is a machine-wide Homebrew service
## reading /opt/homebrew/etc/Caddyfile; running it yourself mints a second CA
## root with the same subject name and breaks HTTPS to *.localhost at random.
## If it is not started, ask a human: brew services start caddy
##
## It does NOT create $(DEV_DB) either, and it must not: --db names a directory
## that has to exist already, and nothing in this system creates one
## (invariant 19). "mkdir var && make init" is the first-run recipe.
dev:
	@brew services list 2>/dev/null | grep -q '^caddy.*started' \
		|| echo "warning: the Caddy service does not look started; ask a human for 'brew services start caddy'"
	@test -d $(DEV_DB) || { \
		echo "$(DEV_DB) does not exist. Create it yourself, then run 'make init':"; \
		echo "    mkdir $(DEV_DB) && make init"; exit 1; }
	$(GO) run ./cmd/cmsd serve --db $(DEV_DB) --env development --timeout 60m

## init: create $(DEV_DB)/cms.db and apply every migration.
##
## The directory is yours to create; this only fills it. The first-run recipe
## is "mkdir var && make init seed bootstrap".
init:
	$(GO) run ./cmd/cmsdb init --db $(DEV_DB)

## seed: create the default roles, their grants, and one site.
##
## Run it before "make bootstrap": the admin role is seeded and bootstrap
## assigns it.
seed:
	$(GO) run ./cmd/cmsdb seed --db $(DEV_DB)

## bootstrap: create the first administrator and print a password once.
##
## The password is generated and shown exactly once; nothing stores it. It is
## never a flag, because arguments are visible in "ps" and land in shell
## history. Override EMAIL and NAME to use your own.
EMAIL ?= admin@example.com
NAME  ?= Admin
bootstrap:
	$(GO) run ./cmd/cmsdb bootstrap admin --db $(DEV_DB) --email $(EMAIL) --name "$(NAME)"

## status: show the schema version and the applied and pending migrations.
status:
	$(GO) run ./cmd/cmsdb migrate status --db $(DEV_DB)

## release: cross-compile the three commands for the server.
##
## Never run this as a side effect of another task, and never deploy the
## result yourself. If a change needs shipping, say so and let a human run it.
## The tag adds one thing: buildenv.Verify panics unless CMS_ENV=production is
## exported. It gates no routes and no features.
release:
	@# The one mkdir in this repository, and it is build output, not data.
	@# Nothing in the Go code creates a directory (invariant 19).
	@mkdir -p $(RELEASE_DIR)
	@for c in $(COMMANDS); do \
		echo "building $$c"; \
		GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
			$(GO) build -tags production -trimpath -o $(RELEASE_DIR)/$$c ./cmd/$$c || exit 1; \
	done
	@echo "release: $(RELEASE_DIR)"

## clean: remove the release output.
clean:
	rm -rf $(RELEASE_DIR)

## help: list the targets.
help:
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed 's/^## /  /'
