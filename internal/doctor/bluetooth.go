package doctor

// The bluetooth.audio row (ADR 0091, issues #2/#3): the appliance's honest
// answer about the Bluetooth output path — the radio itself plus the two
// session prerequisites without which the A2DP sink node cannot exist (the
// lingering user session for the user PipeWire instance, and the verified
// WirePlumber headless override).
//
// The row never FAILs: a radio-less host (a dev laptop, a container) is a
// DEGRADED appliance, not a broken one — and the DEGRADED states carry the
// concrete next step in Remedy, the spec-13 contract ("the service that has
// not been probed is never reported as working"):
//
//	OK          the adapter answers and is powered, linger is on, the
//	            headless override is present (or the run is the labelled
//	            synthetic fake, which says so in the summary);
//	DEGRADED    the daemon is unreachable (no system bus / BlueZ), the
//	            adapter is off, the linger is missing, or the override file
//	            is missing — each with its remedy (the package postinst
//	            applies all three on install).
//
// The fake (GAME_NARRATOR_BT_FAKE) is the ADR-0091 labelled-synthetic seam:
// the row reports it as synthetic, never as hardware.

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/bt"
)

// BluetoothUser is the audio-owning service user the row probes (the
// sysusers line): its lingering user session is what keeps the user
// PipeWire instance — and with it the bluez_output.<MAC>.1 sink node —
// alive on a headless appliance.
const BluetoothUser = "game-narrator"

// BluetoothOverrideRelPath is the headless override's path under the user's
// home (the byte-exact file ADR 0084 verified on the real board; the
// package postinst installs it).
const BluetoothOverrideRelPath = ".config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf"

// BluetoothState is the answer the bluetooth.audio row renders.
type BluetoothState struct {
	// Synthetic is true when the controller is the labelled fake
	// (GAME_NARRATOR_BT_FAKE): the row must say so, never present the
	// fake as hardware.
	Synthetic bool
	// Available is true when the controller reached the BlueZ daemon.
	Available bool
	// Powered is the adapter state (meaningful when Available).
	Powered bool
	// AdapterNote is the daemon's own note (e.g. "the adapter is powered
	// off"); empty when there is nothing to add.
	AdapterNote string
	// BusNote says why the controller could not reach the daemon (the
	// exact error); empty when it reached it.
	BusNote string
	// Linger is "yes", "no", or "" when it could not be checked (no
	// loginctl, no such user on this host).
	Linger string
	// Override is "yes", "no", or "" when it could not be checked (the
	// user's home could not be resolved).
	Override string
}

// BluetoothProbe resolves the state for the row. It gets the row's own
// context deadline (the add() engine abandons a probe that ignores its
// budget).
type BluetoothProbe func(ctx context.Context) BluetoothState

// probeBudget bounds the bus call inside the probe (the row's deadline is
// the outer bound; this one keeps the probe from leaning on it for the
// daemon call, whose own bound would otherwise be the row's).
const probeBudget = 5 * time.Second

// BluetoothProbeFromEnv builds the production probe: the labelled-synthetic
// seam first (GAME_NARRATOR_BT_FAKE, the ADR-0091 pattern), then the real
// D-Bus controller; the session prerequisites are read with the same tools
// the package postinst uses (loginctl, the file on disk).
func BluetoothProbeFromEnv(lookupEnv func(string) (string, bool)) BluetoothProbe {
	return func(ctx context.Context) BluetoothState {
		var st BluetoothState
		if v, ok := lookupEnv(EnvBluetoothFake); ok && (v == "1" || strings.EqualFold(v, "true")) {
			st.Synthetic = true
			st.Available = true
			st.Powered = true
		} else {
			callCtx, cancel := context.WithTimeout(ctx, probeBudget)
			c, err := bt.New(callCtx)
			cancel()
			if err != nil {
				st.BusNote = err.Error()
			} else {
				defer c.Close()
				callCtx, cancel = context.WithTimeout(ctx, probeBudget)
				s, err := c.State(callCtx)
				cancel()
				if err != nil {
					st.BusNote = err.Error()
				} else {
					st.Available = true
					st.Powered = s.Powered
					st.AdapterNote = s.Note
				}
			}
		}
		st.Linger = probeLinger(ctx)
		st.Override = probeOverride(ctx)
		return st
	}
}

