#!/usr/bin/env bash
# build-gc-release_test.sh: exercises build-gc-release.sh end to end against
# a tiny stand-in for the gascity module, local bare repositories for the
# upstream and fork remotes, and a stub gh that records what it is asked to
# publish. Nothing touches the network or a real repository.
#
# Usage: scripts/ucc/build-gc-release_test.sh   (needs git, go, jq, tar)

set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd -P)
script="$here/build-gc-release.sh"
repo_root=$(cd "$here/../.." && pwd -P)
go_bin=${GO:-$(command -v go 2>/dev/null || echo /usr/local/go/bin/go)}
go_dir=$(dirname "$go_bin")

tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT

export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
export GO="$go_bin"
# The fixture uses the repository's own go directive, so the toolchain that
# builds it (and stamps its VCS metadata) is the one a real build uses.
go_directive=$(awk '$1 == "go" { print $2; exit }' "$repo_root/go.mod")

failures=0
pass() { echo "PASS: $*"; }
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}
check() {
  local what=$1
  shift
  if "$@"; then pass "$what"; else fail "$what"; fi
}

# --- fixture: a stand-in gascity module ------------------------------------
src="$tmp/src"
mkdir -p "$src/cmd/gc" "$src/cmd/gc-osc7501" "$src/cmd/gc-write-mint" "$src/internal/productmetrics" "$src/scripts"
printf 'module github.com/gastownhall/gascity\n\ngo %s\n' "$go_directive" >"$src/go.mod"
cat >"$src/internal/productmetrics/release.go" <<'EOF'
package productmetrics

var compiledReleaseTag string

// ReleaseTag returns the linker-injected release tag.
func ReleaseTag() string { return compiledReleaseTag }
EOF
cat >"$src/cmd/gc/main.go" <<'EOF'
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/productmetrics"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 2 && os.Args[2] == "--json" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
			"version": version, "commit": commit, "date": date,
			"release_tag": productmetrics.ReleaseTag(),
		})
		return
	}
	fmt.Println(version)
}
EOF
cat >"$src/cmd/gc-osc7501/main.go" <<'EOF'
package main

import "fmt"

var version = "dev"

func main() { fmt.Println(version) }
EOF
cat >"$src/cmd/gc-write-mint/main.go" <<'EOF'
package main

func main() {}
EOF
cp -f "$repo_root/.goreleaser.yml" "$src/.goreleaser.yml"
cp -f "$repo_root/scripts/verify-release-binary-metadata.sh" "$src/scripts/"
echo "license" >"$src/LICENSE"
echo "readme" >"$src/README.md"
echo "changelog" >"$src/CHANGELOG.md"
printf 'line one\nline two\n' >"$src/notes.txt"

remotes="$tmp/remotes"
upstream_bare="$remotes/gastownhall/gascity.git"
fork_bare="$remotes/UndermountainCC/gascity.git"
git init --quiet --bare "$upstream_bare"
git init --quiet --bare "$fork_bare"
git -C "$src" init --quiet -b main
git -C "$src" add -A
git -C "$src" commit --quiet -m "base"
git -C "$src" push --quiet "$upstream_bare" main
git -C "$src" push --quiet "$fork_bare" main
base_sha=$(git -C "$src" rev-parse HEAD)

# make_pr N FILE CONTENT: one commit on top of the base, published as
# refs/pull/N/head on upstream.
make_pr() {
  local n=$1 file=$2 content=$3
  git -C "$src" switch --quiet --detach "$base_sha"
  printf '%b' "$content" >"$src/$file"
  git -C "$src" add -A
  git -C "$src" commit --quiet -m "pr $n: change $file"
  git -C "$src" push --quiet "$upstream_bare" "HEAD:refs/pull/$n/head"
}
make_pr 1 feature.txt 'feature\n'
make_pr 2 notes.txt 'line one\nline two from pr 2\n'
make_pr 3 notes.txt 'line one\nline two from pr 3\n'
git -C "$src" switch --quiet --detach "$base_sha"
echo extra >"$src/extra.txt"
git -C "$src" add -A
git -C "$src" commit --quiet -m "loose commit"
loose_sha=$(git -C "$src" rev-parse HEAD)
git -C "$src" push --quiet "$upstream_bare" "HEAD:refs/heads/loose"

