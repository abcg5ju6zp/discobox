#!/usr/bin/env bats
# shellcheck disable=SC2164 # bats runs tests and hooks under set -e, so a failed cd already fails.
#
# A multi-source apply is a resumable per-source plan (ADR 26-10-15-223):
# when one source conflicts, the sources before it stay applied and recorded,
# the conflict is held open in a private worktree for the user, and the
# sources after it are reported pending rather than touched. Resolving the
# conflict and re-running lands it, continues with the rest, and a second
# re-run is a no-op with no duplicate records.
#
# Like worktree_git.bats this drives the development stack that is already
# running (`task dev`) and owns only what it creates, purged by recorded ID.
# It skips when the stack is not up.

setup_file() {
  REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  export REPO_ROOT
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"
  command -v jq >/dev/null 2>&1 || skip "jq is required"

  export DISCOBOX_BATS_PORT="${PORT:-18080}"
  export DISCOBOX_BATS_SERVER="http://127.0.0.1:$DISCOBOX_BATS_PORT"
  curl -fsS "$DISCOBOX_BATS_SERVER/openapi.yaml" >/dev/null 2>&1 ||
    skip "no development server on port $DISCOBOX_BATS_PORT; run 'task dev'"

  (cd cli && go build -o ../build/discobox ./cmd/discobox) || skip "cannot build the CLI"

  export DISCOBOX_BATS_HOME="$BATS_SUITE_TMPDIR/home"
  mkdir -p "$DISCOBOX_BATS_HOME"
  export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
  : >"$BATS_SUITE_TMPDIR/sandboxes"

  cli admin harness ls -o json 2>/dev/null | jq -e '.harnessConfigs[] | select(.slug == "shell" and .configured)' >/dev/null ||
    skip "the shell harness is not configured on the development server"

  # Three independent local repositories, all on main with one base commit.
  DISCOBOX_BATS_WORK="$(mktemp -d "$BATS_SUITE_TMPDIR/origins.XXXXXX")"
  export DISCOBOX_BATS_WORK
  for name in alpha bravo charlie; do
    git init -q -b main "$DISCOBOX_BATS_WORK/$name"
    git -C "$DISCOBOX_BATS_WORK/$name" config user.name Bats
    git -C "$DISCOBOX_BATS_WORK/$name" config user.email bats@example.com
    echo "$name base" >"$DISCOBOX_BATS_WORK/$name/README.md"
    git -C "$DISCOBOX_BATS_WORK/$name" add README.md
    commit_in "$DISCOBOX_BATS_WORK/$name" "base $name"
  done

  BRAVO_BASE="$(git -C "$DISCOBOX_BATS_WORK/bravo" rev-parse HEAD)"
  CHARLIE_BASE="$(git -C "$DISCOBOX_BATS_WORK/charlie" rev-parse HEAD)"
  export BRAVO_BASE CHARLIE_BASE

  # The primary source is alpha; bravo and charlie are secondary sources,
  # placed at the same absolute paths they occupy on this machine (the same
  # mapping a `-i` include creates).
  cat >"$DISCOBOX_BATS_WORK/refs.json" <<JSON
{
  "bravo": {
    "kind": "git",
    "slug": "bravo",
    "localDirectory": "$DISCOBOX_BATS_WORK/bravo",
    "checkout": {"commit": "$BRAVO_BASE", "refName": "main", "refType": "branch"},
    "destination": {"directory": "$DISCOBOX_BATS_WORK/bravo"}
  },
  "charlie": {
    "kind": "git",
    "slug": "charlie",
    "localDirectory": "$DISCOBOX_BATS_WORK/charlie",
    "checkout": {"commit": "$CHARLIE_BASE", "refName": "main", "refType": "branch"},
    "destination": {"directory": "$DISCOBOX_BATS_WORK/charlie"}
  }
}
JSON

  created="$(cd "$DISCOBOX_BATS_WORK/alpha" && cli new -H shell -d --include-dirty=false \
    --source-code-references "@$DISCOBOX_BATS_WORK/refs.json" -o json)" ||
    { echo "$created" >&2; exit 1; }
  DISCOBOX_BATS_SANDBOX_ID="$(printf '%s' "$created" | jq -r .id)"
  export DISCOBOX_BATS_SANDBOX_ID
  echo "$DISCOBOX_BATS_SANDBOX_ID" >>"$BATS_SUITE_TMPDIR/sandboxes"
  wait_for_ready "$DISCOBOX_BATS_SANDBOX_ID"
}