// EnvBluetoothFake is the labelled-synthetic seam (ADR-0091): the doctor
// row and the serve process honour the same variable, so `doctor` on the
// walk machine and the UI it documents agree on the seam.
const EnvBluetoothFake = "GAME_NARRATOR_BT_FAKE"

func probeLinger(ctx context.Context) string {
	if _, err := exec.LookPath("loginctl"); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	res := exec.CommandContext(cctx, "loginctl", "show-user", BluetoothUser, "-p", "Linger")
	out, err := res.CombinedOutput()
	if err != nil {
		// No such user, no logind, no policy grant: the answer is
		// "could not check", not "off".
		return ""
	}
	line := strings.TrimSpace(string(out))
	switch {
	case strings.HasSuffix(line, "=yes"):
		return "yes"
	case strings.HasSuffix(line, "=no"):
		return "no"
	default:
		return ""
	}
}

// probeOverride checks the headless override file under the user's home.
// The home comes from getent (the sysusers line sets it to
// /var/lib/game-narrator on the appliance); when the user cannot be
// resolved the answer is "" (could not check) and the row stays honest
// about which half of the state it could not read.
func probeOverride(ctx context.Context) string {
	home := probeHome(ctx)
	if home == "" {
		return ""
	}
	_, err := os.Stat(home + "/" + BluetoothOverrideRelPath)
	if err == nil {
		return "yes"
	}
	if os.IsNotExist(err) {
		return "no"
	}
	return ""
}

func probeHome(ctx context.Context) string {
	if _, err := exec.LookPath("getent"); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	res := exec.CommandContext(cctx, "getent", "passwd", BluetoothUser)
	out, err := res.Output()
	if err != nil {
		return ""
	}
	fields := strings.Split(string(out), ":")
	if len(fields) < 6 {
		return ""
	}
	return strings.TrimSpace(fields[5])
}

// checkBluetooth renders the row (see the file header for the state table).
func (r *runner) checkBluetooth(ctx context.Context) Check {
	if r.opts.Bluetooth == nil {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "the Bluetooth probe is not wired into this run",
			Remedy:  "wire the Bluetooth probe (the CLI builds it from the environment)",
		}
	}
	st := r.opts.Bluetooth(ctx)
	if st.Synthetic {
		return Check{
			Status:  StatusOK,
			Summary: "the Bluetooth surface is the LABELLED-SYNTHETIC fake (GAME_NARRATOR_BT_FAKE); no radio was touched",
		}
	}
	if !st.Available {
		return Check{
			Status:  StatusDegraded,
			Summary: "the Bluetooth daemon is unreachable (no system bus or BlueZ on this host)",
			Detail:  st.BusNote,
			Remedy:  "run under systemd with BlueZ (the appliance), or GAME_NARRATOR_BT_FAKE=1 for the UI walk",
		}
	}
	if !st.Powered {
		return Check{
			Status:  StatusDegraded,
			Summary: "the Bluetooth adapter is powered off",
			Detail:  st.AdapterNote,
			Remedy:  "power the adapter on (the settings page does this through the same surface)",
		}
	}
	var (
		degraded bool
		notes    []string
		remedy   []string
	)
	if st.Linger == "no" {
		degraded = true
		notes = append(notes, "linger is off for "+BluetoothUser+" — the user PipeWire instance (and with it the bluez_output.<MAC>.1 sink node) stops when the last login goes away")
		remedy = append(remedy, "loginctl enable-linger "+BluetoothUser+" (the package postinst applies it)")
	}
	if st.Override == "no" {
		degraded = true
		notes = append(notes, "the WirePlumber headless override is missing — on a headless appliance the device pairs and connects but no A2DP sink node appears")
		remedy = append(remedy, "install "+BluetoothOverrideRelPath+" under the user's home (the package postinst writes it; the skill's bluetooth-audio.sh --apply does the same)")
	}
	if degraded {
		return Check{
			Status:  StatusDegraded,
			Summary: "the adapter is up but the Bluetooth session prerequisites are incomplete",
			Detail:  strings.Join(notes, "; "),
			Remedy:  strings.Join(remedy, "; "),
		}
	}
	return Check{
		Status:  StatusOK,
		Summary: "the Bluetooth adapter is up; the session prerequisites are in place (linger, headless override)",
	}
}
