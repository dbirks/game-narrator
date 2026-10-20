package api

// The output-audio product surface tests (ADR-0074, gap audit G5): the device
// list with the effective selection, the consent-gated test tone through the
// real sinks, and the admin-only persisted selection. The enumeration runs
// through the audioListDevices seam so the suite is hermetic (the real
// enumerator would run pactl/aplay against the host's live sinks); the
// precedence env > store > default is asserted against the real Resolve.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/auth"
)

// audioTestList is the fixture enumeration: a USB DAC, the platform-default
// null sink and the capture dongle's UAC interface — the three identities the
// selection rule must tell apart.
var audioTestList = audio.List{
	Backend:   "synthetic",
	Synthetic: true,
	Devices: []audio.Device{
		{ID: "alsa_output.usb-JBL_Flip_6_0011223344", Name: "JBL Flip 6", Kind: audio.KindUSB},
		{ID: "auto_null", Name: "Null Output", Kind: audio.KindNull, PlatformDefault: true},
		{ID: "alsa_output.usb-General_UAC_Device_001", Name: "Generic UAC Device", Kind: audio.KindUSB},
	},
}

// withAudioList points the audio routes' enumeration at the fixture list for
// the test's duration.
func withAudioList(t *testing.T) {
	t.Helper()
	prev := audioListDevices
	t.Cleanup(func() { audioListDevices = prev })
	audioListDevices = func(ctx context.Context, lookupEnv func(string) (string, bool)) (audio.List, error) {
		return audioTestList, nil
	}
}

