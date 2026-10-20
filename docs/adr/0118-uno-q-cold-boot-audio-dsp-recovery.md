# ADR-0118: UNO Q cold-boot audio-DSP recovery — a shipped oneshot restarts the ADSP when its APR services never register (issue #24)

Date: 2026-10-20
Status: accepted
Owners: issue-24 worker (task issue24)
Related requirements/tests: issue #24 (UNO Q cold boot: the audio DSP's APR services sometimes never register, so there is no sound card), docs/spec/10 ("UNO Q software deployment" — service managed by systemd; hardware preflight), docs/spec/13 (diagnostics page and CLI — the `audio.dsp` doctor row), docs/spec/15 (Debian/systemd package), ADR-0081 (first-install boot enablement), ADR-0053 (Arch packaging parity), ADR-0114 (narrator audio over the hub HDMI via the ArduinoImolaHPH card), ADR-0111 (the hub-HDMI preflight rows), `packaging/systemd/game-narrator-audiodsp-recover.service`, `packaging/scripts/audiodsp-recover.sh`, `internal/doctor/audiodsp.go`, `internal/packaging/audiodsp_test.go`, docs/manuals/19 ("Troubleshooting: no sound card after a cold boot"), skills/uno-q-setup/references/audio.md

## Context and constraints

On the first cold power-on after a power cut (2026-10-20) the UNO Q came up
without its built-in sound card `ArduinoImolaHPH` (the hub HDMI audio path via
the ANX7625 bridge; ADR-0114): the ADSP booted ("adsp is now up") and the
glink channel `apr_audio_svc` existed, but APR never added its services, so
the deferred-probe chain (lpass pinctrl → soundwire → codecs → sound) stayed
deferred and no card appeared. The kernel-log signature of that chain is the
lpass pinctrl deferral line `qcom-sm6115-lpass-lpi-pinctrl: Failed to get clk
'audio'`. It reproduced on two consecutive boots; the proven fix is restarting
the ADSP remoteproc once (`echo stop` / `echo start` on its
`/sys/class/remoteproc/<adsp>/state` file) — the APR services registered at
once and card 1 appeared. Likely a cold-boot race between the in-kernel
qcom_pd_mapper / PDR notification for the ADSP audio PD and APR.

Board facts verified read-only on the real board (2026-10-20, issue #24
session):

- `/proc/device-tree/compatible` = `arduino,imola qcom,qcm2290 qcom,qrb2210`
  (NUL-separated) — the UNO Q marker.
- `/proc/asound/cards` healthy-boot line: ` 1 [ArduinoImolaHPH]: qcm2290 -
  Arduino-Imola-HPH-LOUT` — the bracketed name is the stable card name.
- `/sys/class/remoteproc/remoteproc1/name` = `adsp`, state `running`
  (remoteproc0 is the modem — the recovery must touch only the ADSP one).
- The deferral line also appears TRANSIENTLY on healthy boots (the deferred
  probe then resolves and the card appears): the signature alone is not a
  fault — the fault is the card's ABSENCE with the signature present.

A headless appliance that comes up deaf after a power cut is a silent product
failure: capture and decisions work, but no one hears the narration and
nothing in the normal flow notices. The fix must therefore be automatic,
bounded, safe on healthy boots and on other hardware, and the fault must be
diagnosable (doctor row + preflight row + docs).

## Options considered

1. **Detect-and-report only (doctor row + docs), no automatic action.**
   Rejected: the appliance stays deaf after every affected cold boot until a
   human sshes in — exactly the failure mode ADR-0081 closed for the service
   itself.
2. **A long-running watchdog service that polls for the card and restarts
   the ADSP when it is missing.** Rejected: a persistent process for a
   once-per-boot race; unbounded retries; a new unit whose own failures need
   diagnosis.
3. **A one-shot systemd unit (oneshot, WantedBy multi-user.target) whose
   helper decides and acts once per boot.** Chosen: systemd already provides
   the once-per-boot semantics, journal logging of the outcome, and the
   first-install enablement contract of ADR-0081; the helper carries the
   decision because systemd has no file-content `Condition` (the compatible
   string, the card list and the dmesg signature all need content matching).