rig="$tmp/rig"
git clone --quiet --no-checkout "$upstream_bare" "$rig"
git -C "$rig" checkout --quiet --detach "$base_sha"
git -C "$rig" remote rename origin upstream
git -C "$rig" remote add origin "$fork_bare"
git -C "$rig" fetch --quiet origin
git -C "$rig" config user.name test
git -C "$rig" config user.email test@example.com
git -C "$rig" fetch --quiet upstream loose

# --- stub gh -----------------------------------------------------------------
# State lives in $GH_STATE: one directory per release holding its assets and
# notes. Every call is appended to $GH_STATE/log.
mkdir -p "$tmp/bin" "$tmp/gh-state"
cat >"$tmp/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "gh $*" >>"$GH_STATE/log"
sub="$1 $2"
shift 2
positional=()
notes_file=""
pattern=""
dir=""
target=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo|--json|--title|--jq) shift 2 ;;
    --notes-file) notes_file=$2; shift 2 ;;
    --pattern) pattern=$2; shift 2 ;;
    --dir) dir=$2; shift 2 ;;
    --target) target=$2; shift 2 ;;
    --prerelease|--clobber) shift ;;
    *) positional+=("$1"); shift ;;
  esac
done
case "$sub" in
  "pr view")
    head=$(git -C "$GH_UPSTREAM" rev-parse "refs/pull/${positional[0]}/head")
    jq -n --arg h "$head" --arg n "${positional[0]}" \
      '{title: ("pr " + $n + " title"), state: "OPEN", url: ("https://github.com/gastownhall/gascity/pull/" + $n), headRefOid: $h}'
    ;;
  "release view")
    rel="$GH_STATE/releases/${positional[0]}"
    [[ -d "$rel" ]] || { echo "release not found" >&2; exit 1; }
    jq -n --arg t "${positional[0]}" '{url: ("https://github.com/UndermountainCC/gascity/releases/tag/" + $t), isPrerelease: true}'
    ;;
  "release create")
    tagname=${positional[0]}
    rel="$GH_STATE/releases/$tagname"
    mkdir -p "$rel"
    git -C "$GH_FORK" update-ref "refs/tags/$tagname" "$target"
    cp -f "$notes_file" "$rel/notes.md"
    for asset in "${positional[@]:1}"; do cp -f "$asset" "$rel/"; done
    ;;
  "release upload")
    rel="$GH_STATE/releases/${positional[0]}"
    for asset in "${positional[@]:1}"; do cp -f "$asset" "$rel/"; done
    ;;
  "release edit")
    cp -f "$notes_file" "$GH_STATE/releases/${positional[0]}/notes.md"
    ;;
  "release download")
    mkdir -p "$dir"
    cp -f "$GH_STATE/releases/${positional[0]}/$pattern" "$dir/"
    ;;
  *) echo "stub gh: unsupported: $sub" >&2; exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/gh"
export PATH="$tmp/bin:$go_dir:$PATH"
export GH_STATE="$tmp/gh-state" GH_UPSTREAM="$upstream_bare" GH_FORK="$fork_bare"

run() {
  local out=$1
  shift
  set +e
  (cd "$rig" && "$script" --base upstream/main "$@") >"$out.stdout" 2>"$out.stderr"
  local status=$?
  set -e
  echo "$status" >"$out.status"
}
status_of() { cat "$1.status"; }
fork_refs() { git -C "$fork_bare" for-each-ref --format='%(refname)' 'refs/tags' 'refs/heads/ucc'; }
gh_calls() { grep -c "gh release \(create\|upload\|edit\)" "$GH_STATE/log" 2>/dev/null || true; }

# --- usage errors --------------------------------------------------------------
run "$tmp/usage-noname"
check "missing --name is a usage error" test "$(status_of "$tmp/usage-noname")" = 2
run "$tmp/usage-badname" --name ../evil
check "a name with a path is a usage error" test "$(status_of "$tmp/usage-badname")" = 2
run "$tmp/usage-badpr" --name x --pr abc
check "a non-numeric --pr is a usage error" test "$(status_of "$tmp/usage-badpr")" = 2

# --- dry run: cherry-pick a PR ---------------------------------------------------
run "$tmp/dry" --name dry-1 --pr 1 --dry-run --keep-worktree
check "dry run exits 0" test "$(status_of "$tmp/dry")" = 0
[[ "$(status_of "$tmp/dry")" == 0 ]] || cat "$tmp/dry.stderr" >&2
dist="$rig/worktrees/gc-build-dry-1-dist"
wt="$rig/worktrees/gc-build-dry-1"
check "dry run writes the archive" test -f "$dist/gascity_dry-1_linux_amd64.tar.gz"
check "archive holds the upstream release members" test \
  "$(tar tzf "$dist/gascity_dry-1_linux_amd64.tar.gz" | tr '\n' ' ')" = \
  "CHANGELOG.md LICENSE README.md gc gc-osc7501 gc-write-mint "
