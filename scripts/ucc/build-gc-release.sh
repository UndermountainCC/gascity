#!/usr/bin/env bash
# build-gc-release.sh: build gc from a base ref plus selected upstream pull
# requests or commits, package it the way upstream's release does, and publish
# it as a prerelease on the fork.
#
# Fork-only tooling (UndermountainCC/gascity, branch ucc/tools). It is not
# meant for upstream.
#
# How the build mirrors upstream: upstream's release (.github/workflows/
# release.yml) and rolling edge build (.github/workflows/gc-edge-publish.yml)
# both run GoReleaser with .goreleaser.yml. This script runs the same
# `go build` invocations GoReleaser derives from that file for linux/amd64:
# CGO_ENABLED=0, GOAMD64=v1, the same ldflags per binary, snapshot-mode
# compiledReleaseTag, the same archive name and contents (gc, gc-osc7501,
# gc-write-mint, LICENSE, README.md, CHANGELOG.md) and a sha256 checksums
# file. It refuses to build when the base's .goreleaser.yml no longer matches
# what it replicates, so upstream drift fails loudly instead of producing a
# differently built binary. GoReleaser's {{ .Tag }} becomes the build version
# (the release tag minus its "gc-" prefix), so `gc version` prints it.
#
# Usage: see usage() below or run with --help.

set -euo pipefail

prog=$(basename "$0")

usage() {
  cat <<EOF
usage: $prog --name NAME --base REF [--pr N]... [--commit SHA]... [options]

Builds gc for linux/amd64 from REF plus the given upstream pull requests and
commits (applied in command-line order), packages it like an upstream release
and publishes it as prerelease gc-NAME on the fork.

Required:
  --name NAME           build name and version, e.g. edge-17d7d77ce-8871.
                        Release tag gc-NAME, branch ucc/build/NAME, asset
                        gascity_NAME_linux_amd64.tar.gz.
  --base REF            base ref or sha, e.g. upstream/main.

Inputs (repeatable, applied in the order given):
  --pr N                upstream pull request number (fetched from
                        refs/pull/N/head on the upstream remote).
  --commit SHA          a single commit (full sha unless it is already local).

Options:
  --method M            how a PR is applied: cherry-pick (default; its
                        commits not already in the base, with -x) or merge
                        (a --no-ff merge of its head).
  --dry-run             do everything except push and publish.
  --replace             rebuild and update an existing gc-NAME release in
                        place (moves the tag and branch, replaces assets).
  --keep-worktree       keep the build worktree after a successful run.
  --repo-dir DIR        git repository to work from (default: the main
                        checkout of the repository containing the cwd).
  --worktree-root DIR   where the build worktree goes (default:
                        <repo-dir>/worktrees). The worktree is
                        <root>/gc-build-NAME and the output <root>/gc-build-NAME-dist.
  --upstream-remote R   remote for upstream (default: upstream).
  --fork-remote R       remote for the fork (default: origin).
  --upstream-repo O/R   upstream GitHub repo (default: gastownhall/gascity).
  --fork-repo O/R       fork GitHub repo (default: UndermountainCC/gascity).
  -h, --help            show this help.

Environment: GO selects the go binary (default: go on PATH, else
/usr/local/go/bin/go).

Exit status: 0 built (and published unless --dry-run); 1 error; 2 usage
error; 3 gc-NAME already exists on the fork and --replace was not given.
EOF
}

die() {
  echo "$prog: error: $*" >&2
  exit 1
}

usage_error() {
  echo "$prog: $*" >&2
  echo "Run '$prog --help' for usage." >&2
  exit 2
}

log() {
  echo "==> $*" >&2
}

name=""
base=""
method="cherry-pick"
dry_run=false
replace=false
keep_worktree=false
repo_dir=""
worktree_root=""
upstream_remote="upstream"
fork_remote="origin"
upstream_repo="gastownhall/gascity"
fork_repo="UndermountainCC/gascity"
inputs=()

