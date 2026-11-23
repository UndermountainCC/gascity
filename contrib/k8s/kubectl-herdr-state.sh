#!/bin/sh
# kubectl-herdr-state.sh: record the in-pod agent's state for a herdr mirror tab.
#
# Usage: kubectl-herdr-state.sh idle|working|blocked
#
# Writes one line "<state> <unix-seconds>" to $KUBECTL_HERDR_STATE_FILE, else
# /tmp/kubectl-herdr/state, atomically: a temp file in the same directory,
# then a rename over the state file. The kubectl-herdr supervisor in the mirror
# tab reads that file and reports the state to herdr.
#
# Claude Code hooks run this on every prompt and tool call, so it never fails
# a hook: it always exits 0, prints nothing on stdout (a SessionStart or
# UserPromptSubmit hook's stdout reaches the agent's context), and says what
# went wrong on stderr only.
#
# `date +%s` is not strictly POSIX, but coreutils and busybox date both have it.

state=${1-}
case $state in
idle | working | blocked) ;;
*)
	echo "kubectl-herdr-state: unknown state \"$state\" (want idle, working or blocked)" >&2
	exit 0
	;;
esac

f=${KUBECTL_HERDR_STATE_FILE:-/tmp/kubectl-herdr/state}
tmp=$f.tmp.$$
if mkdir -p "$(dirname "$f")" 2>/dev/null &&
	{ printf '%s %s\n' "$state" "$(date +%s)" >"$tmp"; } 2>/dev/null &&
	mv -f "$tmp" "$f" 2>/dev/null; then
	exit 0
fi
rm -f "$tmp" 2>/dev/null
echo "kubectl-herdr-state: cannot write $f" >&2
exit 0