// TestAudioDevicesRouteGrants pins the scope table: the device read is the
// operator tier (it names physical output devices), the viewer is refused.
func TestAudioDevicesRouteGrants(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))
	rec := doReq(t, h, a.as(auth.RoleViewer, getReq("/api/v1/audio/devices")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/devices")))
	if rec.Code != http.StatusOK {
		t.Fatalf("operator = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAudioDevicesListShowsTheSelection asserts the effective selection with
// its per-part sources and the selected row's flag: the env override wins and
// is named as such, and the platform default is NOT flagged selected when an
// explicit device is chosen.
func TestAudioDevicesListShowsTheSelection(t *testing.T) {
	withAudioList(t)
	t.Setenv(audio.EnvDevice, "alsa_output.usb-JBL_Flip_6_0011223344")
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))
	rec := doReq(t, h, a.as(auth.RoleOperator, getReq("/api/v1/audio/devices")))
	if rec.Code != http.StatusOK {
		t.Fatalf("devices = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Backend   string `json:"backend"`
		Synthetic bool   `json:"synthetic"`
		Devices   []struct {
			ID       string `json:"id"`
			Selected bool   `json:"selected"`
		} `json:"devices"`
		Selected struct {
			Device       string `json:"device"`
			DeviceSource string `json:"device_source"`
			SinkKind     string `json:"sink_kind"`
			SinkSource   string `json:"sink_kind_source"`
		} `json:"selected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("devices body: %v (%s)", err, rec.Body.String())
	}
	if !body.Synthetic || body.Backend != "synthetic" {
		t.Fatalf("synthetic=%v backend=%q: the fixture list must be labelled synthetic", body.Synthetic, body.Backend)
	}
	if body.Selected.Device != "alsa_output.usb-JBL_Flip_6_0011223344" || body.Selected.DeviceSource != audio.SourceEnv {
		t.Fatalf("selection = %q (%s): the env override must win and be named", body.Selected.Device, body.Selected.DeviceSource)
	}
	if body.Selected.SinkKind != audio.SinkFile || body.Selected.SinkSource != audio.SourceDefault {
		t.Fatalf("sink = %q (%s): no override, so the built-in default with its source", body.Selected.SinkKind, body.Selected.SinkSource)
	}
	for _, d := range body.Devices {
		want := d.ID == "alsa_output.usb-JBL_Flip_6_0011223344"
		if d.Selected != want {
			t.Errorf("device %q selected=%v, want %v", d.ID, d.Selected, want)
		}
	}
}

// TestAudioSettingsPutPersistsAndReportsEffective is the admin write: the row
// lands in the store, the effective selection reports the store as the
// source, and a later read (no env) resolves from it.
func TestAudioSettingsPutPersistsAndReportsEffective(t *testing.T) {
	withAudioList(t)
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))

	body := `{"device":"alsa_output.usb-JBL_Flip_6_0011223344","sink_kind":"pipewire","master_volume":0.5}`
	rec := doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPut, "/api/v1/settings/audio", body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("put = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Saved struct {
			Device       string  `json:"device"`
			SinkKind     string  `json:"sink_kind"`
			MasterVolume float64 `json:"master_volume"`
		} `json:"saved"`
		Effective struct {
			Device       string  `json:"device"`
			DeviceSource string  `json:"device_source"`
			SinkSource   string  `json:"sink_kind_source"`
			MasterVolume float64 `json:"master_volume"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("put body: %v (%s)", err, rec.Body.String())
	}
	if resp.Saved.Device != "alsa_output.usb-JBL_Flip_6_0011223344" || resp.Saved.SinkKind != "pipewire" || resp.Saved.MasterVolume != 0.5 {
		t.Fatalf("saved row = %+v", resp.Saved)
	}
	if resp.Effective.DeviceSource != audio.SourceStore || resp.Effective.SinkSource != audio.SourceStore {
		t.Fatalf("effective sources = %q/%q, want the store named", resp.Effective.DeviceSource, resp.Effective.SinkSource)
	}

	// The row is in the store, and a second process's read resolves from it.
	stored, ok, err := a.st.GetAudioSettings(context.Background())
	if err != nil || !ok {
		t.Fatalf("store row: ok=%v err=%v", ok, err)
	}
	if stored.Device != "alsa_output.usb-JBL_Flip_6_0011223344" || stored.MasterVolume != 0.5 {
		t.Fatalf("stored row = %+v", stored)
	}
}

// TestAudioSettingsPutPartialBodyKeepsTheUnchosenParts is the Settings page's
// first write: a device-only body with no row yet. The omitted parts must
// stay "never chosen" (empty in the row, the default source effective) —
// validating the blanks would 400 the very save the page sends first.
func TestAudioSettingsPutPartialBodyKeepsTheUnchosenParts(t *testing.T) {
	withAudioList(t)
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))

	rec := doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPut, "/api/v1/settings/audio", `{"device":"auto_null"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("device-only put = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Saved struct {
			Device       string  `json:"device"`
			SinkKind     string  `json:"sink_kind"`
			MasterVolume float64 `json:"master_volume"`
		} `json:"saved"`
		Effective struct {
			DeviceSource   string `json:"device_source"`
			SinkKind       string `json:"sink_kind"`
			SinkKindSource string `json:"sink_kind_source"`
		} `json:"effective"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("put body: %v (%s)", err, rec.Body.String())
	}
	if resp.Saved.Device != "auto_null" || resp.Saved.SinkKind != "" {
		t.Fatalf("saved row = %+v, want the device chosen and the sink never-chosen", resp.Saved)
	}
	if resp.Effective.DeviceSource != audio.SourceStore {
		t.Fatalf("effective device source = %q, want the store named", resp.Effective.DeviceSource)
	}
	if resp.Effective.SinkKind != audio.DefaultSinkKind || resp.Effective.SinkKindSource != audio.SourceDefault {
		t.Fatalf("effective sink = %q/%q, want the built-in default named (never chosen)", resp.Effective.SinkKind, resp.Effective.SinkKindSource)
	}
}

// TestAudioSettingsPutRoleAndValidation pins the admin gate and the 400s:
// the operator cannot persist a selection, an unenumerated device is refused
// with its id named, and an out-of-range volume is refused.
func TestAudioSettingsPutRoleAndValidation(t *testing.T) {
	withAudioList(t)
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))

	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPut, "/api/v1/settings/audio", `{"device":"auto_null"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator put = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}

	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPut, "/api/v1/settings/audio", `{"device":"alsa_output.usb-Pulled_DAC_999"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Pulled_DAC_999") {
		t.Fatalf("unenumerated device = %d %s, want 400 with the id named", rec.Code, rec.Body.String())
	}

	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPut, "/api/v1/settings/audio", `{"master_volume":1.5}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("volume 1.5 = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}

	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPut, "/api/v1/settings/audio", `{"sink_kind":"jack"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sink kind jack = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAudioTestToneConsentGate is docs/spec/13's consent rule on the wire:
// without the explicit consent flag the tone does not play and the answer
// says so; the tone route itself is the operator tier.
func TestAudioTestToneConsentGate(t *testing.T) {
	withAudioList(t)
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))

	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost, "/api/v1/audio/test-tone", `{}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "consent") {
		t.Fatalf("no consent = %d %s, want 400 consent-required", rec.Code, rec.Body.String())
	}

	rec = doReq(t, h, a.as(auth.RoleViewer, jsonRequest(http.MethodPost, "/api/v1/audio/test-tone", `{"consent":true}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer tone = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAudioTestTonePlaysThroughTheSinks drives the tone through the real sink
