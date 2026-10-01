#!/usr/bin/env bash
# Merge new upstream commits into a sync/upstream-YYYYMMDD branch and open a pull request.
#
# Required environment:
#   UPSTREAM_URL     public upstream repository URL
#   UPSTREAM_BRANCH  upstream branch to merge (main for pint, master for metricsql)
#   TEST_CMD         shell command run after a clean merge
# Optional environment:
#   BASE_BRANCH      our product branch, default main
#   DRY_RUN          "true" to only detect and test the merge locally, no push/PR/issue
#   GITHUB_REPOSITORY owner/name used by gh, set by GitHub Actions
#   GITHUB_STEP_SUMMARY file for the job summary, set by GitHub Actions
set -euo pipefail

BASE_BRANCH=${BASE_BRANCH:-main}
DRY_RUN=${DRY_RUN:-false}
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}
REPO=${GITHUB_REPOSITORY:-}
BRANCH="sync/upstream-$(date -u +%Y%m%d)"

summary() { echo "$*" | tee -a "$SUMMARY"; }

git config user.name >/dev/null || git config user.name "upstream-sync[bot]"
git config user.email >/dev/null || git config user.email "upstream-sync@users.noreply.github.com"

if git remote get-url upstream >/dev/null 2>&1; then
  git remote set-url upstream "$UPSTREAM_URL"
else
  git remote add upstream "$UPSTREAM_URL"
fi
git remote set-url --push upstream DISABLE-pushing-to-public-upstream

git fetch --no-tags origin "$BASE_BRANCH"
git fetch --no-tags upstream "$UPSTREAM_BRANCH"

BASE_SHA=$(git rev-parse "origin/$BASE_BRANCH")
UPSTREAM_SHA=$(git rev-parse "upstream/$UPSTREAM_BRANCH")
SHORT=$(git rev-parse --short "$UPSTREAM_SHA")
BEHIND=$(git rev-list --count "$BASE_SHA..$UPSTREAM_SHA")
AHEAD=$(git rev-list --count "$UPSTREAM_SHA..$BASE_SHA")

summary "## Upstream sync"
summary ""
summary "- upstream: \`$UPSTREAM_URL\` \`$UPSTREAM_BRANCH\` at \`$UPSTREAM_SHA\`"
summary "- ours: \`$BASE_BRANCH\` at \`$BASE_SHA\`"
summary "- merge base: \`$(git merge-base "$BASE_SHA" "$UPSTREAM_SHA" || echo none)\`"
summary "- upstream commits missing from \`$BASE_BRANCH\`: $BEHIND"
summary "- our commits not in upstream: $AHEAD"

if [ "$BEHIND" -eq 0 ]; then
  summary ""
  summary "Nothing to sync, \`$BASE_BRANCH\` already contains upstream \`$SHORT\`."
  exit 0
fi

summary ""
summary "New upstream commits:"
summary '```'
git log --oneline --no-merges "$BASE_SHA..$UPSTREAM_SHA" | head -50 | tee -a "$SUMMARY"
summary '```'

git switch -C "$BRANCH" "$BASE_SHA"
MSG="Merge upstream $UPSTREAM_BRANCH $SHORT into $BASE_BRANCH"

open_issue() {
  local title=$1 body=$2
  if [ "$DRY_RUN" = "true" ] || [ -z "$REPO" ]; then
    return 0
  fi
  local existing
  existing=$(gh issue list -R "$REPO" --state open --search "in:title \"$title\"" --json number --jq '.[0].number // empty')
  if [ -n "$existing" ]; then
    gh issue comment "$existing" -R "$REPO" --body "$body"
  else
    gh issue create -R "$REPO" --title "$title" --body "$body"
  fi
}

if ! git merge --no-ff -m "$MSG" "$UPSTREAM_SHA"; then
  CONFLICTS=$(git diff --name-only --diff-filter=U)
  git merge --abort
  summary ""
  summary "### Merge conflicts"
  summary ""
  summary "Merging upstream \`$SHORT\` into \`$BASE_BRANCH\` conflicts in:"
  summary '```'
  echo "$CONFLICTS" | tee -a "$SUMMARY"
  summary '```'
  echo "::error::upstream merge has conflicts in: $(echo "$CONFLICTS" | tr '\n' ' ')"
  open_issue "Upstream sync conflict: $UPSTREAM_BRANCH $SHORT" "$(cat <<EOF
