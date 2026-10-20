package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/speech/filesink"
	"github.com/dbirks/game-narrator/internal/speech/pipewire"
	"github.com/dbirks/game-narrator/internal/store"
)

// The audio product surface (ADR-0074): the selected output device, the tone
// test, the master volume and the per-voice gain. The selection logic lives in
// internal/audio so the process, the Settings page and the doctor read one
// spelling of the same rule (env override > persisted store value > default).

// audioListDevices is the enumeration the audio routes use. It is a field so
// a test can exercise the routes with a labelled synthetic list without
// touching the environment; the default is the real enumerator.
var audioListDevices = audio.ListDevices

// audioLookupEnv is the environment the routes resolve against. A field so a
// test can assert the precedence with a fixture environment (the same
// indirection internal/audio.Resolve takes); the default is the process env.
var audioLookupEnv = os.LookupEnv

// deviceView is one enumerated output device (docs/spec/09 "Audio output").
type deviceView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	SampleRate      string `json:"sample_rate,omitempty"`
	Channels        int    `json:"channels,omitempty"`
	Selected        bool   `json:"selected"`
	PlatformDefault bool   `json:"platform_default"`
}

// selectionView is the effective audio output selection with its source for
// every part (ADR-0074): the page shows what will actually happen and where
// each part came from (env override, the stored selection, the default).
type selectionView = audio.Selection

// audioDevicesView is GET /api/v1/audio/devices.
type audioDevicesView struct {
	Backend   string        `json:"backend"`
	Synthetic bool          `json:"synthetic"`
	Devices   []deviceView  `json:"devices"`
	Selected  selectionView `json:"selected"`
	Note      string        `json:"note,omitempty"`
}

// testToneRequest is POST /api/v1/audio/test-tone. Consent is the gate: the
// request cannot play anything until the page's explicit "Play test tone"
// action says so (docs/spec/13 makes playback a consented action; a stray
// beep from an API call is a defect, not a feature). Device may name a
// specific enumerated id to probe; absent, the selected device plays.
type testToneRequest struct {
	Consent bool   `json:"consent"`
	Device  string `json:"device,omitempty"`
}

// testToneResponse reports what the tone endpoint did.
type testToneResponse struct {
	// Tone is the deterministic tone's constants (the one shared spelling,
	// ADR-0074): the live check asserts the device delivered these.
	Tone struct {
		FrequencyHz  float64 `json:"frequency_hz"`
		DurationSecs float64 `json:"duration_s"`
		SampleRate   int     `json:"sample_rate"`
		Level        float64 `json:"level"`
	} `json:"tone"`
	Sink         string  `json:"sink"`
	SinkSource   string  `json:"sink_source"`
	Device       string  `json:"device"`
	MasterVolume float64 `json:"master_volume"`
	Played       bool    `json:"played"`
	DurationMS   int64   `json:"duration_ms"`
	// EvidenceFile is the tone's recorded WAV (basename) when the sink is the
	// filesink: on a machine with no audible output the file is the proof.
	EvidenceFile string `json:"evidence_file,omitempty"`
	Note         string `json:"note,omitempty"`
}

// audioSettingsRequest is the partial body of PUT /api/v1/settings/audio:
// every field optional, a nil field keeps the stored value (the same
// partial-body rule the session settings route follows).
type audioSettingsRequest struct {
	Device       *string  `json:"device,omitempty"`
	SinkKind     *string  `json:"sink_kind,omitempty"`
	MasterVolume *float64 `json:"master_volume,omitempty"`
}

// audioSettingsResponse is PUT /api/v1/settings/audio's answer: the row now
// stored and the effective selection (env overrides still win, and the
// sources say so).
type audioSettingsResponse struct {
	Saved     store.AudioSettings `json:"saved"`
	Effective selectionView       `json:"effective"`
}

// audioStoredSelection loads the stored audio row (nil store → none).
func (s *Server) audioStoredSelection() *audio.Stored {
	if s.store == nil {
		return nil
	}
	as, ok, err := s.store.GetAudioSettings(context.Background())
	if err != nil || !ok {
		return nil
	}
	return &audio.Stored{
		Device:       as.Device,
		SinkKind:     as.SinkKind,
		MasterVolume: as.MasterVolume,
		Set:          true,
	}
}

// handleAudioDevicesGet answers GET /api/v1/audio/devices: the enumerated
// output devices with kinds and stable identities, the one that is selected,
// and the effective selection with per-part sources.
func (s *Server) handleAudioDevicesGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := audioListDevices(ctx, audioLookupEnv)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "audio_devices_failed",
			"Internal Error", "enumerating the audio devices failed: "+err.Error())
		return
	}
	sel, err := audio.Resolve(audioLookupEnv, s.audioStoredSelection())
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "audio_selection_failed",
			"Internal Error", "resolving the audio output failed: "+err.Error())
		return
	}
	devs := make([]deviceView, 0, len(list.Devices))
	for _, d := range list.Devices {
		devs = append(devs, deviceView{
			ID:         d.ID,
			Name:       d.Name,
			Kind:       string(d.Kind),
			SampleRate: d.SampleRate,
			Channels:   d.Channels,
			// Selected follows the selection rule (ADR-0074): the enumerated
			// id matching the selection, or the platform default when the
			// selection is "default".
			Selected:        d.ID == sel.Device || (sel.Device == audio.DefaultDevice && d.PlatformDefault),
			PlatformDefault: d.PlatformDefault,
		})
	}
	writeJSON(w, http.StatusOK, audioDevicesView{
		Backend:   list.Backend,
		Synthetic: list.Synthetic,
		Devices:   devs,
		Selected:  sel,
		Note:      list.Note,
	})
}

