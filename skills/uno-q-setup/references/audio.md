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

## Audio over the hub HDMI (ANX7625 bridge)

The UNO Q's USB-C DisplayPort also carries audio into a hub with an
ANX7625 bridge chip and an HDMI out (verified into a Pioneer VSX-823;
its ELD is what the sink reports). Despite the sink's name
(`…HDMI__HDMI__sink`), the path that reaches the receiver is the
**default** ALSA route — MultiMedia4 → SEC_MI2S → ANX7625 → USB-C DP →
hub HDMI. Leave the mixer routes alone:
`SEC_MI2S_RX Audio Mixer MultiMedia4` (numid 207) **on**,
`DISPLAY_PORT_RX Audio Mixer MultiMedia4` (numid 135) **off**. The
DisplayPort route is a dead end (no DP backend dai-link in the qcm2290
device tree; enabling it gives `q6asm_dai_prepare: ret:-22`).

`aplay -D hw:1,3 tone.wav` through it is audible (user-confirmed on the
receiver). PipeWire's default negotiation on the same sink is **silent**
while the PCM runs (it picked S24_LE + MMAP + 10 ms periods + tsched,
and the bridge only reconfigures audio at stream start). Fix — force the
aplay-equivalent config with a user-level WirePlumber rule,
`~/.config/wireplumber/wireplumber.conf.d/82-hdmi-bridge.conf`:

```ini
monitor.alsa.rules = [
  {
    matches = [
      {
        node.name = "~alsa_output.platform-sound.HDMI.*"
      }
    ]
    actions = {
      update-props = {
        audio.format = "S16LE"
        audio.rate = "48000"
        audio.channels = "2"
        api.alsa.period-size = 6240
        api.alsa.period-num   = 4
        api.alsa.headroom = 0
        api.alsa.disable-mmap = true
        api.alsa.disable-tsched = true
        api.alsa.disable-batch = true
        session.suspend-timeout-seconds = 0
      }
    }
  }
]
```

then `systemctl --user restart wireplumber` (HDMI sink stays default).
`session.suspend-timeout-seconds = 0` keeps the stream and the receiver's
ELD lock alive between narrations (cost: ~520 ms buffer latency).

**From the narrator service (no user session).** The service user
(`game-narrator`) has no PipeWire session, so the rule above is the
operator-session form of the same property. The service plays the same
node in-process: sink `alsahw`, device `hw:ArduinoImolaHPH,3`, the exact
proven hw_params (ADR-0114, issue #5) — one writer goroutine owns the
stream, and the keep-alive (one silence period every ~200 ms while idle)
holds the ELD lock in place of the user session's no-suspend rule. The
Settings page lists the device as the "UNO Q hub HDMI (ANX7625)" row
(kind `hdmi`); `doctor`'s `audio.hdmi_bridge` row reads the mixer route
(SEC_MI2S_RX ← MultiMedia4 on, DISPLAY_PORT_RX ← MultiMedia4 off) and
names the exact `amixer` fix for a misroute; the hardware gate
(`make test-hardware`, `GAME_NARRATOR_HW_SMOKE=1` +
`GAME_NARRATOR_HW_AUDIO_CONSENT=1`) plays the 1 s tone and asserts
`hw_ptr` advanced. See manual 19, "Audio over the hub HDMI from the
service".

Verify without ears: with PipeWire playing a WAV, compare
`/proc/asound/card1/pcm3p/sub0/hw_params` against the aplay reference —
it must read `RW_INTERLEAVED / S16_LE / 2ch / 48000 / period_size 6240 /
buffer_size 24960`, state RUNNING, `hw_ptr` advancing, and `dmesg` shows
`q6asm_dai_prepare: pcm size 99840 period 4 period_sz 6240 vs 24960 rate
48000 ch 2` (byte-identical to aplay's line). If they match, the user
should hear the tone — ask them. Issue #5 has the full measurement
table.

## Bluetooth (best-effort)

The appliance's web UI does this itself (ADR-0091): Settings → Audio →
"Bluetooth output" runs the same scan / pair / trust / connect /
disconnect / forget / repair operations against the same bluetoothd over
D-Bus — no `bluetoothctl`, no PIN dialog (the NoInputNoOutput agent is the
product's; a PIN-requiring device gets the daemon's reason). The
`bluetoothctl` recipe below is the **human fallback** for pre-package
boards or a UI that is unreachable; both paths drive the same daemon, so
state does not split. The UI badges the issue-#3 state (connected, no A2DP
unit) as "needs repair" with a re-pair button, and at boot the service
holds the stored selection's headset connected (three tries, five seconds
apart) when it names a `bluez_output.<MAC>.1` sink. `doctor` carries a
`bluetooth` row (adapter, linger, headless override — degraded, not
failed, when the radio or session is absent).

