# land: rules for agents

land lands an agent's work on a repository's trunk. The design and its reasons are in `docs/DESIGN.md`, the terms in `GLOSSARY.md`; use them.

## Layout

- `main.go`: subcommand dispatch. `internal/<module>/`: one package per module (landing, check, verdict, classify, boxes, review, autoreview, provenance, forge, guard, docs, config, stage, proc).
- `internal/docs/LANDING.md`: the rules every adopting repository's agents follow, written there by `land docs`. land refuses a repository's copy that differs from its own, so a change here changes every repository's rules at its next upgrade.
- `internal/autoreview/upstream/`: the bundled reviewer at a pinned upstream commit. Never edit it by hand; move the pin in a release.
- `shim/land`: what each adopting repository copies to `scripts/land`.

## Commands

- `go run . <subcommand> …`: run from source.
- Before every commit: `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass.
- `go test -tags crabboxreal ./internal/boxes/`: runs the real crabbox (macOS, Docker). It must pass on a crabbox version before the supported range widens to it.
- `go test -tags boximage -count=1 -v -timeout 30m -run TestLandsBoxCodeRunsOnUbuntu ./internal/boxes/`, with `HCLOUD_TOKEN` set: runs land's box code (setup and its image check, a box command, rearm, the self-delete) on real Hetzner boxes from crabbox's `os: ubuntu:24.04` and `ubuntu:26.04`, a few cents' worth, and deletes all it made. It must pass before every release.
- To try a fix inside an adopting repository: `go build -o /tmp/land-dev .` and run `/tmp/land-dev` there instead of `scripts/land`.

## Code

- Standard library first; `gopkg.in/yaml.v3` is the only dependency. Child processes go through `internal/proc`; stage commands through `internal/stage`.
- stdout carries what a caller reads (a box command's own output, `land green`'s commit); everything land says goes to stderr.
- **Public, and permanent:** anything pushed is public, and Go's module proxy keeps every version anyone installs, even after a history rewrite. Never name a private repository, path, host or person in code, tests, commits or issues; code taken from another tool loses every repository-specific name, path and constant.
- Tokens (`HCLOUD_TOKEN`, `GH_TOKEN`) come from the environment only: never into a file in the repository, a log, a mark, a comment or a command line.

## Tests

Drive a module through its interface and assert what a caller sees. Each test must fail on a plausible bug in the behaviour it names. Use scratch git repositories in `t.TempDir()`, `forge.Memory` and fakes on `PATH`; `go test` never calls GitHub or Hetzner.

## Commits and releases

- Conventional Commits (`fix(boxes): …`); the body says what and why.
- A release is a tag `vX.Y.Z` on `main`, pushed to GitHub once the `boximage` test passes on it: patch for fixes, minor for new behaviour or `.land.yaml` keys. Never move or reuse a tag; the module proxy keeps the first commit it saw for it. Adopting repositories take it by setting `version:` in `.land.yaml` and running `land docs`.
