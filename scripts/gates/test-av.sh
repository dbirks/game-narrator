#!/bin/sh
# test-av.sh — the multimodal Dolphin A/V harness gate (ADR-0143).
#
# Runs TestLiveDolphinAVHarness (internal/e2e) under the test-av gate: the
# real Dolphin emulator + real model providers, the isolated PipeWire null
# sink (module-null-sink), the shared-clock FFmpeg capture (one .mp4 + one
# 16 kHz mono .wav from the same wall clock) of the Xvfb display AND the
# sink's monitor, the predictive-mode session driven through the first
# in-game conversation, the dev event stream correlated onto that shared
# clock into a per-line timeline (box appear → text settled → commit → Clef
# → TTS ready → audio start → audio end, with the acoustic RMS onset
# measured in the recorded track), the per-line multimodal clips and the
# per-clip ASR WER/CER.
#
# The gate's acceptance (avAssertReport): at least two committed lines, no
# stale start (the ADR-0122 backlog at the timeline level), no duplicate
# commit, and — unless GN_AV_NO_LATENCY_GATE=1 — the settled → audio p50 /
# p95 within the warm-run bounds (p50<=500 ms, p95<=800 ms). The fixture's
# lines are pre-synthesised into the speech cache (both voices) so the
# latency gate measures the session's own stages, not a TTS round trip.
#
# ADR-0170 (the conversation invariants): the report also carries a
# conversation-invariants section and the run gates on it — each page the
# player saw spoken exactly once (DUPLICATE), no read-ahead past the page
# on screen at the audio start (READ-AHEAD), pages spoken in the order
# shown (OUT-OF-ORDER), and — on the slow-reader drive — queue-don't-cut
# (CUT-OFF: the page's audio is not cut; and, when the player advanced
# mid-playback so the next page queued behind the playing one, it starts
# within 800 ms of the end — a waited advance's gap is a QUEUE-GAP report
# row, never a gate). The page-break gap rows (PAGE_BREAK_GAP) are report-only.
# GN_AV_DRIVE selects the drive: "rapid" (the default) or "slow" (A only
# after the current page's speech has ended + 500 ms).
#
# Exit codes (the gate convention, scripts/gates/run-go-gate.sh):
#   0  the run passed
#   1  the run failed (a gate assertion, or go test itself failed)
#   2  NOT RUN (a prerequisite absent on this host — the skip's setup line)
#
# Requires: GAME_NARRATOR_GAME_IMAGE_PATH, dolphin-emu / Xvfb / ffmpeg /
# x11-utils / tesseract / pactl on PATH, the verified first-dialogue
# fixture, and the shared gateway env (the OCR VLM fallback, the `decision`
# role, the TTS roles and the `speech_asr` role). Without any of them the
# test SKIPs as NOT RUN (this script then exits 2, never red).
#
# Artifacts (artifacts/ is gitignored):
#   artifacts/gates/test-av.log  the raw go test output
#   artifacts/gates/test-av.rc   the gate's final exit code (0/1/2)
set -u

mkdir -p artifacts/gates
log=artifacts/gates/test-av.log

GN_GATE=test-av go test -p 1 -count=1 -timeout 45m -v \
	-run TestLiveDolphinAVHarness ./internal/e2e/ >"$log" 2>&1
rc=$?

# Keep the output visible (the NOT RUN setup line, the per-line timeline
# summary) then classify: a NOT RUN skip is rc 2, a failure is rc 1, a
# pass is rc 0.
tail -n 40 "$log"
if [ "$rc" -ne 0 ]; then
	final=1
elif grep -q -- '--- SKIP: TestLiveDolphinAVHarness' "$log"; then
	final=2
elif grep -q -- '--- PASS: TestLiveDolphinAVHarness' "$log"; then
	final=0
else
	# go test exited 0 but the test neither passed nor was skipped
	# (nothing ran): treat it as a gate failure, never a green.
	final=1
fi
echo "$final" > artifacts/gates/test-av.rc
exit "$final"