// handleAudioSettingsPut answers PUT /api/v1/settings/audio (the Settings
// page's audio section): persist the stored selection, validated against the
// live enumeration, and report the effective selection.
func (s *Server) handleAudioSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "store-not-wired",
			"SUBSYSTEM_UNAVAILABLE", "Service Unavailable",
			"the settings store is not wired into this build")
		return
	}
	var req audioSettingsRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid_audio_settings",
			"Bad Request", "the audio settings body is not valid JSON")
		return
	}

	list, lerr := audioListDevices(r.Context(), audioLookupEnv)
	var listOK bool
	if lerr == nil {
		listOK = true
	}

	stored := s.audioStoredSelection()
	if stored == nil {
		// No row yet: seed the master volume at unity so a device-only save
		// never mutes the output (a stored 0 means "the user chose silence",
		// which only exists once the user has chosen it).
		stored = &audio.Stored{MasterVolume: audio.DefaultMasterVolume}
	}
	if req.Device != nil {
		stored.Device = *req.Device
	}
	if req.SinkKind != nil {
		stored.SinkKind = *req.SinkKind
	}
	if req.MasterVolume != nil {
		stored.MasterVolume = *req.MasterVolume
	}
	stored.Set = true

	// Validate the parts that are actually set (ADR-0074's partial body): a
	// blank part means "the user never chose that part" and resolves to the
	// built-in default — validating it would reject every device-only save,
	// which is the exact partial body the Settings page sends first.
	if listOK && stored.Device != "" {
		if err := audio.ValidateDevice(stored.Device, list); err != nil {
			s.writeProblem(w, r, http.StatusBadRequest, "unknown_audio_device",
				"Bad Request", "the selected audio device is not an enumerated output: "+err.Error())
			return
		}
	}
	if stored.SinkKind != "" {
		if err := audio.ValidateSinkKind(stored.SinkKind); err != nil {
			s.writeProblem(w, r, http.StatusBadRequest, "invalid_audio_sink",
				"Bad Request", "the audio sink kind is not one of dummy, filesink, pipewire: "+err.Error())
			return
		}
	}
	if err := audio.ValidateMasterVolume(stored.MasterVolume); err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid_master_volume",
			"Bad Request", "the master volume must be between 0 and 1: "+err.Error())
		return
	}
	row := store.AudioSettings{
		Device:       stored.Device,
		SinkKind:     stored.SinkKind,
		MasterVolume: stored.MasterVolume,
	}
	if err := s.store.SetAudioSettings(r.Context(), row); err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "audio_settings_save_failed",
			"Internal Error", "saving the audio settings failed: "+err.Error())
		return
	}

	// Apply the master volume to the live sessions immediately (ADR-0074:
	// the Settings page's master volume takes effect from the next
	// playback); the sink kind and device apply from the next session (a
	// running session's device is its wiring, not a live-repointable knob).
	if s.sessions != nil && req.MasterVolume != nil {
		if err := s.sessions.SetMasterVolume(*req.MasterVolume); err != nil {
			// The row is saved; the live apply is best-effort and the log
			// says so rather than failing the save.
			s.log.Warn("applying the master volume to the live sessions", "error", err)
		}
	}

	sel, err := audio.Resolve(audioLookupEnv, stored)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "audio_selection_failed",
			"Internal Error", "resolving the audio output failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, audioSettingsResponse{
		Saved:     row,
		Effective: sel,
	})
}

// tonePlayTimeout bounds the tone: the tone is 1.0 s, a device that cannot
// deliver it in 10 s is a defect to report, not a wait.
const tonePlayTimeout = 10 * time.Second

