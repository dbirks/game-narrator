# ADR-0034: Dolphin bridge process supervision and input-proof timing — locked-thread spawns, bounded Stop, fast grabs, and a press budget
Date: 2026-10-17
Status: proposed
Owners: project
Related requirements/tests: docs/spec/11 "Avoiding false positives", docs/spec/12 "Emulator/hardware" (live input proof, no leftovers), ADR-0007 (Dolphin headless render + input), ADR-0012 (supervision); real test `TestRealDolphinBridge` / `TestRealAbruptContextKill` (internal/dolphin/real_test.go)

## Context and constraints

The orchestrator reproduced four real Dolphin-bridge bugs against the
uncommitted `internal/dolphin/` implementation (unit-green but wrong under
the real emulator), plus one test-helper false positive:

1. **Stop hung forever** — every stop closure did `select { <-done | timeout-kill }`
   and then an unconditional trailing `<-done`; on the normal path the select
   already consumed the single buffered reap, so the second receive blocked.
2. **Pdeathsig gotcha** — Linux delivers Pdeathsig when the *OS thread* that
   forked the child exits, not when the process exits; Go retires pooled
   threads at will, so a child spawned from an ordinary goroutine can be
   SIGKILLed at a random moment (golang/go#10743).
3. **Readiness predicate violated** — a frame was accepted while two
   successive frames were identical (`vs_prev=0`) and barely differed from the
   bare GUI, breaking the spec predicate (entropy > 0.1 **and** differs from
   the baseline **and** two successive frames differ).
4. **ProveInput failed** — the control window and the press ran on a ~6 s
   grab cadence, so the press landed ~60 s after the title, inside the
   attract movie, and the movie's own transition was (mis)read as the
   input's scene change. With correct 1 s timing a second, subtler failure
   appeared: the post-press scene (the save-file prompt) is a STABLE
   full-frame screen displayed over the title's own sky, so it measures
   ~0.20 RMSE against the temporally-adjacent no-input control reference —
   below the midpoint scene threshold (~0.29) — while the spike's
   "far from all title framings" clause measured 0.345 because its retained
   references were from a different orbit phase. A port that kept the far
   clause verbatim against an adjacent reference can never cross on this
   game; a port that drops it verbatim risks a static-title false pass.
5. **Test helper false positive** — the no-leftovers / no-parallel scans
   matched any `/proc/<pid>/cmdline` *containing* "dolphin-emu"/"Xvfb", so a
   shell running `pgrep dolphin-emu` tripped them.

Constraints: `internal/dolphin/` is the only code boundary; the image is
user-supplied and referenced read-only; the attract loop is a hard confounder
(the movie starts ~50–90 s after the title on its own); the host is an
llvmpipe dev box where the render window is 640x480 on a 1280x1024 private Xvfb.

## Decision

- **Supervision (bugs 1, 2).** Every supervised child (Xvfb, Dolphin) is
  started and reaped on ONE `runtime.LockOSThread()` goroutine that is held
  for the child's whole lifetime (`spawnSupervised`). The forking thread
  therefore lives exactly as long as the child, so Pdeathsig fires only on
  real parent death. `stopProcessGroup` signals the group with SIGTERM, waits
  a bounded grace, escalates to SIGKILL **only** on the leader if the group
  signal returned ESRCH (the child is not the group leader), and then reaps
  with a bounded timeout so Stop returns only once `/proc/<pid>` is gone.
  Tests spawn children through the same supervised path and assert
  `Getpgid(pid) == pid`, register a `t.Cleanup` SIGKILL, and assert the child
  is gone from `/proc` after Stop.
- **Readiness (bug 3).** `WaitForGameFrame` is spike-faithful (boot.sh): a
  full-root shot, entropy on the render-window crop (the spike's CROP),
  `vs_baseline` and `vs_prev` both made on the window crop of BOTH shots when
  a window exists. ALL THREE clauses must hold, and the first sample is never
  accepted (no predecessor). Thresholds: entropy > 0.1, `vs_baseline` > 0.01,
  `vs_prev` > 0.001 — a `vs_prev=0` sequence is rejected.
- **Input proof (bug 4).** The grab is ImageMagick `import` (the spike's
  ~80 ms native XGetImage) with the ffmpeg x11grab path kept only as a
  no-ImageMagick fallback; the 6 s ffmpeg cadence is what broke the 1 s
  cadence. The no-input control window is the spike's 15 samples at the true
  1 s cadence (~15 s). A `pressBudget` of 30 s bounds how long after the title
  is FOUND the START press may land, so the press + 15 s observation window
  finish inside the title-only span (movie earliest ~50 s). A title visit older
  than the budget, or a pre-press frame that already left the title (the
  movie), is **discarded, not pressed into**: the attract loop brings the
  title back and the next episode gets a fresh timer.
  - **Threshold anchor** — the spike's CONTROL_MAX is the *per-sample*
    distance of each no-input sample to its CLOSEST control neighbour
    (max over samples); the max pairwise distance straddles the whole orbit
    arc the 15 s window spans (measured 0.35 vs the spike's 0.25) and would
    systematically blunt the threshold.
  - **Crossing, two modes (input.sh's PASS rule).** A sample crosses in
    either mode, and PASS needs two CONSECUTIVE crossed samples: (a) the
    letterboxed rendered transition (two consecutive samples far from every
    control sample, letterbox band < 0.10, entropy > 0.20 — a pure-black
    fade frame or a single flicker cannot fake it); or (b) a SETTLED
    full-frame screen (two consecutive samples with letterbox band > 0.25,
    entropy > 0.20, and `step` < 0.10 against their predecessor). The
    settle mode deliberately carries no distance clause: the save-file
    prompt sits over the title's sky and is ~0.20 from the nearest adjacent
    control sample, so the distance clause (calibrated against the spike's
    non-adjacent references) would never fire here; the discriminating
    property is DYNAMICS — the title camera orbit moves 0.10-0.40 per 1 s
    sample (never settling), a real scene is stable. `step` is the spike's
    exact metric (RMSE of the 320x240-normalised luma between consecutive
    samples), so `crossStepMax = 0.10` is the spike's calibrated value.
  - **Settle-mode soundness is verified per episode, not assumed**: if the
    no-input control window itself shows a settled stretch
    (`hasSettledStretch` — a rendering stall or anomalous title), the
    episode is discarded (never accepted on it), and the episode is also
    discarded when the per-sample control distance already exceeds the
    0.40 title-vs-movie reference.
- **Test helper (bug 5).** The no-leftovers / no-parallel scans match the
  **executable basename** — `/proc/<pid>/comm` (kernel-truncated to 15 chars),
  falling back to the `argv[0]` basename — not a cmdline substring, so a shell
  merely running `pgrep dolphin-emu` no longer trips them.

## Alternatives considered

- **Document that Pdeathsig is fine without a locked thread.** Rejected: the
  kernel contract is per-thread, Go retires threads, and the observed leak was
  a real orphan. A locked thread is the smallest correct fix and matches the
  brief's primary recommendation.
- **`close`-based done channels in the stop closures.** Viable, but the
  buffered single-reap channel plus the bounded post-escalation reap is the
  smaller diff and is what the tests already assert.
- **Lower the input scene threshold to force a PASS.** Rejected (brief: fix
  the method, not the threshold). The timing root cause is the press inside
  the attract movie; the remaining gap (a settled prompt at ~0.20 vs a
  ~0.29 threshold) is fixed by porting the spike's settle case honestly —
  its calibrated property (a stable screen the orbit never produces) plus a
  per-episode no-input baseline check — not by moving the calibrated
  0.40 title-vs-movie reference.
- **Keep the ffmpeg grab and shorten the window.** Rejected: at 6 s/sample any
  window is too slow to stay inside the title-only span, and the orbit
  dispersion measured at a 6 s cadence (0.31) blunts the crossing.

## Consequences and risks

- Stop is bounded (grace + 5 s reap) and leaves no zombie; a non-group-leader
  child is still killed directly, so a Setpgid failure cannot leak a process.
- Pdeathsig now fires only on real parent death, so a child is never killed at
  a random moment; abrupt context cancel still tears down (covered by
  `TestRealAbruptContextKill`).
- The readiness predicate cannot be satisfied by a frozen or bare-GUI frame.
- The input proof is attributable: the press and its window land in the
  title-only span, so the observed scene change is the press's, not the
  attract movie's. A settled post-press scene is accepted even when it sits
  close to the title in RMSE (the prompt's case); a static title or a
  rendering stall CANNOT produce a false PASS — the no-input control window
  is checked for a settled stretch per episode and such an episode is
  discarded; a single transition or flicker frame cannot PASS (two
  consecutive crossed samples are required in both modes).
- Cost: one `import` per grab (~80 ms) instead of one ffmpeg (~6 s); the
  control + press + window span is ~35 s instead of ~110 s.

## Evidence and test plan

- `go test -race ./internal/dolphin/` — `TestStopProcessGroupChildExitsOnSIGTERM`,
  `TestStopProcessGroupChildIgnoresSIGTERM` (real short-lived children, Stop
  bounded, `/proc/<pid>` gone), `TestWaitForGameFramePredicate`,
  `TestWaitForGameFrameRejectsIdenticalSuccessives` (vs_prev=0 not accepted),
  `TestProveInputSceneChange` (letterboxed post-press scene, orbiting fake
  title), `TestProveInputSettledScene` (a STABLE post-press scene that sits
  below the scene threshold — accepted via the settle mode),
  `TestProveInputNoSceneChange`.
- Real: `go test -run Real ./internal/dolphin/` with
  `GAME_NARRATOR_GAME_IMAGE_PATH` set — asserts a real first game frame, a
  proven START press (scene change against a no-input control window), a live
  frame pipeline, and no dolphin-emu/Xvfb leftovers.
- Host measurements (dev box): `import -crop 640x480` ≈ 76–112 ms vs
  `ffmpeg x11grab` ≈ 6050 ms per still.

## Migration / rollback

No persisted state changes. Rollback: revert the `internal/dolphin/` edits
(`proc_unix.go`, `readiness.go`, `x11.go`, `frame.go`, `real_test.go`,
`proc_unix_test.go`) to the pre-ADR revisions. No data migration.

## Follow-ups

- If `import` is absent on a release host, the ffmpeg fallback re-introduces the
  6 s cadence; the press budget then discards episodes rather than pressing
  into the attract movie (a safe degradation, not a false PASS).
- Keep the grab cadence and press budget in the ADR record; revisit only with
  new host measurements.
- References: docs/spikes/dolphin-capability.md (lines 43–49, 109–116),
  scripts/spikes/dolphin/{boot.sh,input.sh,lib.sh}, ADR-0007, golang/go#10743.
