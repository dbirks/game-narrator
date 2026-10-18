package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

// The TTS preflight probes (ADR-0022). The voice_design and voice_clone roles
// replaced the DEGRADED "client not implemented yet" placeholder with real
// probes: a 3-word VoiceDesign synthesis for the design role, and for the
// clone role a listing plus a synthesis with a listed voice when a direct
// registry URL is configured, else a synthesis with the built-in "default".
// Every error is reported as FAIL with its taxonomy kind; NOT CONFIGURED is
// handled by the generic branch in checkRole when no model is set.

// VoiceProbe is the slice of the TTS client the doctor needs. Injecting it
// keeps the voice checks testable without a gateway, mirroring ProviderProbe
// and DecisionProbe.
type VoiceProbe interface {
	// Design synthesises a short reference clip on the VoiceDesign checkpoint.
	Design(ctx context.Context, cfg providers.RoleConfig, req tts.DesignRequest) (tts.Audio, error)
	// Generate synthesises one line in a voice on the Base checkpoint.
	Generate(ctx context.Context, cfg providers.RoleConfig, req tts.GenerateRequest) (tts.Audio, error)
	// ListVoices lists the instance's voices through the registry at
	// registryURL; registryURL == "" returns nil, nil (not an error).
	ListVoices(ctx context.Context, cfg providers.RoleConfig, registryURL string) ([]string, error)
	// ProbeReference negotiates the inline ref_audio capability
	// (docs/spec/06: "negotiated and reported in preflight").
	ProbeReference(ctx context.Context, cfg providers.RoleConfig) (tts.ReferenceProbe, error)
}

// voiceProbe is the real VoiceProbe: it builds the same adapters the runtime
// uses and sends real requests.
type voiceProbe struct{}

// The probe texts. Three words, as the check contract requires.
const (
	voiceDesignProbeText  = "Probe the light."
	voiceDesignProbeInstr = "A calm narrator voice."
	voiceCloneProbeText   = "Probe the registered voice."
	defaultProbeVoice     = "default"
)

// Registry URL env vars. The role-level variable is the documented runtime
// configuration; the orchestrator's live-test variable is honoured as a
// fallback so a host that only has the port-forward still probes honestly.
const (
	envVoiceRegistryURL = "GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"
	envTTSBaseDirectURL = "GAME_NARRATOR_TTS_BASE_DIRECT_URL"
)

func (voiceProbe) Design(ctx context.Context, cfg providers.RoleConfig, req tts.DesignRequest) (tts.Audio, error) {
	client, err := tts.NewFromRole(cfg)
	if err != nil {
		return tts.Audio{}, err
	}
	voiceTransport(client)
	return client.Design(ctx, req)
}

func (voiceProbe) Generate(ctx context.Context, cfg providers.RoleConfig, req tts.GenerateRequest) (tts.Audio, error) {
	client, err := tts.NewFromRole(cfg)
	if err != nil {
		return tts.Audio{}, err
	}
	voiceTransport(client)
	return client.Generate(ctx, req)
}

func (voiceProbe) ListVoices(ctx context.Context, cfg providers.RoleConfig, registryURL string) ([]string, error) {
	if strings.TrimSpace(registryURL) == "" {
		return nil, nil
	}
	reg, err := tts.NewRegistry(registryURL, cfg.Token())
	if err != nil {
		return nil, err
	}
	listed, err := reg.List(ctx)
	if err != nil {
		return nil, err
	}
	return listed.Voices, nil
}

func (voiceProbe) ProbeReference(ctx context.Context, cfg providers.RoleConfig) (tts.ReferenceProbe, error) {
	client, err := tts.NewFromRole(cfg)
	if err != nil {
		return tts.ReferenceProbe{}, err
	}
	voiceTransport(client)
	probe := client.ProbeReferenceSupport(ctx)
	if probe.Probed {
		return probe, nil
	}
	return probe, fmt.Errorf("reference probe did not complete")
}

// checkVoiceRole runs the real TTS probes for the two voice roles.
func (r *runner) checkVoiceRole(ctx context.Context, role providers.Role, cfg providers.RoleConfig) Check {
	probe := r.opts.VoiceProbe
	if probe == nil {
		probe = voiceProbe{}
	}
	if role == providers.RoleVoiceDesign {
		return r.checkVoiceDesign(ctx, role, cfg, probe)
	}
	return r.checkVoiceClone(ctx, role, cfg, probe)
}

// checkVoiceDesign: a 3-word Design with a short instruction; OK with the
// measured duration and latency when the checkpoint returns a valid WAV.
func (r *runner) checkVoiceDesign(ctx context.Context, role providers.Role, cfg providers.RoleConfig, probe VoiceProbe) Check {
	a, err := probe.Design(ctx, cfg, tts.DesignRequest{
		Text:         voiceDesignProbeText,
		Instructions: voiceDesignProbeInstr,
	})
	if err != nil {
		return roleFailure(role, cfg, "voice design probe", err)
	}
	summary := fmt.Sprintf("model %s designed a %s reference clip in %s",
		cfg.Model, a.Duration.Round(time.Millisecond), a.Latency.Round(time.Millisecond))
	detail := fmt.Sprintf("sample rate %d Hz, %d-bit PCM; origin %s", a.SampleRate, a.BitsPerSample, a.Origin)
	return Check{Status: StatusOK, Summary: summary, Detail: detail}
}

