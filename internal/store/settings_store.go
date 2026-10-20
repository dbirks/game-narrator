package store

// The appliance's runtime-mutable settings (ADR-0074): one app_settings
// key/value row for the audio output selection and master volume, and the
// per-voice gain on the voice profile row.
//
// The environment stays the override layer (ADR-0039): these rows are the
// DEFAULT the process resolves to when GAME_NARRATOR_AUDIO_SINK /
// GAME_NARRATOR_AUDIO_DEVICE do not name a value. The precedence is
// internal/audio.Resolve's, which the API and the serve wiring both call —
// the store is only the persistence half.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dbirks/game-narrator/internal/audio"
)

// AudioSettingsKey is the app_settings key of the audio row.
const AudioSettingsKey = "audio"

// AudioSettings is the persisted audio output selection (one app_settings
// row). An empty field means "that part was never chosen by the user": the
// resolution then falls through to its default, and the API's per-part
// source field says so.
type AudioSettings struct {
	// Device is the selected output device ID ("default" or the
	// enumerated sink/device name). Empty = never chosen.
	Device string `json:"device"`
	// SinkKind is the selected sink kind (dummy, filesink, pipewire).
	// Empty = never chosen.
	SinkKind string `json:"sink_kind,omitempty"`
	// MasterVolume is the device-level master volume, 0..1. A zero value
	// means "never chosen" (unity is the default), which is why 0 is a
	// legal-but-meaningless stored value: a user who wants silence saves
	// it, and the resolution applies it.
	MasterVolume float64 `json:"master_volume"`
}

// GetAudioSettings reads the audio row. ok is false when no row exists
// (the user never chose a selection).
func (s *Store) GetAudioSettings(ctx context.Context) (AudioSettings, bool, error) {
	const op = "get audio settings"
	var value, at string
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return mapReadError(op, q.queryRow(ctx, op,
			`SELECT value, updated_at FROM app_settings WHERE key = ?`, AudioSettingsKey).
			Scan(&value, &at))
	})
	if err != nil {
		if err := errorIsNotFound(err); err != nil {
			return AudioSettings{}, false, nil
		}
		return AudioSettings{}, false, err
	}
	var a AudioSettings
	if err := json.Unmarshal([]byte(value), &a); err != nil {
		// A row the writer of this table cannot read is a store defect,
		// reported loudly (the resolution then falls back to the
		// environment and the defaults, but the 500 is the operator's
		// business, not the page's silence).
		return AudioSettings{}, false, fmt.Errorf("store: %s: stored value does not decode: %w", op, err)
	}
	return a, true, nil
}

// SetAudioSettings writes the audio row (upsert). The value is validated
// against the same bounds the API enforces, so the store cannot hold a
// selection the resolution would refuse to apply.
func (s *Store) SetAudioSettings(ctx context.Context, a AudioSettings) error {
	const op = "set audio settings"
	if a.Device != "" {
		if err := audio.ValidateDevice(a.Device, audio.List{}); err != nil {
			return fmt.Errorf("store: %s: %w", op, err)
		}
	}
	if a.SinkKind != "" {
		if err := audio.ValidateSinkKind(a.SinkKind); err != nil {
			return fmt.Errorf("store: %s: %w", op, err)
		}
	}
	if err := audio.ValidateMasterVolume(a.MasterVolume); err != nil {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	value, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value,
			 updated_at = excluded.updated_at`,
			AudioSettingsKey, string(value), nowStamp())
		if err != nil {
			return mapWriteError(op, err)
		}
		_ = res
		return nil
	})
}

// errorIsNotFound unwraps the store's ErrNotFound into a sentinel check.
func errorIsNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// UpdateVoiceGain sets the per-voice output gain on one voice revision row
// (ADR-0074, docs/spec/07 "per-voice gain"). The gain is an output property,
// not a design property: it does not move the revision's state, it changes
// no cache key, and it is the one field a voice row may carry that the
// ADR-0005 "frozen reference" invariant does not cover (the reference BYTES
// stay frozen; how loud they play is the user's calibration).
func (s *Store) UpdateVoiceGain(ctx context.Context, voiceProfileID string, gain float64) error {
	const op = "update voice gain"
	if err := audio.ValidateGain(gain); err != nil {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			`UPDATE voice_profiles SET gain = ?, updated_at = ? WHERE id = ?`,
			gain, nowStamp(), voiceProfileID)
		if err != nil {
			return mapWriteError(op, err)
		}
		return errFromAffected(res, op, 1)
	})
}

// GetVoiceGain reads one voice revision's gain. ok is false for a missing
// revision (the API maps it to 404).
func (s *Store) GetVoiceGain(ctx context.Context, voiceProfileID string) (float64, bool, error) {
	const op = "get voice gain"
	var gain float64
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return mapReadError(op, q.queryRow(ctx, op,
			`SELECT gain FROM voice_profiles WHERE id = ?`, voiceProfileID).
			Scan(&gain))
	})
	if err != nil {
		if err := errorIsNotFound(err); err != nil {
			return 0, false, nil
		}
		return 0, false, err
	}
	return gain, true, nil
}
