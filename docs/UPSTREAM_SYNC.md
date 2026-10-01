# Upstream synchronization

This repository (`github.com/AnatoliiVolchkov/metricsql`, private) is a source-aware fork of
`github.com/VictoriaMetrics/metricsql`. It keeps the upstream module path
`github.com/VictoriaMetrics/metricsql`, so consumers use it through a `replace`
directive. Its only consumer is `github.com/AnatoliiVolchkov/pint`.

Fork additions (all additive, upstream behavior is unchanged):

- byte offsets on lexer tokens and `SourceInfo` (`Pos`, `Origin`, `Parens`) on every
  expression node, propagated through WITH template expansion, parenthesis removal and
  constant folding (`source.go`, `lexer.go`, `parser.go`);
- structured parse errors with positions;
- tests for the above in `source_test.go`.

## Repository layout

| Remote     | URL                                                  | Branch   |
|------------|------------------------------------------------------|----------|
| `origin`   | `https://github.com/AnatoliiVolchkov/metricsql.git` (private) | `main`   |
| `upstream` | `https://github.com/VictoriaMetrics/metricsql.git`   | `master` |

```sh
git clone https://github.com/AnatoliiVolchkov/metricsql.git
cd metricsql
git remote add upstream https://github.com/VictoriaMetrics/metricsql.git
git remote set-url --push upstream DISABLE-pushing-to-public-upstream
git fetch upstream master --tags
```

## Branch model

- `main` is our product branch: upstream history plus our commits, joined by merge
  commits from upstream syncs. Never rebase, squash or force-push it.
- A local `master` branch may track `upstream/master` for convenience; it is not pushed.
- Upstream changes come in through `sync/upstream-YYYYMMDD` branches and pull requests,
  merged with a merge commit.

## Manual sync procedure

```sh
git switch main
git pull --ff-only origin main
git fetch upstream master --tags

git rev-list --count main..upstream/master    # 0 means nothing to sync

BRANCH=sync/upstream-$(date -u +%Y%m%d)
git switch -c "$BRANCH" main
git merge --no-ff upstream/master -m "Merge upstream VictoriaMetrics/metricsql $(git rev-parse --short upstream/master)"
```

On conflicts list the files with `git diff --name-only --diff-filter=U`. Conflicts are
expected mostly in `parser.go` and `lexer.go`, where our changes thread `SourceInfo`
through the parser. Keep upstream's change and re-add our position tracking around it.
New upstream expression node types must get a `SourceInfo` field and position tests.

Verify before opening the pull request:

```sh
go vet ./...
go test ./...
go test -race ./...
go test -run '^$' -fuzz=FuzzParseSource -fuzztime=30s .

# Upstream parity: upstream's own tests, unchanged, must pass against our code.
UPSTREAM=$(git rev-parse upstream/master)
git diff --quiet "$UPSTREAM" HEAD -- $(git ls-tree --name-only "$UPSTREAM" | grep '_test.go$') \
  && echo "upstream test files are unchanged"
```

```sh
git push -u origin "$BRANCH"
gh pr create --repo AnatoliiVolchkov/metricsql --base main --head "$BRANCH" \
  --title "Sync with upstream VictoriaMetrics/metricsql $(git rev-parse --short upstream/master)" \
  --body "Merge of upstream/master into main."
```

## Releasing a version for pint

pint pins an exact tag. Tags use the upstream version our `main` is based on plus a
`-pint.N` suffix, for example `v0.87.5-pint.3`:

```sh
git switch main && git pull --ff-only origin main
BASE=$(git describe --tags --abbrev=0 --exclude '*-pint.*' upstream/master)
git tag -l "$BASE-pint.*"                 # pick the next N
git tag -a "$BASE-pint.N" -m "metricsql $BASE with pint source positions"
git push origin "$BASE-pint.N"
```

Never move or delete a pushed tag: the Go checksum database records it.
Versions `v0.87.5-pint.1` and `v0.87.5-pint.2` were published while the fork was public
and are cached by `proxy.golang.org`; they must not be reused.

Then in pint:

```sh
go mod edit -replace github.com/VictoriaMetrics/metricsql=github.com/AnatoliiVolchkov/metricsql@<tag>
GOPRIVATE=github.com/AnatoliiVolchkov/* go mod tidy
```

## Disabled upstream workflows

Upstream's `.github/workflows/main.yml` is kept unchanged and disabled with
`gh workflow disable main.yml -R AnatoliiVolchkov/metricsql`; `ci.yml` runs the same tests plus
race, vet, fuzz and upstream-parity checks. After each sync review new upstream
workflows with `git diff --name-status <old> upstream/master -- .github/workflows`.