check "archive members are not group- or world-writable" test \
  "$(tar tzvf "$dist/gascity_dry-1_linux_amd64.tar.gz" | awk '{print $1}' | sort -u | tr '\n' ' ')" = "-rw-r--r-- -rwxr-xr-x "
check "checksums file matches the archive" bash -c "cd '$dist' && sha256sum -c --quiet gascity_dry-1_checksums.txt"
sha=$(awk '{print $1}' "$dist/gascity_dry-1_checksums.txt")
check "stdout reports version, tag and sha256" bash -c \
  "grep -qx 'version:    dry-1' '$tmp/dry.stdout' && grep -qx 'tag:        gc-dry-1' '$tmp/dry.stdout' && grep -qx 'sha256:     $sha' '$tmp/dry.stdout'"
build_sha=$(jq -r '.build_sha' "$dist/build-info.json")
check "built commit is the base plus the PR commit" test "$(git -C "$wt" rev-parse HEAD~1)" = "$base_sha"
check "cherry-pick records its source" bash -c "git -C '$wt' log -1 --format=%B | grep -q 'cherry picked from commit'"
check "the PR's change is in the build" test -f "$wt/feature.txt"
mkdir -p "$tmp/x"
tar xzf "$dist/gascity_dry-1_linux_amd64.tar.gz" -C "$tmp/x"
check "gc reports the build version and commit" test \
  "$("$tmp/x/gc" version --json | jq -r '.version + " " + .commit')" = "dry-1 $build_sha"
check "gc carries the snapshot release tag" test \
  "$("$tmp/x/gc" version --json | jq -r '.release_tag')" = "dry-1-canary-$(git -C "$wt" show -s --format=%h HEAD)"
check "gc-osc7501 reports the build version" test "$("$tmp/x/gc-osc7501")" = "dry-1"
check "notes name the base, the PR head and the sha256" bash -c \
  "grep -q '$base_sha' '$dist/notes.md' && grep -q '$(git -C "$upstream_bare" rev-parse refs/pull/1/head)' '$dist/notes.md' && grep -q '$sha' '$dist/notes.md'"
check "dry run pushes nothing" test -z "$(fork_refs)"
check "dry run publishes nothing" test "$(gh_calls)" = 0
git -C "$rig" worktree remove --force "$wt"

# --- dry run: merge method and a loose commit -------------------------------------
run "$tmp/merge" --name merge-1 --pr 1 --commit "$loose_sha" --method merge --dry-run --keep-worktree
check "merge dry run exits 0" test "$(status_of "$tmp/merge")" = 0
[[ "$(status_of "$tmp/merge")" == 0 ]] || cat "$tmp/merge.stderr" >&2
wt="$rig/worktrees/gc-build-merge-1"
check "merge method makes a merge commit" test "$(git -C "$wt" rev-list --parents -n1 HEAD~1 | wc -w)" = 3
check "the loose commit is applied last" test "$(git -C "$wt" log -1 --format=%s)" = "loose commit"
git -C "$rig" worktree remove --force "$wt"

# --- conflict ------------------------------------------------------------------------
run "$tmp/conflict" --name conflict-1 --pr 2 --pr 3 --dry-run
check "a conflicting PR fails" test "$(status_of "$tmp/conflict")" = 1
check "the conflict error names the PR and the path" bash -c \
  "grep -q 'PR #3' '$tmp/conflict.stderr' && grep -q 'did not apply cleanly' '$tmp/conflict.stderr' && grep -q 'notes.txt' '$tmp/conflict.stderr'"
check "a conflict builds no archive" test ! -e "$rig/worktrees/gc-build-conflict-1-dist/gascity_conflict-1_linux_amd64.tar.gz"
check "a conflict leaves no cherry-pick in progress" test ! -e "$(git -C "$rig/worktrees/gc-build-conflict-1" rev-parse --git-path CHERRY_PICK_HEAD)"

# --- a PR already in the base ------------------------------------------------------------
run "$tmp/contained" --name contained-1 --base "$base_sha" --commit "$base_sha" --dry-run
check "a commit already in the base fails" test "$(status_of "$tmp/contained")" = 1

