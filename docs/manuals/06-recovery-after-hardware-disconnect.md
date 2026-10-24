# Recovery after hardware disconnect (capture / audio)

## Purpose

Restore capture and/or audio after a device disappears (UVC dongle pulled,
card unplugged, UNO Q port reset), and verify the app noticed and recovered
the way docs/spec/13's resilience matrix requires: input marked offline,
detection paused, session metadata preserved, reconnect with backoff — not a
crash and not stale audio.

## Preconditions

- `bin/game-narrator doctor` (or `/diagnostics`) run **before** the
  reconnect so you have a baseline of what was `OK`.
- On the Debian service, the unit already grants device access via
  `SupplementaryGroups=video audio`; on a dev host the session user must be
  able to open `/dev/video*` and the sound card (see the dev-host notes in
  docs/dev-environment.md "Verified gaps").

## Steps — capture

1. Reconnect the hardware (reinsert the HDMI leg into the powered splitter,
   replug the UVC dongle into the UNO Q or dev host).
2. Confirm the kernel sees it and the user can open it:

   ```sh
   ls -l /dev/video*
   v4l2-ctl --list-devices
   ```

3. Re-run the probe:

   ```sh
   bin/game-narrator doctor        # capture.devices: device exists AND this user can open it
   ```

   The check is deliberately not "a `/dev/video*` node exists": a device the
   current user cannot open is reported as such (`capture device(s) exist
   but this user cannot open them`), which is the usual symptom after a
   group-membership change on a dev host.
4. Re-run the video test for the session that was running (docs/spec/13
   matrix: "Reinsert card, rerun video test").

## Steps — audio

1. Reconnect the USB DAC / USB sound device.
2. Check the audio server actually owns a card:

   ```sh
   pactl list short sinks      # the app plays to a real sink, not auto_null
   wpctl status
   ```

   On a dev host without seat access to the card (`/dev/snd/controlC0`
   owned by `root:audio`, user not in `audio`), PipeWire falls back to the
   null sink — that is an environment fix (add the user to `audio`, start a
   graphical seat), not an app fix; it is hardware-gate work
   (`make test-hardware`, docs/status/blocked.md).
3. Re-run `bin/game-narrator doctor` (`audio.sinks` check).

## Verification

- `doctor` reports `capture.devices` and `audio.sinks` `OK` again (exit 0,
  no new `FAIL`).
- The previously running session state is intact (`GET /api/v1/sessions`,
  the `/traces` timeline (old `/developer` URLs redirect there) shows the
  offline window and the resumed frames).
- A new frame observation and, where speech was flowing, new playback
  events appear after the reconnect.

**NOT YET IMPLEMENTED:** automated hotplug handling and the UNO Q
smoke/sustained-play gates (`make test-hardware` is `NOT RUN` — no UNO Q
hardware tests exist yet; docs/status/blocked.md). Everything above on real
UNO Q hardware is **NOT VERIFIED**.

## What NOT to do

- Never assume a fixed device node (`/dev/video0`) — the unit and the doctor
  probe by capability, never by name.
- Do not kill the service to "reset" a dropped device; the session is
  designed to survive the device being offline (metadata preserved, bounded
  buffering) — restart only if `doctor` shows the app itself as broken.
- Do not route audio to the null sink and call playback verified: the null
  sink proves bytes reached the audio server, not that anything was audible
  (docs/dev-environment.md "Verified gaps" 6).