teardown_file() {
  cd "$REPO_ROOT"
  local id
  while read -r id; do
    [ -n "$id" ] || continue
    cli admin box purge "$id" >/dev/null 2>&1 </dev/null || true
  done <"$BATS_SUITE_TMPDIR/sandboxes"
}

cli() {
  HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" "$@"
}

commit_in() {
  local dir="$1" message="$2"
  shift 2
  git -C "$dir" -c user.name=Bats -c user.email=bats@example.com commit -q -m "$message" "$@"
}

wait_for_ready() {
  local id="$1" deadline=$((SECONDS + 300)) state=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(cli admin box get "$id" -o json 2>/dev/null | jq -r '.runtime.state // empty')"
    [ "$state" = ready ] && return 0
    sleep 2
  done
  echo "discobox $id did not become ready (last state: $state)" >&2
  return 1
}

# in_repo runs a shell snippet in one of the box's source checkouts by path.
in_repo() {
  local repo="$1" snippet="$2"
  cli shell "$DISCOBOX_BATS_SANDBOX_ID" -- sh -c "cd '$DISCOBOX_BATS_WORK/$repo' && $snippet"
}

# apply_here runs apply with the suite HOME from the primary checkout, which is
# the same project context the existing apply suite uses.
apply_here() {
  (cd "$DISCOBOX_BATS_WORK/alpha" && cli apply "$DISCOBOX_BATS_SANDBOX_ID" "$@")
}

applied_records() {
  cli admin box get "$DISCOBOX_BATS_SANDBOX_ID" -o json |
    jq -c '[.runtime.appliedCommits[]? | .slug] | sort'
}

@test "a conflict stops the multi-source apply after the applied predecessor and parks a worktree" {
  # Clean work in alpha and charlie.
  in_repo alpha 'echo alpha-from-box >alpha.txt && git add alpha.txt && git -c user.name=Box -c user.email=box@example.com commit -q -m "alpha in the box"'
  in_repo charlie 'echo charlie-from-box >charlie.txt && git add charlie.txt && git -c user.name=Box -c user.email=box@example.com commit -q -m "charlie in the box"'

  # Bravo diverges: a host commit and a discobox commit on the same file.
  echo "host bravo" >"$DISCOBOX_BATS_WORK/bravo/README.md"
  commit_in "$DISCOBOX_BATS_WORK/bravo" "host bravo work" -a
  BRAVO_BEFORE="$(git -C "$DISCOBOX_BATS_WORK/bravo" rev-parse HEAD)"
  in_repo bravo 'echo box bravo >README.md && git add README.md && git -c user.name=Box -c user.email=box@example.com commit -q -m "bravo in the box"'
  BRAVO_TIP="$(in_repo bravo 'git rev-parse HEAD')"
  export BRAVO_BEFORE BRAVO_TIP

  run apply_here -o json
  echo "$output"
  [ "$status" -ne 0 ]

  # Alpha applied, bravo conflict, charlie never started.
  [ "$(printf '%s' "$output" | jq -r '.sources[] | select(.slug=="primary") | .status')" = applied ]
  [ "$(printf '%s' "$output" | jq -r '.sources[] | select(.slug=="bravo") | .status')" = conflict ]
  [ "$(printf '%s' "$output" | jq -r '.sources[] | select(.slug=="charlie") | .status')" = pending ]

  # Alpha's content landed locally; bravo's branch and tree did not move.
  [ "$(cat "$DISCOBOX_BATS_WORK/alpha/alpha.txt")" = alpha-from-box ]
  [ "$(git -C "$DISCOBOX_BATS_WORK/bravo" rev-parse HEAD)" = "$BRAVO_BEFORE" ]
  [ "$(cat "$DISCOBOX_BATS_WORK/bravo/README.md")" = "host bravo" ]

  # Charlie was never fetched.
  run git -C "$DISCOBOX_BATS_WORK/charlie" for-each-ref "refs/discobox/*"
  [ "$output" = "" ]

  # The server recorded only alpha.
  [ "$(applied_records)" = '["primary"]' ]

  # The conflict is parked in a retained worktree with CHERRY_PICK_HEAD.
  WT="$(printf '%s' "$output" | jq -r '.sources[] | select(.slug=="bravo") | .worktreeDir')"
  [ -n "$WT" ] && [ "$WT" != null ]
  run git -C "$WT" rev-parse CHERRY_PICK_HEAD
  [ "$output" = "$BRAVO_TIP" ]
  run git -C "$DISCOBOX_BATS_WORK/bravo" worktree list --porcelain
  [[ "$output" == *"worktree $WT"* ]]

  # The plan is on disk, keyed under this machine's CLI state directory.
  [ -f "$DISCOBOX_BATS_HOME/.local/state/discobox/cli/apply/$DISCOBOX_BATS_SANDBOX_ID/plan.json" ]
}

