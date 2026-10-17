package voices

// Fakes for the domain unit tests (docs/spec/12 "Unit" mode): an in-memory
// Store, a scripted design/base client that answers with deterministic WAVs,
// and an in-memory registry with the same identity rules tts.Registry applies
// (name digest must match the reference content; names are immutable).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sync"

	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/store"
)

// fakeStore is an in-memory voices.Store.
type fakeStore struct {
	mu        sync.Mutex
	profiles  map[string]store.VoiceProfile
	auditions map[string]store.VoiceAudition
	regs      map[string]store.VoiceRegistration // key: profileID/instance
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		profiles:  map[string]store.VoiceProfile{},
		auditions: map[string]store.VoiceAudition{},
		regs:      map[string]store.VoiceRegistration{},
	}
}

func regKey(profileID, instance string) string { return profileID + "/" + instance }

func (f *fakeStore) CreateVoiceRevision(_ context.Context, p store.VoiceProfile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.profiles[p.ID]; ok {
		return fmt.Errorf("exists")
	}
	f.profiles[p.ID] = p
	return nil
}

func (f *fakeStore) UpdateVoiceProfile(_ context.Context, p store.VoiceProfile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.profiles[p.ID]
	if !ok {
		return fmt.Errorf("not found: %w", store.ErrNotFound)
	}
	if !voiceTransitionOK(cur.State, p.State) {
		return fmt.Errorf("immutable: %w", store.ErrImmutable)
	}
	p.CreatedAt = cur.CreatedAt
	f.profiles[p.ID] = p
	return nil
}

func voiceTransitionOK(from, to string) bool {
	switch {
	case from == store.VoiceStateDraft && to == store.VoiceStateAuditioning:
		return true
	case from == store.VoiceStateAuditioning && to == store.VoiceStateApproved:
		return true
	case from == store.VoiceStateApproved && to == store.VoiceStateRetired:
		return true
	}
	return false
}

func (f *fakeStore) GetVoiceRevision(_ context.Context, id string) (store.VoiceProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.profiles[id]
	if !ok {
		return store.VoiceProfile{}, fmt.Errorf("not found: %w", store.ErrNotFound)
	}
	return p, nil
}