need_arg() {
  [[ $# -ge 2 && -n "$2" ]] || usage_error "$1 needs a value"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --name) need_arg "$@"; name=$2; shift 2 ;;
    --base) need_arg "$@"; base=$2; shift 2 ;;
    --pr) need_arg "$@"; inputs+=("pr:$2"); shift 2 ;;
    --commit) need_arg "$@"; inputs+=("commit:$2"); shift 2 ;;
    --method) need_arg "$@"; method=$2; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    --replace) replace=true; shift ;;
    --keep-worktree) keep_worktree=true; shift ;;
    --repo-dir) need_arg "$@"; repo_dir=$2; shift 2 ;;
    --worktree-root) need_arg "$@"; worktree_root=$2; shift 2 ;;
    --upstream-remote) need_arg "$@"; upstream_remote=$2; shift 2 ;;
    --fork-remote) need_arg "$@"; fork_remote=$2; shift 2 ;;
    --upstream-repo) need_arg "$@"; upstream_repo=$2; shift 2 ;;
    --fork-repo) need_arg "$@"; fork_repo=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage_error "unknown argument: $1" ;;
  esac
done

[[ -n "$name" ]] || usage_error "--name is required"
[[ -n "$base" ]] || usage_error "--base is required"
[[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ && "$name" != *..* ]] ||
  usage_error "--name must match [A-Za-z0-9][A-Za-z0-9._-]* without '..': $name"
case "$method" in
  cherry-pick|merge) ;;
  *) usage_error "--method must be cherry-pick or merge: $method" ;;
esac
for input in ${inputs[@]+"${inputs[@]}"}; do
  case "$input" in
    pr:*) [[ "${input#pr:}" =~ ^[1-9][0-9]*$ ]] || usage_error "--pr needs a pull request number: ${input#pr:}" ;;
    commit:*) [[ "${input#commit:}" =~ ^[0-9a-fA-F]{7,64}$ ]] || usage_error "--commit needs a commit sha: ${input#commit:}" ;;
  esac