@test "resolving the parked conflict and re-running lands bravo, continues charlie, and records each once" {
  # The user resolves the conflict in the retained worktree.
  first_report="$(apply_here -o json || true)"
  WT="$(printf '%s' "$first_report" | jq -r '.sources[] | select(.slug=="bravo") | .worktreeDir')"
  echo "resolved by the user" >"$WT/README.md"
  git -C "$WT" add README.md
  GIT_EDITOR=true git -C "$WT" -c user.name=Bats -c user.email=bats@example.com cherry-pick --continue

  run apply_here -o json
  echo "$output"
  [ "$status" -eq 0 ]
  [ "$(printf '%s' "$output" | jq -r '.sources | length')" -ge 3 ]
  # Primary landed and was recorded during the stopped run, so this run has
  # nothing new for it; bravo (the resolved conflict) and charlie (which was
  # pending) are what this run applies.
  [ "$(printf '%s' "$output" | jq -r '.sources[] | select(.slug=="primary") | .status')" = up-to-date ]
  for slug in bravo charlie; do
    [ "$(printf '%s' "$output" | jq -r --arg slug "$slug" '.sources[] | select(.slug==$slug) | .status')" = applied ]
  done

  # Bravo landed with the resolution; charlie's content arrived.
  [ "$(cat "$DISCOBOX_BATS_WORK/bravo/README.md")" = "resolved by the user" ]
  [ "$(cat "$DISCOBOX_BATS_WORK/charlie/charlie.txt")" = charlie-from-box ]

  # The worktree is gone and each source is recorded exactly once.
  run git -C "$DISCOBOX_BATS_WORK/bravo" worktree list --porcelain
  [[ "$output" != *"worktree $WT"* ]]
  [ "$(applied_records)" = '["bravo","charlie","primary"]' ]

  # The plan is cleared.
  [ ! -f "$DISCOBOX_BATS_HOME/.local/state/discobox/cli/apply/$DISCOBOX_BATS_SANDBOX_ID/plan.json" ]
}

@test "a third apply is up to date and records nothing new" {
  run apply_here -o json
  echo "$output"
  [ "$status" -eq 0 ]
  for slug in primary bravo charlie; do
    [ "$(printf '%s' "$output" | jq -r --arg slug "$slug" '.sources[] | select(.slug==$slug) | .status')" = up-to-date ]
  done
  [ "$(applied_records)" = '["bravo","charlie","primary"]' ]
}
