package support

import (
	"context"

	"github.com/dbirks/game-narrator/internal/doctor"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

// doctorDoc wraps the doctor report with the mode note, so doctor.json states
// up front whether the provider rows were probed for real.
type doctorDoc struct {
	Mode   string        `json:"mode"`
	Note   string        `json:"note,omitempty"`
	Report doctor.Report `json:"report"`
}

const offlineNote = "network provider probes were skipped (offline bundle): no request was sent to any gateway, so configured roles report as not reachable rather than as working. For a live report run 'game-narrator doctor', or rebuild the bundle with --probe"

// buildDoctor runs the preflight. Offline (the default) every provider probe
// is replaced by one that answers with the skip note and never dials; local
// checks (storage, tools, OCR image, capture devices, audio sinks) run for
// real either way, so the bundle always says what is actually there.
func buildDoctor(ctx context.Context, o Options) ([]byte, string, error) {
	opts := doctor.Options{DataDir: o.DataDir}
	mode := DoctorModeOffline
	var note string
	if !o.Probe {
		off := offlineProbe{note: offlineNote}
		opts.Probe = off
		opts.DecisionProbe = off
		opts.VoiceProbe = off
		note = offlineNote
	} else {
		mode = DoctorModeProbe
	}
	report := doctor.Run(ctx, opts)
	doc := doctorDoc{Mode: mode, Note: note, Report: report}
	data, err := marshalDoc(doc)
	if err != nil {
		return nil, "", err
	}
	return data, mode, nil
}

// offlineProbe answers every model probe with the skip note instead of
// dialing a gateway. It implements all three probe interfaces the doctor
// accepts (OpenAI provider, decision, voice), so a bundle build is
// network-free by construction, not by convention.
type offlineProbe struct{ note string }

func (p offlineProbe) skip() error {
	return providers.NewError(providers.KindUnavailable, 0, p.note)
}

func (p offlineProbe) ListModels(context.Context, providers.RoleConfig) ([]string, error) {
	return nil, p.skip()
}

func (p offlineProbe) Chat(context.Context, providers.RoleConfig, string, [][]byte) (doctor.ChatResult, error) {
	return doctor.ChatResult{}, p.skip()
}

func (p offlineProbe) ProbeCapabilities(context.Context, providers.RoleConfig) (openai.Capabilities, error) {
	return openai.Capabilities{}, p.skip()
}

func (p offlineProbe) Decide(context.Context, providers.RoleConfig, systemone.Request) (systemone.Decision, error) {
	return systemone.Decision{}, p.skip()
}

func (p offlineProbe) Design(context.Context, providers.RoleConfig, tts.DesignRequest) (tts.Audio, error) {
	return tts.Audio{}, p.skip()
}

func (p offlineProbe) Generate(context.Context, providers.RoleConfig, tts.GenerateRequest) (tts.Audio, error) {
	return tts.Audio{}, p.skip()
}

func (p offlineProbe) ListVoices(context.Context, providers.RoleConfig, string) ([]string, error) {
	return nil, p.skip()
}

func (p offlineProbe) ProbeReference(context.Context, providers.RoleConfig) (tts.ReferenceProbe, error) {
	return tts.ReferenceProbe{}, p.skip()
}