done
for slug in "$upstream_repo" "$fork_repo"; do
  [[ "$slug" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage_error "not an OWNER/REPO: $slug"
done
[[ "${upstream_repo,,}" != "${fork_repo,,}" ]] || usage_error "--fork-repo must differ from --upstream-repo"

version=$name
tag="gc-$version"
branch="ucc/build/$name"
archive="gascity_${version}_linux_amd64.tar.gz"
checksums="gascity_${version}_checksums.txt"

for tool in git gh jq tar gzip sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not on PATH"
done
go_bin=${GO:-}
if [[ -z "$go_bin" ]]; then
  go_bin=$(command -v go 2>/dev/null || true)
  [[ -n "$go_bin" ]] || go_bin=/usr/local/go/bin/go
fi
[[ -x "$go_bin" ]] || command -v "$go_bin" >/dev/null 2>&1 || die "go not found (set GO): $go_bin"

if [[ -z "$repo_dir" ]]; then
  common_dir=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null) ||
    die "not inside a git repository; pass --repo-dir"
  repo_dir=$(dirname "$common_dir")
fi
repo_dir=$(cd "$repo_dir" && pwd -P) || die "--repo-dir does not exist: $repo_dir"
git -C "$repo_dir" rev-parse --git-dir >/dev/null 2>&1 || die "not a git repository: $repo_dir"
[[ -n "$worktree_root" ]] || worktree_root="$repo_dir/worktrees"
mkdir -p "$worktree_root"
worktree_root=$(cd "$worktree_root" && pwd -P)
worktree="$worktree_root/gc-build-$name"
out_dir="$worktree_root/gc-build-$name-dist"

rgit() {
  git -C "$repo_dir" "$@"
}

wgit() {
  git -C "$worktree" "$@"
}

# remote_slug prints OWNER/REPO for a remote's URL (ssh, https or a path
# ending in OWNER/REPO[.git]).
remote_slug() {
  local url
  url=$(rgit remote get-url "$1" 2>/dev/null) || die "git remote not found: $1"
  url=${url%/}
  url=${url%.git}
  if [[ "$url" =~ ([^/:]+)/([^/:]+)$ ]]; then
    echo "${BASH_REMATCH[1]}/${BASH_REMATCH[2]}"
  else
    die "cannot read OWNER/REPO from remote $1 URL: $url"
  fi
}

upstream_slug=$(remote_slug "$upstream_remote")
fork_slug=$(remote_slug "$fork_remote")
[[ "${upstream_slug,,}" == "${upstream_repo,,}" ]] ||
  die "remote $upstream_remote points at $upstream_slug, expected $upstream_repo"
[[ "${fork_slug,,}" == "${fork_repo,,}" ]] ||
  die "remote $fork_remote points at $fork_slug, expected $fork_repo; refusing to publish anywhere else"

# Idempotency: an existing gc-NAME tag, release or build branch on the fork
# means this build was already published.
log "checking the fork for an existing $tag"
remote_tag_sha=$(rgit ls-remote "$fork_remote" "refs/tags/$tag" | awk '{print $1}')
remote_branch_sha=$(rgit ls-remote "$fork_remote" "refs/heads/$branch" | awk '{print $1}')
release_url=""
gh_err=$(mktemp)
if release_json=$(gh release view "$tag" --repo "$fork_repo" --json url 2>"$gh_err"); then
  release_url=$(jq -r '.url' <<<"$release_json")
elif ! grep -qi 'not found' "$gh_err"; then
  cat "$gh_err" >&2
  rm -f "$gh_err"
  die "gh release view $tag --repo $fork_repo failed"
fi
rm -f "$gh_err"
if [[ -n "$remote_tag_sha$remote_branch_sha$release_url" ]] && ! $replace; then
  echo "$prog: $tag already exists on $fork_repo; nothing built or published." >&2
  [[ -z "$release_url" ]] || echo "  release: $release_url" >&2
  [[ -z "$remote_tag_sha" ]] || echo "  tag $tag -> $remote_tag_sha" >&2
  [[ -z "$remote_branch_sha" ]] || echo "  branch $branch -> $remote_branch_sha" >&2
  echo "Re-run with --replace to rebuild and update it in place, or pick a new --name." >&2
  exit 3
fi

# Base.
log "resolving base $base"
for remote in $(rgit remote); do
  if [[ "$base" == "$remote/"* ]]; then
    rgit fetch --no-tags --quiet "$remote" "${base#"$remote"/}" ||
      die "git fetch $remote ${base#"$remote"/} failed"
    break
  fi
done
if ! base_sha=$(rgit rev-parse --verify --quiet "$base^{commit}"); then
  if [[ "$base" =~ ^[0-9a-fA-F]{40}$ ]]; then
    rgit fetch --no-tags --quiet "$upstream_remote" "$base" ||
      die "base $base is not local and git fetch $upstream_remote $base failed"
    base_sha=$(rgit rev-parse --verify --quiet "$base^{commit}") || die "base $base is not a commit"
  else
    die "base $base does not resolve to a commit"
  fi
fi

# Worktree. The path is this script's own; a leftover one from an earlier run
# is replaced, anything else at that path is refused.
if [[ -e "$worktree" ]]; then
  if rgit worktree list --porcelain | grep -qxF "worktree $worktree"; then
    log "removing leftover build worktree $worktree"
    rgit worktree remove --force "$worktree"
  else
    die "$worktree exists and is not a worktree of $repo_dir; remove it or pick another --name"
  fi
fi
rgit worktree prune
rgit worktree add --quiet --detach "$worktree" "$base_sha" >/dev/null
succeeded=false
cleanup() {
  if $succeeded && ! $keep_worktree; then
    git -C "$repo_dir" worktree remove --force "$worktree" >/dev/null 2>&1 || true
  elif [[ -d "$worktree" ]]; then
    echo "$prog: build worktree kept at $worktree" >&2
  fi
}
trap cleanup EXIT

conflict_error() {
  local what=$1 op=$2 conflicts
  conflicts=$(wgit diff --name-only --diff-filter=U 2>/dev/null | sed 's/^/    /')
  wgit "$op" --abort >/dev/null 2>&1 || true
  {
    echo "$prog: error: $what did not apply cleanly on $base ($base_sha)."
    [[ -z "$conflicts" ]] || printf '  conflicting paths:\n%s\n' "$conflicts"
    echo "Nothing was resolved or published. Rebase that change, drop it, or pick another base."
  } >&2
  exit 1
}

# ensure_commit makes sha available locally, fetching from the upstream
# remote and then the fork, and prints its full sha.
ensure_commit() {
  local want=$1 full
  if full=$(wgit rev-parse --verify --quiet "$want^{commit}"); then
    echo "$full"
    return
  fi
  [[ "$want" =~ ^[0-9a-fA-F]{40}$ ]] || die "commit $want is not local; pass its full 40-character sha"
  wgit fetch --no-tags --quiet "$upstream_remote" "$want" 2>/dev/null ||
    wgit fetch --no-tags --quiet "$fork_remote" "$want" 2>/dev/null ||
    die "commit $want is in neither $upstream_remote nor $fork_remote"
  wgit rev-parse --verify "$want^{commit}"
}

notes_inputs=""
applied_json="[]"

apply_pr() {
  local pr=$1 head pr_json title state url api_head commits commit new map=""
  log "fetching PR #$pr"
  wgit fetch --no-tags --quiet "$upstream_remote" "refs/pull/$pr/head" ||
    die "git fetch $upstream_remote refs/pull/$pr/head failed: is #$pr a pull request on $upstream_repo?"
  head=$(wgit rev-parse FETCH_HEAD)
  pr_json=$(gh pr view "$pr" --repo "$upstream_repo" --json title,state,url,headRefOid) ||
    die "gh pr view $pr --repo $upstream_repo failed"
  title=$(jq -r '.title' <<<"$pr_json")
  state=$(jq -r '.state' <<<"$pr_json")
  url=$(jq -r '.url' <<<"$pr_json")
  api_head=$(jq -r '.headRefOid' <<<"$pr_json")
  [[ "$api_head" == "$head" ]] ||
    die "PR #$pr head is $api_head on GitHub but refs/pull/$pr/head fetched $head; retry once GitHub catches up"
  if wgit merge-base --is-ancestor "$head" HEAD; then
    die "PR #$pr ($head) is already contained in the base; drop --pr $pr"
  fi
  if [[ "$method" == merge ]]; then
    log "merging PR #$pr at $head"
    wgit merge --no-ff --quiet -m "Merge $upstream_repo#$pr: $title" -m "Head $head." "$head" >/dev/null 2>&1 ||
      conflict_error "PR #$pr ($url, head $head)" merge
    new=$(wgit rev-parse HEAD)
    map="    - merged head \`$head\` as \`$new\`"$'\n'
    applied_json=$(jq -c --arg pr "$pr" --arg head "$head" --arg new "$new" --arg title "$title" --arg url "$url" \
      '. + [{kind: "pr", number: ($pr|tonumber), title: $title, url: $url, head: $head, method: "merge", result: $new}]' <<<"$applied_json")
  else
    commits=$(wgit rev-list --reverse --no-merges "HEAD..$head")
    [[ -n "$commits" ]] || die "PR #$pr has no commits outside the base"
    local picks="[]"
    for commit in $commits; do
      log "cherry-picking $commit from PR #$pr"
      wgit cherry-pick -x "$commit" >/dev/null 2>&1 ||
        conflict_error "commit $commit of PR #$pr ($url)" cherry-pick
      new=$(wgit rev-parse HEAD)
      map+="    - \`$commit\` -> \`$new\` $(wgit log -1 --format=%s "$new")"$'\n'
      picks=$(jq -c --arg from "$commit" --arg to "$new" '. + [{from: $from, to: $to}]' <<<"$picks")
    done
    applied_json=$(jq -c --arg pr "$pr" --arg head "$head" --arg title "$title" --arg url "$url" --argjson picks "$picks" \
      '. + [{kind: "pr", number: ($pr|tonumber), title: $title, url: $url, head: $head, method: "cherry-pick", commits: $picks}]' <<<"$applied_json")
  fi
  notes_inputs+="- PR [#$pr]($url) ($state): $title"$'\n'"  head \`$head\`, applied by $method:"$'\n'"$map"
}

apply_commit() {
  local want=$1 full parents subject new
  full=$(ensure_commit "$want")
  parents=$(wgit rev-list --parents -n1 "$full" | wc -w)
  [[ "$parents" -eq 2 ]] || die "commit $full is a merge commit or a root commit; pass its PR or individual commits"
  if wgit merge-base --is-ancestor "$full" HEAD; then
    die "commit $full is already contained in the base; drop --commit $want"
  fi
  subject=$(wgit log -1 --format=%s "$full")
  log "cherry-picking commit $full"
  wgit cherry-pick -x "$full" >/dev/null 2>&1 || conflict_error "commit $full ($subject)" cherry-pick
  new=$(wgit rev-parse HEAD)
  notes_inputs+="- commit \`$full\` -> \`$new\`: $subject"$'\n'
  applied_json=$(jq -c --arg from "$full" --arg to "$new" --arg subject "$subject" \
    '. + [{kind: "commit", from: $from, to: $to, subject: $subject}]' <<<"$applied_json")
}

for input in ${inputs[@]+"${inputs[@]}"}; do
  case "$input" in
    pr:*) apply_pr "${input#pr:}" ;;
    commit:*) apply_commit "${input#commit:}" ;;
  esac
