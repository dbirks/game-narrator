package doctor

import (
	"context"
	"strings"
	"testing"
)

// The bluetooth.audio row (ADR 0091): the probe is injected (the row's job
// is to render the state in the report's states — a doctor test that hit
// the system bus would not pass on every host), and one probe is the real
// ProbeFromEnv pointed at the labelled-synthetic seam (no radio touched).

func bluetoothProbe(st BluetoothState) BluetoothProbe {
	return func(context.Context) BluetoothState { return st }
}

// btRowCase is one rendered state of the bluetooth.audio row.
type btRowCase struct {
	name       string
	probe      BluetoothProbe
	wantStatus string
	wantIn     string
}

func TestBluetoothRowRendersEveryState(t *testing.T) {
	cases := []btRowCase{
		{
			name:       "not wired",
			probe:      nil,
			wantStatus: StatusNotConfigured,
			wantIn:     "not wired",
		},
		{
			name:       "labelled synthetic fake",
			probe:      bluetoothProbe(BluetoothState{Synthetic: true, Available: true, Powered: true}),
			wantStatus: StatusOK,
			wantIn:     "LABELLED-SYNTHETIC",
		},
		{
			name: "daemon unreachable",
			probe: bluetoothProbe(BluetoothState{
				BusNote: "opening the system bus: no system bus on this host",
			}),
			wantStatus: StatusDegraded,
			wantIn:     "unreachable",
		},
		{
			name:       "adapter powered off",
			probe:      bluetoothProbe(BluetoothState{Available: true, AdapterNote: "the adapter is powered off"}),
			wantStatus: StatusDegraded,
			wantIn:     "powered off",
		},
		{
			name: "linger missing",
			probe: bluetoothProbe(BluetoothState{
				Available: true, Powered: true, Linger: "no", Override: "yes",
			}),
			wantStatus: StatusDegraded,
			wantIn:     "linger",
		},
		{
			name: "override missing",
			probe: bluetoothProbe(BluetoothState{
				Available: true, Powered: true, Linger: "yes", Override: "no",
			}),
			wantStatus: StatusDegraded,
			wantIn:     "headless override",
		},
		{
			name:       "all prerequisites in place",
			probe:      bluetoothProbe(BluetoothState{Available: true, Powered: true, Linger: "yes", Override: "yes"}),
			wantStatus: StatusOK,
			wantIn:     "in place",
		},
		{
			name: "both prerequisites missing carry both remedies",
			probe: bluetoothProbe(BluetoothState{
				Available: true, Powered: true, Linger: "no", Override: "no",
			}),
			wantStatus: StatusDegraded,
			wantIn:     "enable-linger",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.opts.Bluetooth = tc.probe
			c := h.check(h.run(), "bluetooth.audio")
			mustStatus(t, c, tc.wantStatus)
			joined := c.Summary + " " + c.Detail + " " + c.Remedy
			if !strings.Contains(joined, tc.wantIn) {
				t.Errorf("summary/detail/remedy = %q / %q / %q, want it to contain %q", c.Summary, c.Detail, c.Remedy, tc.wantIn)
			}
		})
	}
}

// TestBluetoothProbeSeamIsLabelledSynthetic runs the real ProbeFromEnv with
// the seam variable set: it must report the fake (available + powered,
// synthetic labelled) without touching any bus — and must NOT report
// synthetic when the variable is unset even if the bus is absent (the row
// then degrades with the bus note, it does not pretend).
func TestBluetoothProbeSeamIsLabelledSynthetic(t *testing.T) {
	withEnv := func(k, v string) func(string) (string, bool) {
		return func(name string) (string, bool) {
			if name == k {
				return v, true
			}
			return "", false
		}
	}
	st := BluetoothProbeFromEnv(withEnv("GAME_NARRATOR_BT_FAKE", "1"))(context.Background())
	if !st.Synthetic || !st.Available || !st.Powered {
		t.Fatalf("the seam must answer available+powered+synthetic: %+v", st)
	}
	// Unset: the probe reaches for the real bus. On a host without a
	// system bus/BlueZ it answers BusNote (the row degrades); on a host
	// with one it answers the real adapter state. Both are honest; the
	// assertion is only that it is not the fake.
	st = BluetoothProbeFromEnv(withEnv("GAME_NARRATOR_BT_FAKE", "absent"))(context.Background())
	if st.Synthetic {
		t.Fatalf("the real probe must never report synthetic: %+v", st)
	}
}
