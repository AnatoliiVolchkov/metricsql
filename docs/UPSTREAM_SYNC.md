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

`.claude/` holds onboarding docs for AI coding agents. It is fork-only and never conflicts
with upstream.

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
- Upstream changes come in through `sync/upstream-<tag>` branches and pull requests,
  merged with a merge commit.
- Only stable upstream releases are merged: tags matching `vX.Y.Z`, never the head of
  upstream `master`. Upstream tags releases on `master`, so merging one release after
  another is an ordinary merge. Upstream tags are fetched into `refs/upstream-tags/*` so
  they never mix with our `-pint.N` tags. To take a single upstream fix before it is
  released, cherry-pick it (`git cherry-pick -x <sha>`) in a pull request.

## Manual sync procedure

```sh
git switch main
git pull --ff-only origin main
git fetch --no-tags --force upstream '+refs/tags/*:refs/upstream-tags/*'

# Newest stable upstream release:
TAG=$(git for-each-ref --sort=-version:refname --format='%(refname:lstrip=2)' refs/upstream-tags/ \
  | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n 1)

git rev-list --count main..refs/upstream-tags/$TAG    # 0 means nothing to sync

BRANCH=sync/upstream-$TAG
git switch -c "$BRANCH" main
git merge --no-ff refs/upstream-tags/$TAG -m "Merge upstream release $TAG into main"
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
UPSTREAM=$(git rev-parse "refs/upstream-tags/$TAG^{commit}")
git diff --quiet "$UPSTREAM" HEAD -- $(git ls-tree --name-only "$UPSTREAM" | grep '_test.go$') \
  && echo "upstream test files are unchanged"
```

```sh
git push -u origin "$BRANCH"
gh pr create --repo AnatoliiVolchkov/metricsql --base main --head "$BRANCH" \
  --title "Sync with upstream release $TAG" \
  --body "Merge of upstream VictoriaMetrics/metricsql $TAG into main."
```

## Automated sync

`.github/workflows/upstream-sync.yml` runs `.github/scripts/upstream-sync.sh` (identical
to the copy in AnatoliiVolchkov/pint) every Monday and on demand:

```sh
gh workflow run upstream-sync.yml -R AnatoliiVolchkov/metricsql                  # newest stable release
gh workflow run upstream-sync.yml -R AnatoliiVolchkov/metricsql -f dry_run=true
gh workflow run upstream-sync.yml -R AnatoliiVolchkov/metricsql -f tag=v0.87.6   # a specific release
gh workflow run upstream-sync.yml -R AnatoliiVolchkov/metricsql -f mode=branch   # head of upstream master (exception)
```

It picks the newest stable upstream release tag (or the `tag` input), counts its commits
missing from `main` and exits successfully if there are none, so runs between upstream
releases are no-ops. Otherwise it merges the release into `sync/upstream-<tag>`, runs `go vet`,
`go test` and `go test -race`, pushes the branch and opens a pull request against `main`.
On conflicts it lists the conflicting files, opens an issue with resolution commands and
fails without pushing. It never pushes to `main`.

The optional `SYNC_TOKEN` secret (fine-grained PAT on `AnatoliiVolchkov/metricsql` with Contents,
Pull requests, Issues and Workflows read/write) is needed when upstream changes files under
`.github/workflows`; otherwise `GITHUB_TOKEN` is used. Pull requests opened by `GITHUB_TOKEN`
need "Allow GitHub Actions to create and approve pull requests":

```sh
gh api -X PUT repos/AnatoliiVolchkov/metricsql/actions/permissions/workflow \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=true
```

## Releasing a version for pint

pint pins an exact tag. Tags use the upstream version our `main` is based on plus a
`-pint.N` suffix, for example `v0.87.5-pint.3`:

```sh
git switch main && git pull --ff-only origin main
# The newest upstream release merged into main, for example v0.87.5:
BASE=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' main)
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
workflows with `git diff --name-status <old> refs/upstream-tags/<tag> -- .github/workflows`.