done
[[ -n "$notes_inputs" ]] || notes_inputs="- none (plain build of the base)"$'\n'

build_sha=$(wgit rev-parse HEAD)
short_sha=$(wgit show -s --format=%h HEAD)
commit_time=$(wgit show -s --format=%ct HEAD)
[[ -z "$(wgit status --porcelain)" ]] || die "build worktree $worktree is not clean"

# check_goreleaser_drift fails when the base's .goreleaser.yml no longer
# builds what this script replicates.
check_goreleaser_drift() {
  local cfg="$worktree/.goreleaser.yml" mains want
  [[ -f "$cfg" ]] || die "base has no .goreleaser.yml; cannot mirror the upstream release build"
  mains=$(sed -n 's/^[[:space:]]*-\{0,1\}[[:space:]]*main:[[:space:]]*\([^[:space:]]*\).*/\1/p' "$cfg" | LC_ALL=C sort | tr '\n' ' ')
  [[ "$mains" == "./cmd/gc ./cmd/gc-osc7501 ./cmd/gc-write-mint " ]] ||
    die ".goreleaser.yml now builds [$mains]; update $prog to match before building"
  for want in \
    '- -s -w -X main.version={{ .Tag }} -X main.commit={{ .Commit }} -X main.date={{ .Date }} -X github.com/gastownhall/gascity/internal/productmetrics.compiledReleaseTag={{ if .IsSnapshot }}{{ .Version }}-canary-{{ .ShortCommit }}{{ else }}{{ .Version }}{{ end }}' \
    '- -s -w -X main.version={{ .Tag }}' \
    '- CGO_ENABLED=0' \
    'name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"' \
    'name_template: "{{ .ProjectName }}_{{ .Version }}_checksums.txt"' \
    'algorithm: sha256'; do
    grep -qF -- "$want" "$cfg" || die ".goreleaser.yml no longer contains '$want'; update $prog to match before building"
  done
  grep -qE '^[[:space:]]*- -s -w[[:space:]]*$' "$cfg" ||
    die ".goreleaser.yml no longer builds gc-write-mint with '-s -w'; update $prog to match before building"
  ! grep -qE '^[[:space:]]*(flags|tags|asmflags|gcflags|files):' "$cfg" ||
    die ".goreleaser.yml now sets build flags, tags or archive files; update $prog to match before building"
}
check_goreleaser_drift