// handleAudioTestTone answers POST /api/v1/audio/test-tone (ADR-0074): the
// deterministic 440 Hz / 1.0 s / 48 kHz tone through the session's sink
// machinery — the existing sinks, the effective master volume — behind an
// explicit consent flag.
func (s *Server) handleAudioTestTone(w http.ResponseWriter, r *http.Request) {
	var req testToneRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid_test_tone_request",
			"Bad Request", "the test-tone body is not valid JSON")
		return
	}
	if !req.Consent {
		// The gate: no consent, no tone. The problem says so explicitly so
		// the page can point at the consent control.
		s.writeCodeProblem(w, r, http.StatusBadRequest, "consent-required",
			"CONSENT_REQUIRED", "Bad Request",
			"the test tone plays only on the user's explicit consent: set consent to true")
		return
	}

	sel, err := audio.Resolve(audioLookupEnv, s.audioStoredSelection())
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "audio_selection_failed",
			"Internal Error", "resolving the audio output failed: "+err.Error())
		return
	}

	list, lerr := audioListDevices(r.Context(), audioLookupEnv)
	device := sel.Device
	if req.Device != "" {
		device = req.Device
	}
	if lerr == nil {
		if err := audio.ValidateDevice(device, list); err != nil {
			s.writeProblem(w, r, http.StatusBadRequest, "unknown_audio_device",
				"Bad Request", "the requested audio device is not an enumerated output: "+err.Error())
			return
		}
	}

	tone := audio.ToneWAV()
	// The effective master volume shapes the tone exactly as it shapes the
	// session's speech: one level, applied to the samples. A master of 0 is
	// the user's silence calibration and plays as silence here too — the
	// condition is therefore "below unity", not "strictly between".
	if mv := sel.MasterVolume; mv < 1 {
		tone = audio.ScaleWAV(tone, mv)
	}

	ctx, cancel := context.WithTimeout(r.Context(), tonePlayTimeout)
	defer cancel()

	sink, kind, err := s.toneSink(ctx, sel.SinkKind, device)
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "tone_sink_unavailable",
			"SINK_UNAVAILABLE", "Service Unavailable", err.Error())
		return
	}
	defer func() { _ = sink.Close() }()

	handle, err := sink.Play(ctx, speech.PlayRequest{
		UtteranceID: "tone",
		Generation:  1,
		WAV:         tone,
		Gain:        1.0, // the master volume already shaped the samples
	})
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "tone_play_failed",
			"Internal Error", "starting the test tone failed: "+err.Error())
		return
	}
	started := time.Now()
	var res speech.PlayResult
	select {
	case res = <-handle.Done():
	case <-ctx.Done():
		handle.Stop(0)
		s.writeCodeProblem(w, r, http.StatusGatewayTimeout, "tone_timeout",
			"TONE_TIMEOUT", "Gateway Timeout",
			"the test tone did not finish within the timeout: the output device is not delivering audio")
		return
	}
	played := res.Err == nil
	resp := testToneResponse{
		Sink:         kind,
		SinkSource:   sel.SinkKindSource,
		Device:       device,
		MasterVolume: sel.MasterVolume,
		Played:       played,
		DurationMS:   time.Since(started).Milliseconds(),
	}
	resp.Tone.FrequencyHz = audio.ToneFreqHz
	resp.Tone.DurationSecs = audio.ToneSeconds
	resp.Tone.SampleRate = audio.ToneSampleRate
	resp.Tone.Level = audio.ToneLevel
	switch p := handle.(type) {
	case *filesink.Play:
		resp.EvidenceFile = filepath.Base(p.Path())
		resp.Note = "the tone was recorded (filesink): on a machine without audible output the evidence file is the proof"
	}
	if kind == SinkDummy {
		resp.Note = "the dummy sink paces and records nothing: the timing ran, no audio was delivered"
	}
	if !played {
		resp.Note = "the tone playback did not complete: " + res.Err.Error()
		// A tone the device refused is not a 200: the operator must see the
		// failure, and the page's "the tone did not play" state reads it.
		writeJSON(w, http.StatusBadGateway, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// toneSink builds the sink the tone plays through, following the selection's
// sink kind (ADR-0074: the tone and the session use the same sinks, so a tone
// that works is the evidence the session's path works). The filesink writes
// under the data directory's audio-test-tone directory (or a temp directory
// when no store is wired), so a tone that plays leaves evidence.
func (s *Server) toneSink(ctx context.Context, kind, device string) (speech.AudioSink, string, error) {
	switch kind {
	case SinkDummy:
		return dummy.New(0, 0), kind, nil
	case SinkFile:
		// The store's Path() is the database FILE under the data directory, so
		// the evidence directory is its parent's: the data directory's
		// audio-test-tone directory, or a temp directory when no store is
		// wired.
		dir := filepath.Join(os.TempDir(), "game-narrator-tone")
		if s.store != nil {
			dir = filepath.Join(filepath.Dir(s.store.Path()), "audio-test-tone")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, "", fmt.Errorf("creating the tone evidence directory: %w", err)
		}
		sk, err := filesink.New(dir)
		if err != nil {
			return nil, "", fmt.Errorf("filesink: %w", err)
		}
		return sk, kind, nil
	case SinkPipeWire:
		if device == "" {
			device = audio.DefaultDevice
		}
		sk, err := pipewire.New(device)
		if err != nil {
			return nil, "", fmt.Errorf("pipewire: %w", err)
		}
		return sk, kind, nil
	default:
		return nil, "", fmt.Errorf("unknown sink kind %q", kind)
	}
}

// The runtime's sink-kind constants (the session's wiring uses the same
// vocabulary; the API package references them rather than re-spelling them).
const (
	SinkDummy    = runtime.SinkDummy
	SinkFile     = runtime.SinkFile
	SinkPipeWire = runtime.SinkPipeWire
)