`scripts/uno-q-setup/scripts/bluetooth-audio.sh` reports the state
(read-only) and, with `--apply`, applies the two user-level, reversible
prerequisites (`loginctl enable-linger` and the WirePlumber headless
override) for the audio-owning user. On packaged installs the .deb
postinst applies both for the `game-narrator` user (and the postrm
reverses the linger on purge); the script remains the pre-package path.
Pairing itself you drive with `bluetoothctl` — the proven recipe, all of
it hand-verified on the real board (issue #2, #3 comments, 2026-10-19):

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
   `connect` again. The A2DP sink reappears and playback works. The web
   UI's "Repair (re-pair over classic)" button performs exactly this
   sequence (forget, bounded rescan, re-pair) when the device is badged
   "needs repair".
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
re-check. The web UI (ADR-0091) surfaces the same state — the device row
is badged "needs repair" and its repair button re-pairs it; and the
distinct case of *connected, A2DP unit present, no node at all* is the
override missing: `doctor` names it and `bluetooth-audio.sh --apply` (or
the package postinst on a deb install) writes the file.

## "No sound card after a cold boot" (issue #24)

After the first power-on following a power cut, the board can come up
**without its built-in sound card**: `cat /proc/asound/cards` has no
`ArduinoImolaHPH` (the hub-HDMI audio path via the ANX7625 bridge), the
ADSP booted ("adsp is now up" in `dmesg`), but its APR services never
registered, so the lpass pinctrl → soundwire → codecs → sound probe
chain stays deferred. The `dmesg` signature of the hang is the lpass
deferral line `qcom-sm6115-lpass-lpi-pinctrl: Failed to get clk 'audio'`
(it can also appear transiently on HEALTHY boots — the card's absence is
the fault, the signature is the cause).

- **Automatic fix (the package ships it):** the oneshot
  `game-narrator-audiodsp-recover.service` runs once per boot (ADR 0118):
  after a short settle, on a UNO Q with the card absent and the signature
  present, it restarts the ADSP remoteproc once and waits up to 20 s for
  the card. Healthy boots and other hardware are logged no-ops. Check the
  outcome: `journalctl -u game-narrator-audiodsp-recover.service`.
- **Manual fix (the proven one, issue #24):** find the ADSP remoteproc
  (`/sys/class/remoteproc/*/name` is `adsp`; on the board it is
  `remoteproc1` — never `remoteproc0`, the modem) and restart it:
  ```sh
  echo stop  > /sys/class/remoteproc/remoteproc1/state
  echo start > /sys/class/remoteproc/remoteproc1/state
  ```
  The APR services register at once and card 1 `ArduinoImolaHPH`
  appears; verify with `cat /proc/asound/cards` and the doctor row
  `audio.dsp` (OK).
- **Diagnostics:** `sudo game-narrator doctor` — the `audio.dsp` row is
  FAIL with the cause and this remedy when the signature is present,
  BLOCKED when the card is absent without it (a different fault: check
  `dmesg` for the deferral chain, then reboot). The skill preflight's
  `audiodsp` row reports the same three-way state read-only.
- If it reproduces, file an upstream report (Arduino image / kernel):
  likely a cold-boot race between the in-kernel PDR notification for the
  ADSP audio PD and APR (issue #24, item 4).