rm -rf "$out_dir"
mkdir -p "$out_dir/bin" "$out_dir/stage"
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
release_tag_ldflag="$version-canary-$short_sha"
go_version=$(cd "$worktree" && env -u GOFLAGS "$go_bin" env GOVERSION)

build_one() {
  local main=$1 binary=$2 ldflags=$3
  log "building $binary ($go_version, linux/amd64)"
  (cd "$worktree" &&
    env -u GOFLAGS CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 \
      "$go_bin" build -ldflags "$ldflags" -o "$out_dir/bin/$binary" "$main") ||
    die "go build $main failed"
}
build_one ./cmd/gc gc "-s -w -X main.version=$version -X main.commit=$build_sha -X main.date=$build_date -X github.com/gastownhall/gascity/internal/productmetrics.compiledReleaseTag=$release_tag_ldflag"
build_one ./cmd/gc-osc7501 gc-osc7501 "-s -w -X main.version=$version"
build_one ./cmd/gc-write-mint gc-write-mint "-s -w"
[[ -z "$(wgit status --porcelain)" ]] || die "the build modified $worktree"

log "checking the built gc"
clean_env=(env -i HOME="$HOME" PATH="$(dirname "$(command -v "$go_bin")"):/usr/bin:/bin")
gc_version=$("${clean_env[@]}" "$out_dir/bin/gc" version) || die "gc version failed"
gc_version_json=$("${clean_env[@]}" "$out_dir/bin/gc" version --json --long) || die "gc version --json --long failed"
[[ "$(jq -r '.version' <<<"$gc_version_json")" == "$version" ]] ||
  die "built gc reports version $(jq -r '.version' <<<"$gc_version_json"), expected $version"
