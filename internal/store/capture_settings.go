package store

// The persisted capture selection (ADR-0078): one app_settings key/value row,
// the same mechanism as the audio row (ADR-0074) — a JSON value under a new
// key, so no schema migration in either direction, and a process that
// predates the row reads ok=false and falls through to the first device.
//
// The row is the DEFAULT the capture stack resolves to: an explicit device id
// in a session request outranks it, and the operator's pinned mode outranks
// the ADR-0076 policy while it stays an offered mode of the device. The
// validation against the live enumeration happens at the API's PUT route (the
// store cannot see the enumeration), so the store's own bounds are the
// value-shape ones: a positive geometry and a rate, a known pixel format.

import (
	"context"
	"encoding/json"
	"fmt"
)

// CaptureSettingsKey is the app_settings key of the capture row.
const CaptureSettingsKey = "capture"

// CaptureMode is the pinned capture mode (the API view's spelling of the
// mode package's Mode; the store does not import the capture tree, and the
// four fields are the whole contract).
type CaptureMode struct {
	W      int     `json:"w"`
	H      int     `json:"h"`
	FPS    float64 `json:"fps"`
	PixFmt string  `json:"pix_fmt"`
}

// CaptureSettings is the persisted capture selection (one app_settings row).
// An empty DeviceID means "never chosen" (the stack then falls to the first
// usable device); a nil Mode means "the policy decides for the chosen
// device".
type CaptureSettings struct {
	DeviceID string       `json:"device_id"`
	Mode     *CaptureMode `json:"mode,omitempty"`
}

// GetCaptureSettings reads the capture row. ok is false when no row exists
// (the user never chose a selection).
func (s *Store) GetCaptureSettings(ctx context.Context) (CaptureSettings, bool, error) {
	const op = "get capture settings"
	var value string
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return mapReadError(op, q.queryRow(ctx, op,
			`SELECT value FROM app_settings WHERE key = ?`, CaptureSettingsKey).
			Scan(&value))
	})
	if err != nil {
		if err := errorIsNotFound(err); err != nil {
			return CaptureSettings{}, false, nil
		}
		return CaptureSettings{}, false, err
	}
	var a CaptureSettings
	if err := json.Unmarshal([]byte(value), &a); err != nil {
		// A row the writer of this table cannot read is a store defect,
		// reported loudly (the resolution then falls back to the first
		// device, but the 500 is the operator's business, not the page's
		// silence).
		return CaptureSettings{}, false, fmt.Errorf("store: %s: stored value does not decode: %w", op, err)
	}
	return a, true, nil
}

// SetCaptureSettings writes the capture row (upsert). The value is validated
// against the value-shape bounds (the API validates against the enumeration,
// which the store cannot see).
func (s *Store) SetCaptureSettings(ctx context.Context, a CaptureSettings) error {
	const op = "set capture settings"
	if a.DeviceID != "" && len(a.DeviceID) > 512 {
		return fmt.Errorf("store: %s: device id longer than 512 bytes", op)
	}
	if m := a.Mode; m != nil {
		if m.W <= 0 || m.H <= 0 || m.FPS <= 0 {
			return fmt.Errorf("store: %s: a pinned mode needs a positive geometry and rate", op)
		}
		switch m.PixFmt {
		case "mjpeg", "yuyv422":
		default:
			return fmt.Errorf("store: %s: pinned mode pixel format %q: want mjpeg or yuyv422", op, m.PixFmt)
		}
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
			CaptureSettingsKey, string(value), nowStamp())
		if err != nil {
			return mapWriteError(op, err)
		}
		_ = res
		return nil
	})
}