// checkVoiceClone: with a registry URL, list the instance's voices and
// synthesise with one of them (registered voices first: built-in names are
// not resolvable by a Base checkpoint, verified live); without one, probe the
// built-in "default". The inline-reference negotiation is recorded in the
// detail line, as docs/spec/06 requires.
func (r *runner) checkVoiceClone(ctx context.Context, role providers.Role, cfg providers.RoleConfig, probe VoiceProbe) Check {
	registryURL, usedFallback := r.registryURL()
	var voice string
	var listed int
	// noRegisteredVoice marks the probe as having no registered voice to
	// try: with a registry URL the instance listed zero voices, without one
	// the probe speaks the built-in "default" which a Base checkpoint only
	// resolves when the runtime has pushed a voice to the instance.
	noRegisteredVoice := false
	if registryURL != "" {
		names, err := probe.ListVoices(ctx, cfg, registryURL)
		if err != nil {
			return roleFailure(role, cfg, "voice registry listing", err)
		}
		if len(names) == 0 {
			// Fresh install: the instance holds no voice yet. The Base
			// checkpoint cannot speak without one, so this is the expected
			// "nothing is configured for this component — not a fault"
			// state (docs/spec/13), not a broken voice.
			return noRegisteredVoiceCheck(role, cfg, "the voice registry lists no voices yet")
		}
		listed = len(names)
		voice = pickProbeVoice(names)
	} else {
		voice = defaultProbeVoice
		noRegisteredVoice = true
	}

	a, err := probe.Generate(ctx, cfg, tts.GenerateRequest{Text: voiceCloneProbeText, Voice: voice})
	if err != nil {
		// The Base checkpoint reports an unknown voice as voice_missing —
		// the "requires 'ref_audio' or 'speaker_embedding'" 400 when the
		// instance holds no embedding for the name. With no registered voice
		// to try, that is the fresh-install state the first device run hit:
		// NOT CONFIGURED, not a FAIL. Any other kind, or a voice_missing on
		// a voice the registry actually listed (a real synthesis failure
		// for an existing registered voice), stays FAIL.
		var pe *providers.Error
		if errors.As(err, &pe) && pe.Kind == providers.KindVoiceMissing && noRegisteredVoice {
			return noRegisteredVoiceCheck(role, cfg, safeText(err.Error(), cfg.Token()))
		}
		c := roleFailure(role, cfg, "voice synthesis probe", err)
		if registryURL == "" {
			c.Remedy = c.Remedy + "; built-in voices may be absent on a Base checkpoint — set " +
				envVoiceRegistryURL + " to the direct Base service to probe a registered voice instead"
		}
		return c
	}

	summary := fmt.Sprintf("model %s spoke the probe line in %s with voice %q",
		cfg.Model, a.Latency.Round(time.Millisecond), voice)
	detail := fmt.Sprintf("duration %s, sample rate %d Hz; ", a.Duration.Round(time.Millisecond), a.SampleRate)
	if registryURL != "" {
		detail += fmt.Sprintf("%d voices listed on the instance", listed)
		if usedFallback {
			detail += " (registry URL from " + envTTSBaseDirectURL + ")"
		}
	}

	// Best-effort: the inline-reference negotiation. A failed probe must not
	// fail the check — it is a fallback capability, and the check already
	// proved the primary path works.
	if ref, refErr := probe.ProbeReference(ctx, cfg); refErr == nil && ref.Probed {
		if ref.Supported {
			detail += "; inline ref_audio supported"
		} else {
			detail += "; inline ref_audio unsupported"
		}
	} else {
		detail += "; inline ref_audio not probed"
	}
	return Check{Status: StatusOK, Summary: summary, Detail: detail}
}

// noRegisteredVoiceCheck renders the expected fresh-install state of the
// voice_clone role: the Base checkpoint has no voice to speak with yet.
// evidence, when non-empty, is the probe's own failure line (the gateway's
// 400 saying the name resolves to no stored embedding), already redacted.
func noRegisteredVoiceCheck(role providers.Role, cfg providers.RoleConfig, evidence string) Check {
	detail := "model " + cfg.Model + ": the Base checkpoint cannot speak without a registered voice; a fresh install is expected to report this"
	if evidence != "" {
		detail += "; probe evidence: " + evidence
	}
	return Check{
		Status:  StatusNotConfigured,
		Summary: "no registered voice yet; register one from the Voices page",
		Detail:  detail,
		Remedy:  "register a voice from the Voices page (or load built-in voices on the instance), then re-run doctor",
	}
}

// pickProbeVoice orders the listing for the probe: names from our own
// registered namespace first (they are the voices the runtime reconciles),
// then any non-default name, then the built-in default, then the first name.
func pickProbeVoice(names []string) string {
	for _, n := range names {
		if strings.HasPrefix(n, "gn_") {
			return n
		}
	}
	for _, n := range names {
		if n != defaultProbeVoice {
			return n
		}
	}
	for _, n := range names {
		if n == defaultProbeVoice {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return defaultProbeVoice
}

// registryURL resolves the direct Base service URL for registry probes.
// The second result reports the fallback variable, so the detail line can say
// which configuration the probe actually used.
func (r *runner) registryURL() (url string, usedFallback bool) {
	if v, ok := r.opts.LookupEnv(envVoiceRegistryURL); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), false
	}
	if v, ok := r.opts.LookupEnv(envTTSBaseDirectURL); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v), true
	}
	return "", false
}

// voiceTransport is shared by the real probe clients: the role's runtime
// latency budget is removed as a transport timeout, exactly as the decision
// probe does — 120 s bounds an in-loop utterance, not a diagnostic a user
// runs deliberately, where a cold model load is normal. The check's own
// context deadline (doctor.Options.ModelTimeout) bounds the probe instead.
func voiceTransport(c *tts.Client) {
	transport := http.Client{}
	if c.HTTP != nil {
		transport = *c.HTTP
	}
	transport.Timeout = 0
	c.HTTP = &transport
}
