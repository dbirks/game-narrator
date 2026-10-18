package hwsmoke

// Host-agnostic unit tests for the two conversions the resource-budget smoke
// gets wrong when it does not know its host: the thermal-zone unit (finding
// 4 of the first UNO Q run: raw 3590 logged as "3590 mC" = 3.6 °C) and the
// serve-binary resolution for the 60 s session (GAME_NARRATOR_BINARY > PATH >
// dev-host build).

import (
	"os"
	"testing"
)

func TestThermalCelsiusUnitConversion(t *testing.T) {
	cases := []struct {
		name        string
		raw         int
		wantC       float64
		wantRawAsIs bool
		wantOK      bool
	}{
		{"the UNO Q idle reading (0.01 °C unit)", 3590, 35.9, false, true},
		{"UNO Q mapss zone (millidegrees, forum evidence)", 33700, 33.7, false, true},
		{"already a Celsius value", 42, 42, true, true},
		{"zero raw reads as zero", 0, 0, true, true},
		{"millidegrees at the floor", 5000, 5.0, false, true},
		{"just under the millidegrees floor reads as 0.01 °C", 4999, 49.99, false, true},
		{"warm millidegrees", 71250, 71.25, false, true},
		{"millidegrees at the plausibility ceiling", 120000, 120.0, false, true},
		{"millidegrees wins the tie at 12000 (12 °C)", 12000, 12.0, false, true},
		{"impossibly hot under every unit", 125000, 0, false, false},
		{"garbage reading", 999999, 0, false, false},
		{"negative is out of band", -12, 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := thermalCelsius(tc.raw)
			if got.ok != tc.wantOK {
				t.Fatalf("raw %d: ok = %v, want %v (got %+v)", tc.raw, got.ok, tc.wantOK, got)
			}
			if !got.ok {
				return
			}
			if got.celsius != tc.wantC {
				t.Fatalf("raw %d: celsius = %v, want %v", tc.raw, got.celsius, tc.wantC)
			}
			if got.rawAsIs != tc.wantRawAsIs {
				t.Fatalf("raw %d: rawAsIs = %v, want %v", tc.raw, got.rawAsIs, tc.wantRawAsIs)
			}
		})
	}
}

// TestServeBinaryPrefersTheInstalledBinary proves the appliance path: when
// game-narrator is on PATH (the .deb install) the session budgets that
// binary and does not touch the module root at all — the failure the first
// device run hit ("cannot resolve the module root: stat /go.mod").
func TestServeBinaryPrefersTheInstalledBinary(t *testing.T) {
	t.Setenv(EnvBinary, "")
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+fakeBinDir(t, "game-narrator"))
	bin, replayRoot, missing := serveBinary(t)
	if missing != "" {
		t.Fatalf("serveBinary on an 'installed' host: %s", missing)
	}
	if base := bin[len(bin)-len("game-narrator"):]; base != "game-narrator" {
		t.Fatalf("bin = %q, want the PATH-resolved game-narrator", bin)
	}
	if replayRoot != "" {
		t.Fatalf("replayRoot = %q, want none for the installed binary", replayRoot)
	}
}

// TestServeBinaryExplicitEnvPinsTheNamedBuild proves GAME_NARRATOR_BINARY
// wins over PATH, and that a name that does not exist is a NOT RUN reason,
// not a crash.
func TestServeBinaryExplicitEnvPinsTheNamedBuild(t *testing.T) {
	dir := t.TempDir()
	pinned := dir + "/pinned-game-narrator"
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBinary, pinned)
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+fakeBinDir(t, "game-narrator"))
	bin, _, missing := serveBinary(t)
	if missing != "" || bin != pinned {
		t.Fatalf("bin = %q (missing %q), want the pinned %s", bin, missing, pinned)
	}

	t.Setenv(EnvBinary, dir+"/does-not-exist")
	_, _, missing = serveBinary(t)
	if missing == "" {
		t.Fatal("a pinned path that does not exist must explain the NOT RUN")
	}
}

// fakeBinDir fabricates a PATH entry with a single executable named name.
func fakeBinDir(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/"+name, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
