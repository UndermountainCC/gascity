# UndermountainCC fork tooling

Fork-only scripts on branch `ucc/tools` of UndermountainCC/gascity. They are
not meant for upstream (gastownhall/gascity); never open an upstream PR with
them.

## build-gc-release.sh

Builds `gc` for linux/amd64 from a base ref plus selected upstream PRs or
commits, packages it like an upstream release, and publishes it as a
prerelease on the fork. The city pins the result in the infra role
(`gascity_versions.gc`, `gascity_gc_release_tag`, sha256).

```sh
# from any checkout of the fork with remotes origin (fork) and upstream
scripts/ucc/build-gc-release.sh --name edge-7b5614ac7-8871 \
  --base 7b5614ac70d1031111f072370536bfd440199b34 --pr 8871 --dry-run
# then the same without --dry-run to publish
```

What it does:

1. Refuses (exit 3) when tag `gc-NAME`, its release, or branch
   `ucc/build/NAME` already exists on the fork, unless `--replace` is given.
2. Creates its own throwaway worktree `<repo>/worktrees/gc-build-NAME`
   detached at the base, then applies each `--pr` (cherry-pick of the PR's
   commits not in the base, with `-x`; or `--method merge`) and `--commit` in
   the order given. A change that does not apply cleanly stops the run; it is
   never resolved.
3. Builds `gc`, `gc-osc7501` and `gc-write-mint` exactly as upstream's
   GoReleaser config does for linux/amd64 (`CGO_ENABLED=0`, its ldflags,
   snapshot `compiledReleaseTag`), and fails if the base's `.goreleaser.yml`
   no longer matches what the script mirrors. `gc version` reports NAME;
   `scripts/verify-release-binary-metadata.sh` checks commit and VCS stamp.
4. Packages `gascity_NAME_linux_amd64.tar.gz` (the three binaries plus
   LICENSE, README.md, CHANGELOG.md, like upstream's archive) and
   `gascity_NAME_checksums.txt`, plus `notes.md` and `build-info.json`, in
   `<repo>/worktrees/gc-build-NAME-dist`.
5. Unless `--dry-run`: pushes the built commit to the fork as branch
   `ucc/build/NAME` (never `main`, never upstream), creates prerelease
   `gc-NAME` targeting that commit with the notes and both assets, then
   re-downloads the archive and checks its sha256.
6. Prints version, tag, sha256, commit and release URL.

Test: `scripts/ucc/build-gc-release_test.sh` runs the script against a tiny
stand-in module, local bare remotes and a stub `gh` (dry run, merge method,
conflicts, publish, re-run, `--replace`, wrong fork remote, GoReleaser drift).
