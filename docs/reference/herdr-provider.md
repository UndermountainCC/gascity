---
title: "herdr Session Provider"
---

[herdr](https://herdr.dev) is a terminal workspace manager built for AI coding
agents. Gas City ships a native **herdr** session-provider backend as an
**opt-in** alternative to tmux: one shared herdr session-server per city, one
workspace per rig (and one for the town), and one tab per agent. tmux stays the
default backend and the fallback — herdr is additive, selected through the same
runtime-selection setting that picks tmux, k8s, ssh, or exec.

## Prerequisites

Install the `herdr` binary and make sure it is on `PATH`:

```bash
herdr --version   # 0.9.5 is the recorded live validation; read the version note below
```

The backend is registered as a builtin runtime name (`herdr`) — no pack or
`[runtimes.*]` declaration is needed. If the binary is missing, sessions
selected onto herdr fail to start; install it before flipping the selector.

**Pin a herdr version in any shared environment**, and check herdr's own release
list rather than trusting a version named here. The adapter talks to herdr's CLI,
and minor herdr releases have changed that surface twice: 0.7.4 and 0.7.5 altered
commands the original adapter depended on, and 0.8.0 renamed the code herdr
returns for a pane with no named agent. That second one cost a fallback:
when the agent-prompt call fails on a pane with no registered agent, the adapter
pastes the text instead, and under the renamed code it stopped recognising that
case, so the original failure was returned to the caller and the paste never
happened. Nudge delivery surfaced an error rather than going quiet, but nothing
in the build or the default test suite went red, so the regression had to be
noticed in the field.

The adapter reads 0.8's spelling now. One gap is known and tracked: 0.8.0 made
herdr's agent registry detection-based, so the live journey that drives herdr's
session-event stream waives itself when the installed herdr reports 0.8 or later
rather than asserting against a registry it no longer matches
([#5808](https://github.com/gastownhall/gascity/issues/5808)). That waiver reads
the version, not the registry: a herdr whose version cannot be read or parsed
runs the journey, and on 0.8 its registry assertions then fail. The other
real-herdr journeys (provider lifecycle, pane binding, activity, and the runtime
conformance suite) do run on 0.8; the kind-launch journey needs an installed
`claude` binary as well and skips without one.

The recorded live validation is herdr 0.9.5 against `main` at `0bd038188e`
(2026-11-06): `make test-herdr-live` passes apart from the #5808 waiver. gc
v1.5.1 fails the kind-launch journey on 0.9.5: herdr reports a freshly launched
Claude as blocked on a startup dialog, and v1.5.1 returns `agent_not_ready`
instead of answering it. Run 0.9.x with a gc that includes
[#8478](https://github.com/gastownhall/gascity/pull/8478). A live city on that
build still crosses session identities when two sessions start in the same rig
workspace at once
([#8934](https://github.com/gastownhall/gascity/issues/8934)), so treat rig
dispatch on herdr as unvalidated.

That is the reason to pin. The tests that drive a real herdr binary are opt-in
(`make test-herdr-live`); the default suite runs against a fake, so a change on
herdr's side cannot fail the build. After a herdr upgrade, run
`make test-herdr-live` and then put a session through a full work cycle before
trusting it.

## Enabling herdr

`herdr` is selected with the same runtime selector used for every other
backend. That selector is city-wide (`[session] provider`) or, for a one-off
run, process-wide (`GC_SESSION`); herdr cannot be selected for individual
agents.

### City default

Set the session provider in `city.toml`:

```toml
[session]
provider = "herdr"
```

Every agent the city starts runs under herdr, except agents on the ACP transport
(whether pinned `session = "acp"` or because their provider defaults to ACP),
which route to the separate ACP backend instead (see below).

### Per-agent / per-rig

herdr cannot be selected for individual agents. The runtime backend is chosen
city-wide by `[session] provider` (above) or process-wide by `GC_SESSION`
(below); no patch puts an agent onto herdr when the city default is something
else.

The per-agent patch field is `session`, but it selects a **transport**, not a
backend. It accepts only `acp`, `tmux`, or omission (`IsValidSessionTransport`
in `internal/config/provider.go`), so `session = "herdr"` never selects the
herdr runtime. Config validation flags it as a warning:

```text
agent "dog-1": session "herdr" is not a valid session transport (use "acp", "tmux", or omit)
```

Under a herdr city, the transport router (`internal/runtime/auto`) sends only
ACP-registered sessions to the separate ACP backend and routes everything else
to the city's base provider, which is herdr. Two consequences follow:

- `session = "acp"` (or a provider that defaults to ACP) moves that agent off
  herdr, onto the ACP backend. It is the one per-agent lever that changes which
  backend an agent runs on.
- `session = "tmux"` does not keep an agent on tmux. The herdr provider does not
  implement the transport-capability check, so the pin is neither honored nor
  rejected; the agent falls back to the base provider and runs on herdr. To put
  an agent on tmux, the whole city (or process) must default to tmux.

### Environment (one-off)

For a quick local trial without editing config, export the selector:

```bash
export GC_SESSION=herdr
gc start <city>
```

`GC_SESSION` overrides the effective provider name for that process, the same
way it selects `exec:<script>` or any other backend.

## Using herdr as the hybrid local leg

The `hybrid` session provider runs some sessions locally and sends the rest to
a remote backend, chosen by session name. Its two legs are tmux (local) and
k8s (remote) by default. To run the local sessions on herdr while matching
sessions still start in k8s pods, name the legs in `[session.hybrid]`:

```toml
[session]
provider = "hybrid"
remote_match = "pod-"   # sessions whose name contains "pod-" go to the remote leg

[session.hybrid]
local = "herdr"         # every other session runs on herdr
remote = "k8s"          # the default; may be omitted
```

| Setting | Effect |
| --- | --- |
| `remote_match` | A session whose name contains this substring goes to the remote leg. |
| `GC_HYBRID_REMOTE_MATCH` | Overrides `remote_match` when set to a non-empty value. |
| `local` | Runtime for every other session. Empty means `tmux`. |
| `remote` | Runtime for matching sessions. Empty means `k8s`. |

Each leg takes a builtin runtime name (`tmux`, `herdr`, `k8s`, `subprocess`,
and so on) or an `exec:` or `ssh:` selection. Startup fails with an error
naming the key and value for `hybrid` itself, a name that is not a builtin
runtime (including pack-declared `[runtimes.<name>]` runtimes), a value with
surrounding whitespace, or two legs that name the same runtime. An unknown name
never falls back to tmux.

With a herdr local leg, `gc init` checks that herdr is installed and asks for
tmux only if the other leg needs it. A change to `[session.hybrid]` takes
effect after the city restarts. The `hybrid` provider stays
unavailable under the v2 session reconciler whichever legs it runs. With a
k8s remote leg, herdr can also show the remote sessions: see
[Remote sessions in herdr](#remote-sessions-in-herdr-mirror-tabs).

The generated reference lists both keys under
[`HybridSessionConfig`](/reference/config#hybridsessionconfig).

## Remote sessions in herdr (mirror tabs)

A hybrid city with a herdr local leg shows its local agents in herdr, but the
sessions it sends to k8s pods run out of sight. A **mirror tab** brings each
one back: a herdr tab attached to the pod's terminal, showing the agent as
working, idle, or blocked like any local tab.

### Turning it on

Mirror tabs need three things, and no config key:

- `[session] provider = "hybrid"` with `[session.hybrid] local = "herdr"` and
  the remote leg `k8s` (the default).
- `kubectl-herdr` installed on the `PATH` of the `gc` process that runs the city. Without it every remote session
  still starts, and each start logs one line:

  ```text
  gc: hybrid: kubectl-herdr not found on PATH; skipping herdr mirror tab for session "rig--pod-worker"
  ```

- An agent image that records the agent's state (see
  [Agent state in the pod](#agent-state-in-the-pod)). Without it the tab still
  mirrors the terminal but reports no agent state.

Any other leg combination runs exactly as before: nothing is looked up or
logged.

### What gc does

| When | gc |
| --- | --- |
| A remote session starts | In the background, finds or creates the workspace `k8s:<namespace>` in the city's herdr session and runs one `kubectl-herdr attach` for the session's pod, unless that workspace already has a tab labeled with the session name. |
| A remote session stops | Closes every tab labeled with the session name in that workspace. A stop that fails while the pod may still be running keeps the tab. |

The herdr session is the one the herdr leg uses: the city name, else
`default`. The namespace and kube context are the k8s leg's own
(`GC_K8S_NAMESPACE`, default `gc`, and `GC_K8S_CONTEXT`). The attach targets
the same pod and tmux session the k8s leg created:

```bash
# -l: the k8s leg's session label; -c/--tmux-session: the agent container's
# tmux session; --workspace: the k8s:<namespace> workspace's id;
# --label: the tab label, which is the gc session name.
kubectl-herdr attach -n gc rig--pod-worker \
  -l gc-session=rig--pod-worker \
  -c agent --tmux-session main \
  --herdr-session city --workspace w2 \
  --label rig--pod-worker \
  --focus=false --output json
```

The attach runs off the session's start path under a 45-second budget, and
the close under a 10-second budget. A mirror failure is one log line starting
`gc: hybrid: herdr mirror tab for session "<name>":` and never fails the
session's start or stop.

### Agent state in the pod

kubectl-herdr's supervisor reads the agent's state from a file in the agent
container:

| Contract | Value |
| --- | --- |
| Path | `$KUBECTL_HERDR_STATE_FILE`, else `/tmp/kubectl-herdr/state` |
| Content | one line, `<state> <unix-seconds>`, e.g. `working 1795000000` |
| States | `idle`, `working`, `blocked` |
| Write | atomic: a temp file in the same directory, then a rename |

The reference agent image (`contrib/k8s/Dockerfile.agent`) installs a writer
and the Claude Code hooks that call it, as managed settings that apply to every
agent user and run alongside gc's own hooks:

| Claude Code hook event | State |
| --- | --- |
| `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure` | `working` |
| `Notification` (matcher `permission_prompt`) | `blocked` |
| `Stop`, `StopFailure`, `SessionStart` (matcher `startup\|resume\|clear`) | `idle` |

A custom image adds the same two files:

```dockerfile
USER root
COPY contrib/k8s/kubectl-herdr-state.sh /usr/local/bin/kubectl-herdr-state.sh
COPY contrib/k8s/claude-managed-settings.json /etc/claude-code/managed-settings.json
```

An image that already ships `/etc/claude-code/managed-settings.json` replaces
the reference file, so merge its `hooks` block into yours. An agent other than
Claude Code records its state by running the writer itself:

```bash
sh /usr/local/bin/kubectl-herdr-state.sh working
```

The writer always exits 0 and prints nothing on stdout, so a failed write never
breaks the agent's turn.

### Known limits

- A session already running when the city starts gets no mirror tab until it
  restarts, and a relaunch does not reopen a tab you closed.
- After the city restarts, stopping a session closes its whole tab, including
  any panes you split into it.
- The pod selector `gc-session=<session>` is not scoped to a city. Two cities
  sharing a namespace with a session of the same name match two pods, and the
  supervisor waits instead of attaching.
- Two cities with the same herdr session (both unnamed, so both `default`) and
  the same namespace share one mirror workspace, and same-named sessions
  collide on a tab label.
- The mirror shows one agent per container, as kubectl-herdr's default agent
  kind (`claude`); there is no per-agent override.

## Piloting safely

herdr is opt-in, and the backend is a whole-city choice, so you pilot it by
scoping which city runs on herdr, not by pinning individual agents. Recommended
path:

1. **Try it per-process on a scratch city.** Select herdr with the environment
   variable on a throwaway city, so nothing is committed and your real city is
   untouched:

   ```bash
   GC_SESSION=herdr gc start <scratch-city>
   ```

   Every agent in that process runs under herdr, except agents on the ACP
   transport (whether pinned `session = "acp"` or because their provider
   defaults to ACP), which still route to the separate ACP backend. Watch it
   through a normal work cycle.

2. **Promote to the scratch city's default.** Once the per-process trial looks
   good, set the default in that city's `city.toml` and run it end to end:

   ```toml
   [session]
   provider = "herdr"
   ```

3. **Widen** to your real city by flipping its `[session] provider` to
   `"herdr"`, once the scratch city has been stable across several work cycles.
   The switch is city-wide with no way to move agents over one at a time, so
   keep any city you are not ready to migrate on the tmux default.

## Applying and verifying

- Reload or restart the city to apply a selector change (`gc reload`, or restart
  the city). Agents launched after the switch run under herdr; already-running
  sessions keep their current backend until they next restart.
- Confirm the effective selector with `gc config show` (the `[session]`
  `provider` value, plus any agent `session` overrides).
- Once agents are on herdr, their workspaces and tabs are visible through
  herdr's own UI (`herdr` lists the per-rig/town workspaces and per-agent tabs).

## What is not wired yet

**The controller does not consume herdr's session-event stream.** herdr pushes
session events and the provider implements the stream
(`runtime.SessionEventProvider`), but the only consumer today is the provider's
own activity reporting. The reconciler discovers session state on its periodic
pass, so a session that goes away is noticed on the next pass rather than when the
event arrives. That costs latency rather than correctness, because each pass reads
live state rather than trusting an event. The delay is bounded by the reconcile
interval only while the controller keeps up: ticks run one at a time, so a busy
controller stretches it. That is the thing to watch during a pilot.

## Layout

Within the single per-city herdr session-server, gc places agents so the
workspace/tab structure mirrors the town:

- **One workspace per rig**, plus one for the town (mayor and town-level
  agents), named for the city. This holds under the default
  `session_template` (`{{.Agent}}`) too, where a town-level runtime name
  carries no city prefix.
- **One tab per agent.** Rig polecats land in their rig's workspace as
  `polecat-<themed-name>` tabs; the placement is display-only and does not
  change agent identity.

See `internal/runtime/herdr-provider-design.md` in the source tree for the
provider's design notes, capabilities, and pilot rationale.