[[ "$(jq -r '.commit' <<<"$gc_version_json")" == "$build_sha" ]] ||
  die "built gc reports commit $(jq -r '.commit' <<<"$gc_version_json"), expected $build_sha"
if [[ -x "$worktree/scripts/verify-release-binary-metadata.sh" ]]; then
  "${clean_env[@]}" "$worktree/scripts/verify-release-binary-metadata.sh" "$out_dir/bin/gc" "$build_sha" "$version" >&2 ||
    die "scripts/verify-release-binary-metadata.sh rejected the built gc"
fi

log "packaging $archive"
cp -f -p "$out_dir/bin/gc" "$out_dir/bin/gc-osc7501" "$out_dir/bin/gc-write-mint" "$out_dir/stage/"
shopt -s nullglob nocaseglob
for doc in "$worktree"/LICENSE* "$worktree"/README* "$worktree"/CHANGELOG*; do
  [[ -f "$doc" ]] && cp -f "$doc" "$out_dir/stage/"
done
shopt -u nullglob nocaseglob
members=()
while IFS= read -r member; do
  members+=("$member")
done < <(cd "$out_dir/stage" && LC_ALL=C ls -1)
LC_ALL=C tar -C "$out_dir/stage" --sort=name --owner=0 --group=0 --numeric-owner --mode=go-w \
  --mtime="@$commit_time" -cf - "${members[@]}" | gzip -n -9 >"$out_dir/$archive"
(cd "$out_dir" && sha256sum "$archive" >"$checksums")
sha256=$(awk '{print $1}' "$out_dir/$checksums")
rm -rf "$out_dir/stage"