4. **Fix it upstream (Arduino image / kernel patch) first.** Rejected as a
   substitute: out of our hands and slow; the appliance must be robust either
   way. The upstream report is a follow-up (issue #24, item 4), not a
   precondition.

## Decision

1. **`packaging/systemd/game-narrator-audiodsp-recover.service`** — a
   oneshot unit, `WantedBy=multi-user.target` (once per boot), ordered
   `After=sound.target`, with `ConditionPathExists=/proc/asound/cards` and
   `ConditionPathExists=/sys/class/remoteproc` so it is a no-op at the systemd
   level on non-ALSA / non-remoteproc hosts. `ExecStart` runs the helper as
   root (it must write the remoteproc `state` file and read the real dmesg;
   sandboxing directives like ProtectKernelLogs would blind the evidence it
   reads).
2. **`packaging/scripts/audiodsp-recover.sh`** (shipped to
   `/usr/libexec/game-narrator/`) carries the whole decision, with every root
   and timing env-overridable (`GN_AUDIO_PROC_ROOT`, `GN_AUDIO_SYS_ROOT`,
   `GN_AUDIO_SETTLE_SECS`=10, `GN_AUDIO_WAIT_SECS`=20, `GN_AUDIO_POLL_SECS`=1,
   `GN_AUDIO_DMESG_FILE`) so the logic is testable against fake /proc and /sys
   trees. The decision, with all its evidence:
   - settle ~10 s (healthy boots register the card a few seconds after the
     ADSP comes up);
   - not a UNO Q (no `arduino,imola` in `/proc/device-tree/compatible`) →
     logged no-op, exit 0;
   - `ArduinoImolaHPH` present in `/proc/asound/cards` → logged healthy
     no-op, exit 0 (idempotent; the transient signature line on healthy boots
     must not trigger a restart);
   - card absent but the dmesg signature `Failed to get clk 'audio'` absent →
     logged no-op, exit 0 (a different fault; do not blindly restart the ADSP
     again);
   - card absent + signature present → restart the ADSP remoteproc ONCE
     (stop, then start, on the `adsp`-named entry only), then wait up to 20 s
     for the card; exit 0 with a success log when the card appears, exit 1
     (visible in `systemctl status` and the journal) if it does not.
   Bounded by construction: one settle, one stop/start pair, one 20 s wait;
   the unit cannot loop.
3. **Deb enablement follows ADR-0081 exactly**: `packaging/deb/postinst.sh`
   enables the oneshot only on `$1 = install`; `packaging/deb/postrm.sh`
   disables it on remove/purge. An admin-disabled unit stays disabled across
   upgrades. The Arch hooks deliberately do NOT auto-enable (the Arch package
   never auto-enables `game-narrator.service` either — ADR-0053; the Arch
   admin enables per manual 19).
4. **`.goreleaser.yaml` nfpm contents**: the unit and the helper ship in both
   formats (`/lib/systemd/system` + `/usr/libexec/game-narrator` for the deb,
   `/usr/lib/systemd/system` + the same helper path for Arch), helper mode
   0755.
5. **Doctor row `audio.dsp`** (`internal/doctor/audiodsp.go`, registered
   after `audio.hdmi_bridge` in `internal/doctor/doctor.go`): read-only — it
   reports, never restarts. NOT CONFIGURED off the UNO Q (no `arduino,imola`
   in compatible; the dmesg probe is never even run); OK when the card is in
   `/proc/asound/cards`; FAIL when the card is absent AND dmesg carries the
   signature (the detail names the cause — APR never registered — and the
   remedy names the shipped oneshot); BLOCKED when the card is absent without
   the signature or dmesg is unreadable (a different fault; the remedy
   steers to dmesg + reboot). A new `Options.ProcRoot` seam (default `/proc`)
   makes the row testable against a fabricated device tree and card list.
6. **Preflight + skill + manual**: a read-only `audiodsp` row in
   `skills/uno-q-setup/scripts/preflight.sh` (same three-way decision: PASS /
   FAIL-with-fix / FAIL-other-fault), a troubleshooting entry in
   `skills/uno-q-setup/references/audio.md`, and a "no sound card after a
   cold boot" section in docs/manuals/19.
7. **Upstream**: if the race reproduces, report it to the Arduino image /
   kernel (issue #24, item 4) — tracked, not a gate.

## Positive consequences

- A headless appliance survives the cold-boot race by itself: the card is
  back seconds after boot, the journal shows exactly what happened (or a
  logged no-op proves a healthy boot), and the failure — when the bounded
  restart is not enough — is visible (`systemctl status`, doctor `audio.dsp`,
  preflight `audiodsp`) with the cause and the next step.
- The recovery is safe by construction: it cannot run on other hardware
  (compatible string + path Conditions), cannot act on a healthy boot (card
  present), cannot act on an unrelated fault (signature required), and cannot
  loop (oneshot, one stop/start, one bounded wait).
- The doctor, the preflight and the helper read the SAME evidence (the
  compatible string, `/proc/asound/cards`, the dmesg signature), so the
  diagnosis and the automatic fix agree by construction.

## Negative consequences / risks

- The ADSP restart interrupts anything using the DSP. On an affected boot
  there is no sound card, so nothing is using it — the window of risk is the
  (buggy) case only. A healthy boot never triggers the action.
- The deferral line persists in the dmesg ring buffer after a successful
  recovery; if the board is then rebooted into another bad state, the unit
  acts again (as intended). If a NON-remoteproc fault removes the card while
  the signature is still in the buffer, the unit does one harmless ADSP
  restart and then reports failure (exit 1) — the doctor row's BLOCKED/FAIL
  distinction plus the dmesg evidence is the diagnostic path.
- The 10 s settle delays a no-op by 10 s on every healthy boot (the oneshot
  exits ~10 s after boot); that is the price of not racing the healthy-boot
  card registration.
- A real fix still has to come from upstream; this ADR makes the appliance
  robust in the meantime, it does not remove the root cause.

## How to verify (commands and evidence)

- `go test ./internal/doctor/ ./internal/packaging/` (and `-race`): the
  `audio.dsp` row against fake /proc trees + recorded dmesg (NOT CONFIGURED
  off-board, OK with the card, FAIL with the signature, BLOCKED without it /
  with unreadable dmesg); the helper's decision logic against fake /proc +
  /sys (no-op off-UNO Q, healthy no-op, no-signature no-op, no-ADSP no-op,
  the bounded stop-then-start that fails without the card, the success path
  with the card appearing mid-wait); the postinst block's first-install-only
  enablement of both units (upgrade runs no enable). All passing 2026-10-20
  (issue #24 session).
- `sh -n` on the helper and the maintainer scripts (the
  `TestTmpfilesAndScriptsLint` target list now includes the helper).
- Read-only on the real board (2026-10-20): compatible string,
  `/proc/asound/cards` (card 1 present), `remoteproc1` = adsp/running,
  `sound.target` active, `dmesg` readable from the ssh user (adm group) — the
  doctor `audio.dsp` row reports OK on the healthy board.
- NOT RUN (needs a fresh package install + a cold power cycle, and must not
  run while the board is in use): the oneshot on the appliance and the
  recovery itself. Reproduction is weather-dependent (the race hit 2 of 2
  boots on 2026-10-20 after the power cut); when it reproduces:
  `journalctl -u game-narrator-audiodsp-recover.service` must show the
  restart and the success log, and `doctor` must flip `audio.dsp` to OK.

## Security / compatibility implications

- New package files only (unit + 0755 helper); the helper runs as root once
  per boot and writes only the ADSP remoteproc `state` file — no other state
  is touched, no user data is read.
- The unit is inert on non-UNO Q hardware (Conditions + the compatible
  string check) and on healthy boots (card present).
- No protocol, persistence or provider change; no schema change.

## Rollback / migration path

`systemctl disable game-narrator-audiodsp-recover.service` stops the
automatic recovery (the manual stop/start commands in the docs remain the
workaround); the package's postrm already disables it on removal. Removing
the unit files and the helper from the nfpm contents and reverting
`postinst.sh`/`postrm.sh` restores the pre-ADR-0118 package; the doctor row
and the docs degrade gracefully (they only NAME the oneshot as the remedy).
No data migration.

## Links / references

- Issue #24 (the observed fault, the manual fix, the signature, the upstream
  follow-up).
- ADR-0081 (first-install enablement contract), ADR-0053 (Arch packaging),
  ADR-0111 (hub-HDMI preflight), ADR-0114 (the hub-HDMI ALSA card).
- `skills/uno-q-setup/references/audio.md` (board audio facts),
  docs/manuals/19 (install + troubleshooting), docs/manuals/18 (first-boot
  preflight).