# --- publish -------------------------------------------------------------------------------
run "$tmp/pub" --name pub-1 --pr 1
check "publish exits 0" test "$(status_of "$tmp/pub")" = 0
[[ "$(status_of "$tmp/pub")" == 0 ]] || cat "$tmp/pub.stderr" >&2
pub_sha=$(jq -r '.build_sha' "$rig/worktrees/gc-build-pub-1-dist/build-info.json")
check "publish pushes the build branch to the fork" test \
  "$(git -C "$fork_bare" rev-parse refs/heads/ucc/build/pub-1)" = "$pub_sha"
check "the release tag targets the built commit" test "$(git -C "$fork_bare" rev-parse refs/tags/gc-pub-1)" = "$pub_sha"
check "publish creates a prerelease" grep -q "gh release create gc-pub-1 .*--target $pub_sha --prerelease" "$GH_STATE/log"
check "the release carries the archive and checksums" test -f "$GH_STATE/releases/gc-pub-1/gascity_pub-1_linux_amd64.tar.gz" -a \
  -f "$GH_STATE/releases/gc-pub-1/gascity_pub-1_checksums.txt"
check "the fork's main is untouched" test "$(git -C "$fork_bare" rev-parse refs/heads/main)" = "$base_sha"
check "upstream is untouched" test "$(git -C "$upstream_bare" for-each-ref --format='%(refname)' refs/tags refs/heads/ucc)" = ""
check "publish removes the build worktree" test ! -e "$rig/worktrees/gc-build-pub-1"
check "publish reports the release URL" grep -q '^release: *https://github.com/UndermountainCC/gascity/releases/tag/gc-pub-1$' "$tmp/pub.stdout"

# --- idempotency -----------------------------------------------------------------------------
creates_before=$(grep -c "gh release create" "$GH_STATE/log")
run "$tmp/again" --name pub-1 --pr 1
check "re-running an existing release exits 3" test "$(status_of "$tmp/again")" = 3
check "re-running creates no second release" test "$(grep -c "gh release create" "$GH_STATE/log")" = "$creates_before"
run "$tmp/again-dry" --name pub-1 --pr 1 --dry-run
check "a dry run of an existing release also stops" test "$(status_of "$tmp/again-dry")" = 3

run "$tmp/replace" --name pub-1 --pr 1 --commit "$loose_sha" --replace
check "--replace exits 0" test "$(status_of "$tmp/replace")" = 0
[[ "$(status_of "$tmp/replace")" == 0 ]] || cat "$tmp/replace.stderr" >&2
replace_sha=$(jq -r '.build_sha' "$rig/worktrees/gc-build-pub-1-dist/build-info.json")
check "--replace moves the tag and branch" test \
  "$(git -C "$fork_bare" rev-parse refs/tags/gc-pub-1) $(git -C "$fork_bare" rev-parse refs/heads/ucc/build/pub-1)" = "$replace_sha $replace_sha"
check "--replace updates assets in place" grep -q "gh release upload gc-pub-1 .*--clobber" "$GH_STATE/log"
check "--replace creates no second release" test "$(grep -c "gh release create" "$GH_STATE/log")" = "$creates_before"

# --- safety: a fork remote pointing elsewhere is refused ----------------------------------------
git -C "$rig" remote add wrong "$upstream_bare"
run "$tmp/wrongfork" --name wrong-1 --fork-remote wrong --dry-run
check "a fork remote that is not the fork is refused" bash -c \
  "test \"\$(cat '$tmp/wrongfork.status')\" = 1 && grep -q 'refusing to publish' '$tmp/wrongfork.stderr'"

# --- drift: a changed .goreleaser.yml stops the build ----------------------------------------------
git -C "$src" switch --quiet --detach "$base_sha"
sed -i 's/CGO_ENABLED=0/CGO_ENABLED=1/' "$src/.goreleaser.yml"
git -C "$src" commit --quiet -am "drift"
git -C "$src" push --quiet "$upstream_bare" "HEAD:refs/heads/drift"
git -C "$rig" fetch --quiet upstream drift
run "$tmp/drift" --name drift-1 --base upstream/drift --dry-run
check "goreleaser drift stops the build" bash -c \
  "test \"\$(cat '$tmp/drift.status')\" = 1 && grep -q 'no longer contains' '$tmp/drift.stderr'"

if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi
echo "all checks passed"
