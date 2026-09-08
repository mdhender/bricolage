# Project Guidance

## Project context

- This repository is the current Go implementation of Bricolage.
- The original Perl implementation is hosted at
  [github.com/bricoleurs/bricolage](https://github.com/bricoleurs/bricolage).
  Its last official release was 2.0.1. The 2.1.0 code in that repository was
  unfinished work toward a planned 2.2.0 release before development was
  abandoned; do not treat it as an official release.
- The [bricoleurs organization](https://github.com/bricoleurs) contains other
  repositories related to Bricolage CMS. Use those repositories as historical
  and domain references when needed; make changes for the current Go
  implementation in this repository.
- The Go module is `github.com/mdhender/bricolage` and targets Go 1.20.

## Repository layout

- `cmd/bricolage/` contains the executable entry point.
- `cli/` owns Cobra commands, flags, and command-line configuration.
- `config.go` defines shared application configuration and defaults.
- `pkg/server/` owns HTTP server construction, options, and routes.
- `pkg/way/` contains the attributed HTTP router implementation. Preserve its
  attribution and licensing when changing it.
- `testdata/` is reserved for test fixtures.

Keep behavior in the package that owns it. Avoid putting application logic in
the executable entry point or adding wrappers when an existing package is the
natural owner.

## Development workflow

Use standard Go tooling from the repository root:

```sh
go build ./cmd/bricolage
go test ./...
go vet ./...
```

- Format changed Go files with `gofmt`.
- Add or update focused tests when changing behavior. Run `go test ./...` before
  finishing.
- Run `go vet ./...` for changes that affect Go code across package boundaries.
- Do not edit `go.sum` manually; update dependencies with Go module commands.

## Code conventions

- Follow idiomatic Go and the existing package naming and functional-option
  patterns.
- Keep changes small and avoid introducing abstractions until they have a clear
  package-level responsibility or more than one real use.
- Propagate errors to the layer that can handle them; reserve process exits and
  fatal logging for the CLI boundary.
- Existing Go source files carry the project copyright and MIT license header.
  Preserve it when editing files and include it in new Go source files.
