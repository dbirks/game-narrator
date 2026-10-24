package cast

// The store-backed audit engine (ADR-0171): for a profile's roster, find the
// voice the runtime actually speaks each character with — the highest
// approved revision carrying a fingerprint and a reference asset, the same
// selection rule internal/runtime.approvedVoiceForKey uses — measure that
// reference's median F0 with the YIN estimator, and compare it with the
// character's expected class. Read-only: it writes nothing.

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// AssignedVoice is one character's voice the runtime speaks with.
type AssignedVoice struct {
	CharacterName string
	VoiceID       string
	Revision      int
	// Brief is the voice design instruction (the full brief stored on the
	// revision); the expected class reads it too.
	Brief        string
	ReferenceSHA string
}

// Auditor runs read-only audits over one durable store.
type Auditor struct {
	Store  *store.Store
	Assets assets.Store
}

// NewAuditor builds the read-only auditor.
func NewAuditor(st *store.Store, a assets.Store) *Auditor {
	return &Auditor{Store: st, Assets: a}
}

// AssignedVoiceOf returns the character's spoken voice per the runtime's
// selection rule, or a zero value when the character has no approved voice.
func (a *Auditor) AssignedVoiceOf(ctx context.Context, character string) (AssignedVoice, error) {
	revs, err := a.Store.ListVoiceProfiles(ctx, character)
	if err != nil {
		return AssignedVoice{}, fmt.Errorf("cast audit: list voices of %q: %w", character, err)
	}
	for _, p := range revs { // ordered by revision, descending
		if p.State != store.VoiceStateApproved || p.Fingerprint == "" || p.ReferenceAssetID == "" {
			continue
		}
		return AssignedVoice{
			CharacterName: character,
			VoiceID:       p.ID,
			Revision:      p.Revision,
			Brief:         p.DesignInstruction,
			ReferenceSHA:  p.ReferenceAssetID,
		}, nil
	}
	return AssignedVoice{}, nil
}

// Profile audits every roster row of one profile. Characters without an
// assigned voice carry a row with HasVoice false and Verdict UNKNOWN: the
// audit reports absence, never invents a measurement.
func (a *Auditor) Profile(ctx context.Context, profileID string) ([]Row, error) {
	if _, _, err := a.Store.GetProfile(ctx, profileID); err != nil {
		return nil, fmt.Errorf("cast audit: load profile %s: %w", profileID, err)
	}
	chars, err := a.Store.ListCharacters(ctx, profileID)
	if err != nil {
		return nil, fmt.Errorf("cast audit: list characters of %s: %w", profileID, err)
	}
	rows := make([]Row, 0, len(chars))
	for _, c := range chars {
		text := c.PersonalitySummary + " " + c.Role
		v, err := a.AssignedVoiceOf(ctx, c.Name)
		if err != nil {
			return nil, err
		}
		ch := Character{ProfileName: c.Name, ProfileText: text}
		if v.VoiceID != "" {
			ch.HasVoice = true
			ch.VoiceID = v.VoiceID
			ch.ProfileText += " " + v.Brief
			p, err := a.measureReference(ctx, v.ReferenceSHA)
			if err != nil {
				return nil, fmt.Errorf("cast audit: measure reference of %s's voice: %w", c.Name, err)
			}
			ch.MeasuredF0 = p.F0
			ch.VoicedFrames = p.VoicedFrames
		}
		rows = append(rows, RowFor(ch))
	}
	return rows, nil
}

// pitchCache memoises measured reference pitch by the reference asset's
// SHA-256. The asset store is content-addressed, so a SHA always names the same
// bytes and its pitch never changes. Without it the Cast page's audit decoded
// and pitch-tracked every reference on every view: 15 s on the UNO Q for the
// 29-voice Wind Waker cast. Bounded; when full it is cleared, since a
// re-measure is only a cost, never a wrong answer.
var pitchCache = struct {
	sync.Mutex
	m map[string]speech.Pitch
}{m: map[string]speech.Pitch{}}

const pitchCacheMax = 4096

// measurePitch is the measurement the cache wraps (a seam for tests).
var measurePitch = speech.MeasurePitch

func (a *Auditor) measureReference(ctx context.Context, sha string) (speech.Pitch, error) {
	pitchCache.Lock()
	p, ok := pitchCache.m[sha]
	pitchCache.Unlock()
	if ok {
		return p, nil
	}
	wav, err := a.referenceBytes(ctx, sha)
	if err != nil {
		return speech.Pitch{}, err
	}
	p, err = measurePitch(wav)
	if err != nil {
		return speech.Pitch{}, err
	}
	pitchCache.Lock()
	if len(pitchCache.m) >= pitchCacheMax {
		pitchCache.m = map[string]speech.Pitch{}
	}
	pitchCache.m[sha] = p
	pitchCache.Unlock()
	return p, nil
}

func (a *Auditor) referenceBytes(ctx context.Context, sha string) ([]byte, error) {
	r, _, err := a.Assets.Get(ctx, sha)
	if err != nil {
		return nil, fmt.Errorf("cast audit: read reference asset: %w", err)
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		return nil, fmt.Errorf("cast audit: read reference asset: %w", err)
	}
	return buf.Bytes(), nil
}

// Mismatches filters a row set to its MISMATCH rows.
func Mismatches(rows []Row) []Row {
	out := make([]Row, 0)
	for _, r := range rows {
		if r.Verdict == VerdictMismatch {
			out = append(out, r)
		}
	}
	return out
}
