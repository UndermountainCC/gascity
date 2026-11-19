# herdr: grouping non-LLM sessions as panes of one shared tab

Status: implemented (`ensureGroupPlacement`). Scope: `internal/runtime/herdr`.

Problem: the herdr provider gives every session its own tab. Non-LLM
sessions such as `core.control-dispatcher` (a `gc convoy control --serve`
loop) get one tab each, so they clutter every rig workspace. The goal is an
opt-in setting that places these sessions as split panes of one shared tab per
workspace (for example a tab named `control`). gc creates the panes itself and
keeps track of them across restart, stop and close.

## 1. Today: where a session gets its tab, and how gc tracks it

- `Provider.start` computes `wsLabel, tabLabel := placementFor(...)`
  (`provider.go:143`; workspace = `GC_RIG` or town, tab = agent leaf/alias) and
  calls `p.place` under `p.mu` (`provider.go:144-146`).
- `place` (`provider.go:220`) sends a work dir inside a linked git worktree to
  `client.ensureWorktreePlacement` (`client.go:1435`, fork PR #1). Every
  other work dir goes to `client.ensurePlacement` (`client.go:1289`):
  find or create the workspace, close any stale tab with the same label, then
  `tabCreate` (`client.go:1242`). The new tab's root shell pane is the
  session's pane.
- Tracking is keyed by pane, not by tab. `bindPlacement` (`panebinding.go:148`)
  writes `GC_HERDR_PANE_ID`, `_TAB_ID`, `_WORKSPACE_ID`, the shell PID and
  start identity, and the launch mode into the metadata sidecar
  (`panebinding.go:29-42`). Start calls it before and after the launch
  (`provider.go:172`, `:199`). `resolveBinding` (`panebinding.go:111`) probes
  the pane live before trusting it, because pane ids get recycled.
- Stop calls `closeSessionPane` (`provider.go:640`), which runs
  `closeBoundPane` and then `herdr pane close <pane>` (`client.go:1090`). gc
  never closes a tab on Stop. herdr closes a tab when its last pane closes and
  removes an empty workspace.
- Restart is a fresh Start: the stale tab with the same label is closed and a
  new tab is created.

Pane-keyed tracking is why this change is small. Liveness, Peek, Nudge, Stop
and the reaper all work on the bound pane id and never look at which tab holds
it.

## 2. Config surface and which sessions qualify

- Opt-in key: the agent env variable `GC_HERDR_PANE_GROUP=<tab label>`
  (for example `control`). It is off by default. Unset or empty keeps
  today's behavior exactly.
- Scope comes from the existing env merge, so no schema change is needed:
  - per agent: `env` in `agent.toml` or in `[[agent]]`
  - per pack agent such as the dispatcher: `[[patches.agent]] name =
    "core.control-dispatcher"` with `env = { GC_HERDR_PANE_GROUP = "control" }`
  - per rig: rig agent overrides
  - city-wide: one patch per qualifying agent

  The provider reads the key from `cfg.Env`, as `placementFor` already does for
  `GC_RIG` and `GC_ALIAS`.
- A session qualifies only through this explicit flag. `prompt_mode=none` is
  not a usable signal, for two reasons:
  - LLM TUIs use it too (grok and other REPL adapters, see
    `internal/worker/builtin/profiles.go:514`, `:579`, `:695`, …).
  - `runtime.Config` does not carry PromptMode to the provider.

  The provider makes no "is this an LLM" decision; that would be judgment in Go.
- Rejected alternatives:
  - A `config.Agent` field: it is herdr-specific and would need
    `TestAgentFieldSync` wiring in every copy and merge site.
  - A `[session.herdr]` city table: it needs `SessionConfig` plus
    registry plumbing, and it cannot express per-agent selection on its own.
    It can be added later as a default on top of the env key.

## 3. Mechanism

New `client.ensureGroupPlacement(ctx, wsLabel, group, paneLabel, cwd, env)`.
`place` calls it first when `GC_HERDR_PANE_GROUP` is set.

1. Find or create workspace `wsLabel` (same as `ensurePlacement`). In a new
   workspace, rename the default tab to `group`; its root pane is the
   session's pane. Go to step 5.
2. `tab list --workspace` and look for a tab labeled `group`. If there is
   none, `tabCreate(ws, group, cwd, env)` and use its root pane. Go to step 5.
3. `pane list --workspace` and keep the panes in the group tab. Split the last
   one: `pane split <anchor> --direction down --cwd <workDir> --env K=V…
   --no-focus`. The new pane is the session's pane, with cwd and env set
   exactly as `tabCreate` sets them.
4. Only after the new pane exists, close the panes in that tab labeled
   `paneLabel` that are not the new pane: stale lives of this session. Closing
   first could empty the tab and make herdr remove it before the split. The
   worktree root-tab code follows the same rule (`client.go:1429`).
5. `pane rename <pane> <paneLabel>` (`paneLabel` = today's `tabLabel`), so
   humans and step 4 can identify it.

Return `agentInfo{WorkspaceID, TabID: groupTab, PaneID}`. `bindPlacement` and
everything downstream stay unchanged. `p.mu` already serializes concurrent
Starts in the same workspace, so two dispatchers cannot both create a group
tab.

- **Worktree workspaces (PR #1):** the group takes precedence over worktree
  placement. The group tab lives in the rig workspace `wsLabel` with the
  pane's cwd set to the worktree dir. One worktree workspace per dispatcher
  would defeat the grouping. Sessions without the key keep PR #1 behavior.
- **Stop or close:** unchanged, `pane close <bound pane>`. herdr reflows the
  remaining panes. When the last pane closes, the tab closes, and an empty
  workspace collapses as it does today.
- **Restart:** Start runs `ensureGroupPlacement` again. The new pane is split
  into the same tab and the old same-label pane is closed after it, so panes
  do not pile up.
- **herdr restore:** a server restart restores the saved layout, including
  the group tab and its panes, as fresh shells. Bound panes fail the
  shell-PID/identity probe and read as not running. The reconciler restarts
  them, and step 4 closes the restored same-label pane. Restored panes with no
  binding are handled as today (see the herdr-provider-design gotchas).

## 4. Failure modes and fallbacks

- **herdr lacks `pane split`, `pane rename` or `pane list` tab fields**
  (an unknown-command or unknown-option error code): fall back to
  `ensurePlacement`, today's own tab, with one stderr diagnostic per Start.
  This is the same pattern as the worktree fallback in `place`. Rename is
  required, not cosmetic, because stale cleanup depends on the label.
- **Anchor pane disappears between `pane list` and `split`**
  (`pane_not_found`): re-list once. If the tab is gone, create it. Never loop.
- **The user closes the whole group tab:** every grouped session's pane dies
  and its binding probe fails. Each restart then follows step 2 (create the
  tab) or step 3 (split).
- **The user moves a pane out of the tab by hand:** tracking is unaffected
  because it is pane-keyed. The next restart puts the session back in the
  group.
- **Many panes in one tab:** they get small. The group is meant for a handful
  of sessions per workspace. The split direction is fixed (`down`) and there
  is no rebalancing in Go.
- **To verify live before merge:** on herdr 0.9.5, that closing the last
  pane closes its tab, and that pane labels survive layout restore. If labels
  do not survive, step 4 also closes a restored pane whose id equals the
  sidecar's prior `GC_HERDR_PANE_ID`; a crashed life keeps its sidecar.

## 5. Tests, size, upstream fit

- **Unit tests** against the fake herdr CLI (`fakeherdr_test.go`, extended
  with `pane split`, `pane rename` and tab-scoped `pane list`), in
  `placement_test.go`:
  - key unset: unchanged
  - first session creates the `control` tab
  - second session splits into it
  - restart closes the stale pane only after the split
  - split or rename unsupported: falls back to a tab, with the diagnostic
  - worktree work dir plus key: lands in the group in the rig workspace
  - concurrent Starts: one group tab
- **Live test:** add a gated case to `provider_live_test.go`: two grouped
  sessions, Stop both, and the tab is gone.
- **Bazel:** `bazel test //internal/runtime/herdr:herdr_test`, plus
  `bazel test //...` for the herdr resource census. New subprocess verbs may
  need a census count bump, as in commit `038b8fcfd`.
- **Docs:** a section in `internal/runtime/herdr-provider-design.md`.
- **Size:** about 110 lines in `client.go`, about 15 in `provider.go`, about
  60 in the fake herdr, about 220 in tests.
- **Upstream fit:** a good small PR to gastownhall/gascity. It is opt-in, local
  to the provider, and adds no config schema or wire change. Only the
  worktree-precedence rule depends on fork PR #1, so the upstream version
  drops that bullet if #1 has not landed. Prior art: none found.
  `gh search` on gastownhall/gascity issues and PRs for "herdr tab", "pane
  split" and "herdr" shows only lifecycle, startup and nudge fixes such as
  #9281, #9250 and #8934. #8934 (concurrent same-rig Starts crossing
  identities) touches the same `place` path and should land first or be
  rebased onto.