Merging upstream \`$UPSTREAM_BRANCH\` at \`$UPSTREAM_SHA\` into \`$BASE_BRANCH\` (\`$BASE_SHA\`) has conflicts.

Conflicting files:
\`\`\`
$CONFLICTS
\`\`\`

Resolve locally:
\`\`\`sh
git fetch origin $BASE_BRANCH && git fetch upstream $UPSTREAM_BRANCH
git switch -C $BRANCH origin/$BASE_BRANCH
git merge --no-ff upstream/$UPSTREAM_BRANCH
git diff --name-only --diff-filter=U
# resolve, keeping both upstream's and our changes, then:
git commit --no-edit
$TEST_CMD
git push -u origin $BRANCH
gh pr create --base $BASE_BRANCH --head $BRANCH
\`\`\`
See docs/UPSTREAM_SYNC.md.
EOF
)"
  exit 1
fi

summary ""
summary "Merged cleanly as \`$(git rev-parse --short HEAD)\`, running: \`$TEST_CMD\`"
TESTS_OK=true
if ! bash -c "$TEST_CMD"; then
  TESTS_OK=false
  summary ""
  summary "### Tests failed after merge"
  echo "::error::tests failed after merging upstream $SHORT"
fi

if [ "$DRY_RUN" = "true" ]; then
  summary ""
  summary "Dry run: not pushing \`$BRANCH\` and not opening a pull request."
  [ "$TESTS_OK" = "true" ]
  exit $?
fi

PUSH_ERR=$(mktemp)
if ! git push --force-with-lease origin "$BRANCH" 2>"$PUSH_ERR"; then
  cat "$PUSH_ERR"
  if grep -q "workflow" "$PUSH_ERR"; then
    open_issue "Upstream sync push refused: $UPSTREAM_BRANCH $SHORT" "Upstream \`$SHORT\` changes files under \`.github/workflows\`. GITHUB_TOKEN cannot push workflow changes; set the \`SYNC_TOKEN\` secret (fine-grained PAT with Contents, Pull requests, Issues and Workflows read/write on this repository) and re-run the Upstream sync workflow."
  fi
  exit 1
fi

TITLE="Sync with upstream $UPSTREAM_BRANCH $SHORT"
BODY=$(cat <<EOF
Automated merge of upstream \`$UPSTREAM_BRANCH\` (\`$UPSTREAM_SHA\`) into \`$BASE_BRANCH\`.

- upstream commits: $BEHIND
- tests (\`$TEST_CMD\`): $([ "$TESTS_OK" = "true" ] && echo passed || echo "**failed**, see the workflow run")
- workflow run: ${GITHUB_SERVER_URL:-https://github.com}/$REPO/actions/runs/${GITHUB_RUN_ID:-}

Merge this pull request with a merge commit, not squash or rebase. Review new files under
\`.github/workflows\` and disable upstream-only workflows as described in docs/UPSTREAM_SYNC.md.
EOF
)
PR=$(gh pr list -R "$REPO" --head "$BRANCH" --base "$BASE_BRANCH" --state open --json number --jq '.[0].number // empty')
DRAFT=()
[ "$TESTS_OK" = "true" ] || DRAFT=(--draft)
if [ -n "$PR" ]; then
  gh pr edit "$PR" -R "$REPO" --title "$TITLE" --body "$BODY"
else
  gh pr create -R "$REPO" --base "$BASE_BRANCH" --head "$BRANCH" --title "$TITLE" --body "$BODY" ${DRAFT[@]+"${DRAFT[@]}"}
fi
summary ""
summary "Pull request: $(gh pr view "$BRANCH" -R "$REPO" --json url --jq .url)"
[ "$TESTS_OK" = "true" ]