// machinery (the same sinks the session plays): the dummy sink paces, the
// filesink leaves an evidence file the test opens and checks is a non-empty
// WAV, and the response reports the tone's constants and the master volume
// that shaped it.
func TestAudioTestTonePlaysThroughTheSinks(t *testing.T) {
	withAudioList(t)
	t.Setenv(audio.EnvSink, audio.SinkDummy)
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))

	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost, "/api/v1/audio/test-tone", `{"consent":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("tone = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Sink         string  `json:"sink"`
		SinkSource   string  `json:"sink_kind_source"`
		MasterVolume float64 `json:"master_volume"`
		Played       bool    `json:"played"`
		Tone         struct {
			FrequencyHz  float64 `json:"frequency_hz"`
			DurationSecs float64 `json:"duration_s"`
			SampleRate   int     `json:"sample_rate"`
			Level        float64 `json:"level"`
		} `json:"tone"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("tone body: %v (%s)", err, rec.Body.String())
	}
	if !body.Played || body.Sink != audio.SinkDummy {
		t.Fatalf("tone = %+v, want played on the dummy sink", body)
	}
	if body.Tone.FrequencyHz != audio.ToneFreqHz || body.Tone.DurationSecs != audio.ToneSeconds ||
		body.Tone.SampleRate != audio.ToneSampleRate || body.Tone.Level != audio.ToneLevel {
		t.Fatalf("tone constants = %+v, want the shared spellings (%v/%v/%d/%v)", body.Tone,
			audio.ToneFreqHz, audio.ToneSeconds, audio.ToneSampleRate, audio.ToneLevel)
	}
}

// TestAudioTestToneFilesinkLeavesEvidence is the no-audible-output host's
// proof: the filesink records the tone and the response names the file, which
// the test then opens — a 200 with a missing file would be a lie.
func TestAudioTestToneFilesinkLeavesEvidence(t *testing.T) {
	withAudioList(t)
	t.Setenv(audio.EnvSink, audio.SinkFile)
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))

	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost, "/api/v1/audio/test-tone", `{"consent":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("tone = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Played       bool   `json:"played"`
		EvidenceFile string `json:"evidence_file"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("tone body: %v (%s)", err, rec.Body.String())
	}
	if !body.Played || body.EvidenceFile == "" {
		t.Fatalf("tone = %+v, want played with the evidence file named", body)
	}
	p := filepath.Join(filepath.Dir(a.st.Path()), "audio-test-tone", body.EvidenceFile)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("evidence file missing at %s: %v", p, err)
	}
	if len(data) < 44 || string(data[0:4]) != "RIFF" {
		t.Fatalf("evidence file at %s is not a WAV (%d bytes)", p, len(data))
	}
}

// TestAudioTestToneRefusesUnenumeratedDevice: a named device that the
// enumeration does not show is a 400, not a tone into a device that does not
// exist.
func TestAudioTestToneRefusesUnenumeratedDevice(t *testing.T) {
	withAudioList(t)
	t.Setenv(audio.EnvSink, audio.SinkDummy)
	a := newTestAuth(t)
	h := a.handler(WithStore(a.st))

	rec := doReq(t, h, a.as(auth.RoleOperator,
		jsonRequest(http.MethodPost, "/api/v1/audio/test-tone", `{"consent":true,"device":"missing"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "missing") {
		t.Fatalf("tone to missing device = %d %s, want 400 with the id named", rec.Code, rec.Body.String())
	}
}