func (f *fakeStore) ListVoiceProfiles(_ context.Context, character string) ([]store.VoiceProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.VoiceProfile
	for _, p := range f.profiles {
		if character == "" || p.Character == character {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) ListApprovedVoiceRevisions(_ context.Context) ([]store.VoiceProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.VoiceProfile
	for _, p := range f.profiles {
		if p.State == store.VoiceStateApproved && p.ReferenceAssetID != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) NextVoiceRevision(_ context.Context, character string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	max := 0
	for _, p := range f.profiles {
		if p.Character == character && p.Revision > max {
			max = p.Revision
		}
	}
	return max + 1, nil
}

func (f *fakeStore) AddAuditions(_ context.Context, profileID string, ids []string, auditions []store.VoiceAudition) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.profiles[profileID]
	if !ok || p.State != store.VoiceStateDraft {
		return fmt.Errorf("not found: %w", store.ErrNotFound)
	}
	for _, a := range auditions {
		f.auditions[a.ID] = a
	}
	p.State = store.VoiceStateAuditioning
	p.AuditionIDs = ids
	f.profiles[profileID] = p
	return nil
}

func (f *fakeStore) ListAuditions(_ context.Context, profileID string) ([]store.VoiceAudition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.VoiceAudition
	for _, a := range f.auditions {
		if a.VoiceProfileID == profileID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) GetAudition(_ context.Context, id string) (store.VoiceAudition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.auditions[id]
	if !ok {
		return store.VoiceAudition{}, fmt.Errorf("not found: %w", store.ErrNotFound)
	}
	return a, nil
}

func (f *fakeStore) UpsertVoiceRegistration(_ context.Context, r store.VoiceRegistration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regs[regKey(r.VoiceProfileID, r.InstanceID)] = r
	return nil
}

func (f *fakeStore) GetVoiceRegistration(_ context.Context, profileID, instance string) (store.VoiceRegistration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.regs[regKey(profileID, instance)]
	if !ok {
		return store.VoiceRegistration{}, fmt.Errorf("not found: %w", store.ErrNotFound)
	}
	return r, nil
}

func (f *fakeStore) ListVoiceRegistrations(_ context.Context, profileID string) ([]store.VoiceRegistration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.VoiceRegistration
	for _, r := range f.regs {
		if r.VoiceProfileID == profileID {
			out = append(out, r)
		}
	}
	return out, nil
}

// fakeRegistry is an in-memory tts.RegistryClient with the identity rules the
// real one enforces: a name's digest8 must equal the reference's digest8, and
// a name cannot be re-uploaded with different content. conflictNames simulates
// the report Ensure produces when a name's stored content contradicts its
// identity (the domain under test is what it does with that report).
type fakeRegistry struct {
	mu            sync.Mutex
	voices        map[string][]byte // name -> reference content
	dropped       map[string]bool   // names deleted by a "provider restart"
	conflictNames map[string]bool
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		voices:        map[string][]byte{},
		dropped:       map[string]bool{},
		conflictNames: map[string]bool{},
	}
}

// drop simulates a provider restart or a lost replica: the voice is gone.
func (r *fakeRegistry) drop(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.voices, name)
	r.dropped[name] = true
}

func (r *fakeRegistry) List(_ context.Context) (tts.ListResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for n, b := range r.voices {
		if r.dropped[n] || len(b) == 0 {
			continue
		}
		names = append(names, n)
	}
	return tts.ListResult{Voices: names}, nil
}

func (r *fakeRegistry) Ensure(_ context.Context, required []tts.Voice) (tts.EnsureReport, error) {
	var rep tts.EnsureReport
	listed, _ := r.List(context.Background())
	have := map[string]bool{}
	for _, n := range listed.Voices {
		have[n] = true
	}
	for _, v := range required {
		if r.conflictNames[v.Name] {
			rep.Conflicts = append(rep.Conflicts, tts.Conflict{
				Name: v.Name, Reason: "name already registered with different content",
			})
			continue
		}
		want := tts.Digest8(v.RefAudio)
		if len(v.Name) < 9 || v.Name[len(v.Name)-8:] != want {
			rep.Conflicts = append(rep.Conflicts, tts.Conflict{
				Name: v.Name, Reason: "name digest does not match the reference audio content",
			})
			continue
		}
		if have[v.Name] {
			rep.Present = append(rep.Present, v.Name)
			continue
		}
		r.mu.Lock()
		if cur, ok := r.voices[v.Name]; ok && !r.dropped[v.Name] && string(cur) != string(v.RefAudio) {
			r.mu.Unlock()
			rep.Conflicts = append(rep.Conflicts, tts.Conflict{Name: v.Name, Reason: "content differs"})
			continue
		}
		r.voices[v.Name] = v.RefAudio
		delete(r.dropped, v.Name)
		r.mu.Unlock()
		rep.Registered = append(rep.Registered, v.Name)
	}
	return rep, nil
}

// wavTone renders one deterministic valid PCM16 WAV (non-silent, unclipped):
// the same shape the tts test fixture is, generated per index so two "design
// calls" can return different bytes.
func wavTone(freqHz float64, seconds int) []byte {
	const rate = 24000
	n := rate * seconds
	data := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(9000 * math.Sin(2*math.Pi*freqHz*float64(i)/rate))
		data[2*i] = byte(s)
		data[2*i+1] = byte(s >> 8)
	}
	out := make([]byte, 0, 44+len(data))
	out = append(out, "RIFF"...)
	out = appendU32(out, 36+uint32(len(data)))
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	out = appendU32(out, 16)
	out = appendU16(out, 1)
	out = appendU16(out, 1)
	out = appendU32(out, rate)
	out = appendU32(out, rate*2)
	out = appendU16(out, 2)
	out = appendU16(out, 16)
	out = append(out, "data"...)
	out = appendU32(out, uint32(len(data)))
	out = append(out, data...)
	return out
}

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// sha256Hex is the content identity of one blob.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
