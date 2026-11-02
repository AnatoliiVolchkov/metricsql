# metricsql fork with source positions, used by pint

This repository is **AnatoliiVolchkov/metricsql** (public), a fork of
[VictoriaMetrics/metricsql](https://github.com/VictoriaMetrics/metricsql). It keeps the
upstream module path `github.com/VictoriaMetrics/metricsql`.

- **Only consumer:** **AnatoliiVolchkov/pint**, which pins an exact tag in its `go.mod`:
  `replace github.com/VictoriaMetrics/metricsql => github.com/AnatoliiVolchkov/metricsql v0.87.5-pint.3`.
- **What the fork adds:** byte positions for every parsed expression and parse error. pint
  needs them to point diagnostics at the right part of a rule. Upstream behaviour is
  otherwise unchanged.

In the local workspace (`<workspace>/`), pint lives next to this repository in
`../pint`. Its `.claude/CLAUDE.md` and `.claude/docs/` cover the whole project: user rules,
releases, VictoriaMetrics facts and the backlog. Read them too, starting with "This
repository is public": the same rules on internal names, accounts and pushing apply here.

## Rules from the user

1. **Separate commits.** "all big changes must be separate commit". Documentation-only work
   goes in its own commit.
2. **Commit, push, tag or release only when the user asks.**
3. **"my patched version with metricsql patches must build and work then usual".**
   - Upstream behaviour must not change.
   - Upstream's own `*_test.go` files run unchanged in CI ("Upstream test parity" job) and
     must pass.
   - The fork only adds code and tests.
4. **Keep upstream merges cheap.**
   - Put new logic in fork-only files (`source.go`, `source_*_test.go`).
   - Keep edits to `parser.go`, `lexer.go` and `utils.go` minimal.
   - Keep upstream's `main.yml` workflow unchanged and disabled.
5. **Answer in the user's language** (Russian or English).

## Fork-only additions

| File | What |
|---|---|
| `source.go` | `Span`, `SourceInfo{Pos, Origin, Parens}`, `SourceNode`, `SourceOf(e)`, `OuterPos()`, `ParseError{Msg, Pos}`, and WITH-template position relocation |
| `lexer.go`, `parser.go` (edited) | byte offsets on tokens, `SourceInfo` threaded through every node; errors become `*ParseError` with a position |
| `utils.go` (edited) | "unsupported function" errors carry the function's position |
| `source_test.go`, `source_provenance_test.go`, `source_fuzz_test.go`, `testdata/fuzz/FuzzParseSource/` | position tests, the provenance oracle and the fuzz corpus |
| `.github/workflows/ci.yml`, `upstream-sync.yml`, `.github/scripts/upstream-sync.sh` | fork CI and sync |
| `docs/UPSTREAM_SYNC.md`, `.claude/` | fork docs |

[docs/source-positions.md](docs/source-positions.md) explains the position model, its
invariants, and the API pint depends on.

## Commands

```sh
go vet ./...
go test ./...
go test -race ./...
go test -run '^$' -fuzz=FuzzParseSource -fuzztime=30s .     # CI runs 30s; longer locally
```

Test pint against local changes without tagging. In `../pint`:

```sh
cp go.work.example go.work     # if missing
go build ./... && go test ./internal/parser/... ./internal/checks/...
GOWORK=off go build ./...      # back to the pinned tag, as CI builds
```

## Releasing a version for pint

1. Merge the change to `master`.
2. Tag it `<upstream base>-pint.N`, the next free N:
   ```sh
   BASE=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' master)   # v0.87.5
   git tag -l "$BASE-pint.*"
   git tag -a "$BASE-pint.N" -m "metricsql $BASE with pint source positions"
   git push origin "$BASE-pint.N"
   ```
   - The current tag is `v0.87.5-pint.3`.
   - **Tags in this repository start at `-pint.3`.** Don't reuse lower numbers.
   - **Never move or delete a pushed tag.**
3. Bump the pin in pint, in a separate pint commit. The steps are in
   `../pint/.claude/docs/release-and-sync.md`, section "Updating the metricsql pin".

## Upstream sync

- **What gets merged:** stable upstream release tags only (`vX.Y.Z`). They are fetched into
  `refs/upstream-tags/*`, merged with `--no-ff` in a `sync/upstream-<tag>` branch, and go
  through a PR merged with a merge commit.
- **Script:** `.github/scripts/upstream-sync.sh` is identical to pint's copy; keep them in
  sync.
- **Commands:** `gh workflow run upstream-sync.yml -R AnatoliiVolchkov/metricsql -f dry_run=true`. The
  full procedure is in `docs/UPSTREAM_SYNC.md`.
- **Conflicts** are expected in `parser.go` and `lexer.go`. Keep upstream's change and
  re-add the position tracking around it.
- **New upstream expression types** need a `SourceInfo` and position tests. The provenance
  oracle (`TestSourceProvenanceUpstreamSuccessCorpus`) fails until they have them.
- **State on 2026-10-07:** the base is upstream release v0.87.5 (`13e13ef`), which is still
  upstream's newest release, and `master` had every upstream commit on that date.
- **Release tag push:** since `067f7fa`, the sync script also pushes the merged upstream
  release tag. The `-pint.N` base comes from `git describe` against those tags, so a merged
  release must have its tag here. AnatoliiVolchkov/pint uses the same script; its v0.89.0 sync on
  2026-10-07 is described in `../pint/.claude/docs/release-and-sync.md`.
- **Secrets:** the owner sets `SYNC_TOKEN` in the browser. Without it, a sync can't push upstream changes to `.github/workflows/*`.

## Gotchas

- **`vendor/`** is upstream's vendored dependency tree. Leave it alone unless `go.mod`
  dependencies change.
- **Built-in templates** like `ru()`, `alias()` and `range_median()` get **negative**
  positions (`defaultTemplateBase = -1 << 30`), so they can be told apart from query
  positions. pint relies on that, so don't "fix" it.
- **Positions are bytes, not runes.** Multibyte metric names are covered by
  `TestSourceQuotedAndMultibyte`.
- **Upstream `IsLikelyInvalid`** (`utils.go`) flags implicit conversions such as
  `rate(sum(foo))`, `sum(foo)[5m]` and `rate(foo)[5m]`. It's upstream code, don't change it.
  - pint's `promql/syntax` check uses it to warn about the same queries VictoriaMetrics
    rejects with `-search.disableImplicitConversion`.
  - pint mirrors its rules to find positions, in `implicitConversions()` in
    `../pint/internal/checks/promql_syntax_metricsql.go`.
  - A pint test (`TestImplicitConversionsMatchIsLikelyInvalid`) fails if the two disagree. If
    an upstream sync changes `IsLikelyInvalid`, update pint's mirror in the same pin bump.
