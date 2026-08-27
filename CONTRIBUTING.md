# Contributing to auditloop

Thanks for looking. Issues and pull requests are welcome, but this repository works a
little differently from most — please read the first section before you spend time on a
change.

## This repository is a generated snapshot

Development happens in a private working repository. What you see here is produced from it
by a scripted publisher: every path is classified as published or omitted, private names
are stripped, the Go module path is rewritten to `github.com/ZacxDev/auditloop`, and the
result is committed here as a snapshot.

Two consequences worth knowing up front:

- **A merge here is not how a change lands.** Your patch is applied upstream and arrives in
  this repository on the next publish — as part of a snapshot commit, not as your commit.
  Your PR will normally be closed with a pointer to the commit that carries the change,
  rather than merged. That is not a rejection; it is the only mechanism available. Say so
  in your PR if you would like to be credited a particular way.
- **Anything committed directly to this repository is destroyed by the next publish.** Do
  not push fixes straight to `main` here expecting them to survive.

If that model does not suit the change you have in mind — a large refactor, say — open an
issue first and we can talk about sequencing before you write it.

## Building and running

Requirements: Go 1.26+, a system Chromium, and Node only to build CSS.

```bash
npm install          # Tailwind CLI + vendored axe-core (once)
make dev             # DEV_MODE, web+worker, http://localhost:8112
```

`make dev` needs nothing external — no S3, no Postgres, no API keys. It bypasses auth with
a fixed dev user, uses SQLite plus local-filesystem storage, and permits crawling loopback
so you can point it at something on your own machine.

```bash
make build && ./bin/auditloop     # production-style
```

## The gate your change has to pass

CI runs outside this repository, so **your PR will show no checks**. The authoritative gate
is the one you run locally:

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./...
```

`gofmt -l .` must print nothing. `go test ./...` is fully hermetic — no Docker, no network,
no API keys; the e2e suites use fixture HTTP servers and a fake OpenRouter endpoint. It does
use a real Chromium for the browser tests, which **skip** when none is found, so a green run
on a machine without Chromium is a weaker claim than it looks.

Two extras that apply only to specific areas:

- Touching `internal/crawler/watchdog.go` or the driver's stall handling? Run
  `go test -race ./internal/crawler`. That code deliberately abandons a stuck goroutine
  which may still read a test seam, and `make test` passes no `-race`.
- Adding or widening a `.gitleaks.toml` allowlist? Plant a realistic fake credential in a
  tracked file first and confirm gitleaks still goes red. It allowlists its own canonical
  examples, so a textbook fixture scans clean and proves nothing.

## What good tests look like here

The test suite is unusually opinionated, and matching its style is the fastest way to get a
change accepted:

- **A regression test should be watched failing on pre-change code.** If it never went red,
  say so in the PR and call it what it is — an invariant guard, not regression coverage.
- **A guard is worth what its mutation test proves.** Break the thing on purpose and check
  that *this* guard's assertion is what fails, not an earlier check or a different error.
  Several test comments in this repo record exactly that exercise, including which mutants
  survive and why; that is the house convention, not decoration.
- **State the limits you know about.** A comment saying "this skips without Chromium" or
  "this pins only the first of two call sites" is more useful than silence, and it is how
  the next person avoids re-deriving a dead end.
- Prefer literal expected values over expectations derived from the implementation under
  test.

## Code conventions

- **Go**: standard `gofmt`. Database access goes through `*db.DB` methods — no raw SQL in
  handlers. Migrations are inline, dual-dialect (Postgres and SQLite), additive, and use
  `INTEGER` rather than `BOOLEAN` for portability.
- **UI**: server-rendered gomponents (`g`/`h`/`c` aliases) with htmx 2 and Tailwind 4. Use
  the semantic tokens and component classes defined in `static/input.css` (`.card`,
  `.btn-primary`, `.badge-*`, …) rather than raw colour utilities, and gate all motion
  behind `motion-safe:`. Never put an entry animation on an htmx self-polling element — it
  re-fires on every poll.
- **Untrusted text is escaped, always.** Model-authored strings, pushed payloads, and
  selectors from a page render through `g.Text`, never `g.Raw`.

## Security-sensitive areas

Some code is load-bearing for safety and changes there get read closely:

- The SSRF guard and the runtime request interceptor (`internal/crawler/ssrf.go`,
  `intercept.go`) — including that redirect hops are re-checked, not just the initial URL.
- The closed action sets in `internal/recipe` and `internal/action`. There is deliberately
  **no** eval/script step; please do not add one, and keep `DisallowUnknownFields` in the
  parsers.
- The DOM-grounding gate in `internal/eval`, which can *drop* findings — anything ambiguous
  must err toward refuting less.
- Credential handling: encrypted at rest, write-only in the UI, and redacted from logs,
  errors, and `report.json`.

Found a vulnerability? Please use GitHub's private vulnerability reporting on this
repository rather than filing exploit details in a public issue.

## Pull requests

Describe what changed and why, and state what you actually verified — "ran `go test ./...`,
20 packages ok" beats "tests pass". If something is unverified, saying so plainly is
welcome and will not count against you.

## License

By contributing you agree that your contribution is licensed under the MIT License, the
same terms as the rest of the project — see [`LICENSE`](LICENSE).