base_subject=$(wgit log -1 --format=%s "$base_sha")
base_line="\`$base_sha\`"
[[ "$base" == "$base_sha" ]] || base_line="\`$base\` at $base_line"
cat >"$out_dir/notes.md" <<EOF
gc \`$version\`: a fork build of [$upstream_repo](https://github.com/$upstream_repo), not an upstream release.

- **Base:** $base_line ($base_subject)
- **Built commit:** \`$build_sha\` (branch \`$branch\` on $fork_repo)
- **Applied on top:**
$(sed 's/^/  /' <<<"${notes_inputs%$'\n'}")
- **Build:** \`$prog\` on ucc/tools, mirroring upstream's .goreleaser.yml (CGO_ENABLED=0, linux/amd64, $go_version, release ldflags)
- **\`gc version\`:** \`$gc_version\`

sha256 of \`$archive\`:

\`\`\`
$sha256
\`\`\`

linux/amd64 only.
EOF

jq -n --arg version "$version" --arg tag "$tag" --arg sha256 "$sha256" --arg archive "$archive" \
  --arg checksums "$checksums" --arg base "$base" --arg base_sha "$base_sha" --arg build_sha "$build_sha" \
  --arg branch "$branch" --arg go_version "$go_version" --arg gc_version "$gc_version" \
  --arg build_date "$build_date" --argjson applied "$applied_json" --argjson dry_run "$dry_run" \
  '{version: $version, tag: $tag, sha256: $sha256, archive: $archive, checksums: $checksums,
    base: $base, base_sha: $base_sha, build_sha: $build_sha, branch: $branch, applied: $applied,
    go_version: $go_version, gc_version: $gc_version, build_date: $build_date, dry_run: $dry_run}' \
  >"$out_dir/build-info.json"

release_url_out="(dry run: nothing pushed or published)"
if ! $dry_run; then
  title="gc $version"
  if $replace; then
    log "pushing $branch to $fork_remote (replacing)"
    wgit push --quiet --force "$fork_remote" "$build_sha:refs/heads/$branch"
  else
    log "pushing $branch to $fork_remote"
    wgit push --quiet "$fork_remote" "$build_sha:refs/heads/$branch"
  fi
  if [[ -n "$release_url" ]]; then
    log "updating release $tag in place"
    wgit push --quiet --force "$fork_remote" "$build_sha:refs/tags/$tag"
    gh release upload "$tag" --repo "$fork_repo" --clobber "$out_dir/$archive" "$out_dir/$checksums" ||
      die "gh release upload $tag failed"
    gh release edit "$tag" --repo "$fork_repo" --prerelease --title "$title" --notes-file "$out_dir/notes.md" >/dev/null ||
      die "gh release edit $tag failed"
  else
    if [[ -n "$remote_tag_sha" ]]; then
      wgit push --quiet --force "$fork_remote" "$build_sha:refs/tags/$tag"
    fi
    log "creating prerelease $tag"
    gh release create "$tag" --repo "$fork_repo" --target "$build_sha" --prerelease \
      --title "$title" --notes-file "$out_dir/notes.md" "$out_dir/$archive" "$out_dir/$checksums" >/dev/null ||
      die "gh release create $tag failed"
  fi

  log "verifying the published release"
  published_tag_sha=$(rgit ls-remote "$fork_remote" "refs/tags/$tag" | awk '{print $1}')
  [[ "$published_tag_sha" == "$build_sha" ]] ||
    die "tag $tag on $fork_repo points at ${published_tag_sha:-nothing}, expected $build_sha"
  release_json=$(gh release view "$tag" --repo "$fork_repo" --json url,isPrerelease) ||
    die "gh release view $tag failed after publishing"
  [[ "$(jq -r '.isPrerelease' <<<"$release_json")" == true ]] || die "release $tag is not marked prerelease"
  release_url_out=$(jq -r '.url' <<<"$release_json")
  rm -rf "$out_dir/download"
  gh release download "$tag" --repo "$fork_repo" --pattern "$archive" --dir "$out_dir/download" ||
    die "gh release download $tag failed"
  [[ "$(sha256sum "$out_dir/download/$archive" | awk '{print $1}')" == "$sha256" ]] ||
    die "published $archive does not match sha256 $sha256"
  rm -rf "$out_dir/download"
  jq --arg url "$release_url_out" '. + {release_url: $url}' "$out_dir/build-info.json" >"$out_dir/build-info.json.tmp"
  mv -f "$out_dir/build-info.json.tmp" "$out_dir/build-info.json"
fi

succeeded=true
cat <<EOF
version:    $version
tag:        $tag
sha256:     $sha256
commit:     $build_sha
release:    $release_url_out
archive:    $out_dir/$archive
gc version: $gc_version
EOF
