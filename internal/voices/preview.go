package voices

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// PreviewResult is one resolved preview line: the playable WAV plus the cache
// evidence (docs/spec/11 "avoiding false positives": a WAV is not proof of
// anything by itself, so the hit/miss flag is returned with it, not inferred
// from it).
type PreviewResult struct {
	WAV      []byte
	SHA256   string
	Duration time.Duration
	// FromCache is true when the bytes came from the cache rather than a fresh
	// Base call — the miss-then-hit contract of docs/spec/07 step 7.
	FromCache  bool
	CacheKey   speech.CacheKey
	Generation string // the provider origin recorded on the audio
}

// Preview synthesises one line in an approved revision's voice through the
// speech cache (docs/spec/07 "Cache"): the key carries the revision's
// fingerprint, so a second identical preview is a cache hit with no provider
// call, and a re-approval (new revision, new fingerprint) is a disjoint key
// space — old previews stay cached and attributable to the old revision
// (step 8).
//
// When the voice is registered on this instance the Base checkpoint speaks
// the registered name; otherwise the call carries the approved reference
// inline (docs/spec/06 fallback: negotiated, not assumed — GenerateWith
// Reference itself probes and reports the capability).
func (s *Service) Preview(ctx context.Context, revisionID, text string) (PreviewResult, error) {
	if len(text) == 0 {
		return PreviewResult{}, fmt.Errorf("voices: preview text is empty")
	}
	rev, err := s.Store.GetVoiceRevision(ctx, revisionID)
	if err != nil {
		return PreviewResult{}, err
	}
	if rev.State != store.VoiceStateApproved || rev.Fingerprint == "" || rev.ReferenceAssetID == "" {
		return PreviewResult{}, fmt.Errorf("%w: state %s", ErrNotApproved, rev.State)
	}
	key, err := previewCacheKey(rev, text, s.BackendDigest)
	if err != nil {
		return PreviewResult{}, err
	}

	// Resolve the voice identity before the generation function so the
	// decision (registered name vs inline reference) is made once and is the
	// same for every caller singleflighting on this key.
	reg, err := s.Store.GetVoiceRegistration(ctx, revisionID, s.InstanceID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return PreviewResult{}, err
	}
	name := reg.RegisteredName
	ready := reg.State == store.VoiceRegReady
	if name == "" {
		name, err = s.nameFor(ctx, rev)
		if err != nil {
			return PreviewResult{}, err
		}
	}

	var gen speech.GenerationFunc
	if ready {
		gen = func(ctx context.Context) (tts.Audio, error) {
			return s.Base.Generate(ctx, tts.GenerateRequest{
				Text: text, Voice: name, Language: rev.Language,
			})
		}
	} else {
		ref, err := s.loadReference(ctx, rev.ReferenceAssetID)
		if err != nil {
			return PreviewResult{}, err
		}
		gen = func(ctx context.Context) (tts.Audio, error) {
			return s.Base.GenerateWithReference(ctx, tts.GenerateRequest{
				Text: text, Voice: name, Language: rev.Language,
			}, ref, rev.RefTranscript)
		}
	}

	res, err := s.Speech.GetOrGenerate(ctx, key, speech.PriorityCurrent, gen)
	if err != nil {
		return PreviewResult{}, fmt.Errorf("voices: preview: %w", err)
	}
	return PreviewResult{
		WAV:       res.WAV,
		SHA256:    res.SHA256,
		Duration:  res.Duration,
		FromCache: res.FromCache,
		CacheKey:  key,
	}, nil
}
