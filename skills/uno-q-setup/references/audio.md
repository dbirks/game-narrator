# Audio output on the UNO Q

The dependable output on the UNO Q is **USB audio** (a USB DAC/speaker
on the powered hub). Bluetooth is **best-effort**: it works, but a
link-level stutter on A2DP SBC is known and unrepaired (issue #3).
Treat USB DAC as the baseline for any verification claim; Bluetooth is
an operator option with the caveats below.

## USB DAC baseline

- A USB DAC/speaker on the powered hub shows up as a UAC sound card
  (`cat /proc/asound/cards`) and as a PipeWire default-sink candidate
  (the preflight `audio` row checks for a sound card; no cards → WARN,
  which is the normal pre-arrival state).
- Set it as the default sink and verify with a chime and a TTS line:

  ```sh
  wpctl get-default sink                 # which sink is active
  aplay -l                               # ALSA cards
  pw-play /path/to/chime.wav             # a short chime
  pw-play /path/to/tts-line.wav          # a narration line
  ```

- Record the chosen sink name in a **comment line** in
  `/etc/game-narrator/env` (there is no app env variable for the sink —
  it is the OS default sink; see references/install.md).

## Bluetooth (best-effort)

`scripts/uno-q-setup/scripts/bluetooth-audio.sh` reports the state
(read-only) and, with `--apply`, applies the two user-level, reversible
prerequisites (`loginctl enable-linger` and the WirePlumber headless
override) for the audio-owning user. Pairing itself you drive with
`bluetoothctl` — the proven recipe, all of it hand-verified on the real
board (issue #2, #3 comments, 2026-10-19):

1. **Scan.** `bluetoothctl --timeout 25 scan on; bluetoothctl devices`
   lists the headphones by name; note the MAC.
2. **Pair/trust/connect** (most headphones have no PIN):

   ```sh
   printf 'agent NoInputNoOutput\ndefault-agent\npair <MAC>\ntrust <MAC>\nconnect <MAC>\n' | bluetoothctl
   ```

   Result should be `Paired: yes`, `Trusted: yes`, `Connected: yes`.
3. **Headless gotcha (required on a headless appliance).** WirePlumber
   0.5 creates no Bluetooth audio sink unless the user has an active
   local seat; over SSH only, the device pairs but **no sink appears**.
   Fix (user-level, reversible): `~/.config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf`
   containing

   ```ini
   wireplumber.profiles = { main = { monitor.bluez.seat-monitoring = disabled } }
   ```

   then `systemctl --user restart wireplumber`. The sink then appears
   as `bluez_output.<MAC>.1` in the `a2dp-sink` profile (SBC codec) and
   becomes the default sink. `bluetooth-audio.sh --apply` writes exactly
   this file.
4. **Linger (required).** Without `loginctl enable-linger <user>`, the
   user's PipeWire/WirePlumber stop when the last SSH session closes and
   the headphones drop with a "disconnected" prompt. Whatever user owns
   audio for the service must linger (or use a system-wide session).
   `bluetooth-audio.sh --apply` enables linger for the audio-owning
   user.
5. **Dual-mode headsets can come back over LE only.** After a drop,
   `bluetoothctl connect` may report success while bluetoothd only does
   GATT reads and the A2DP unit (`0000110b-…`) is missing
   (`br-connection-unknown`). Fix: put the headset in pairing mode,
   `remove <MAC>`, rescan forcing the classic transport
   (`bluetoothctl menu scan` → `transport bredr`), then `pair`, `trust`,
   `connect` again. The A2DP sink reappears and playback works.
6. **Verify** with a chime and a TTS line: `pw-play chime.wav`,
   `pw-play tts-line.wav`.

## Known limitation (issue #3) — report it, do not hide it

- **Stutter/crackle on A2DP SBC.** At the default quantum (1024) the
  SBC ACL packets leave the host in ~7-packet bursts followed by
  ~90–100 ms pauses — heard as crackle several times a second.
  `pw-metadata -n settings 0 clock.force-quantum 256` made the packets
  evenly spaced (10–19 ms) and was heard as "a bit better", but crackle
  remained. Suspects: Wi-Fi power-save and BT/Wi-Fi coexistence on the
  shared Qualcomm antenna, and QCA in-band-sleep. Still open.
- Latency: A2DP adds ~150–250 ms (issue #2).
- **Recommendation on record:** USB audio is the dependable output on
  the UNO Q; Bluetooth stays best-effort. In any run report, state which
  output you verified and carry the Bluetooth caveat when you used it.

## "Connected but no audio sink"

If `bluetoothctl` shows the device connected but `wpctl get-default
sink` is not the `bluez_output.…` sink, you hit the headless gotcha
(step 3): apply the override, `systemctl --user restart wireplumber`,
re-check. The operator UI is expected to surface this and offer the
re-pair path (issue #3).
