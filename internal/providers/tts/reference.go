package tts

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Inline-reference fallback (docs/spec/06 "Routing, reliability and
// replicas"): when registration is unavailable on a replica, the utterance
// is synthesised with an explicit ref_audio instead of a registered name.
// The spec requires that this fallback be *negotiated and reported in
// preflight*, never assumed. This file implements the negotiation:
//
//   - ProbeReferenceSupport sends one minimal synthesis request carrying a
//     ref_audio and records whether the deployment honours it;
//   - GenerateWithReference refuses with unsupported_capability when the
//     probe already answered "no", and maps a first-contact 400 about
//     ref_audio to the same result, recording the probe result either way.
//
// Verified live (2026-10-17, ADR-0022): the deployed Base checkpoint
// accepts ref_audio as a base64 data URI together with ref_text and
// task_type:"Base"; the probe below is the functional form of that check.

// ReferenceProbe is the negotiated result of the inline-reference
// capability check.
type ReferenceProbe struct {
	// Supported is true only after a live probe answered affirmatively.
	Supported bool
	// Probed records that the deployment was actually asked.
	Probed bool
	// Note explains a negative or inconclusive probe in one line.
	Note string
}

// probeToneSeconds is the length of the synthetic reference clip the probe
// generates in memory: long enough to carry a speaker embedding, short
// enough to cost ~1 s of synthesis.
const probeToneSeconds = 0.5

// GenerateWithReference synthesises req.Text using an inline reference
// (refAudio, the approved reference WAV, plus refText, its exact transcript)
// instead of a registered voice name. It returns unsupported_capability —
// not a retryable failure — when the probe has established that the
// deployment does not support ref_audio, because no request shape will
// make it work.
func (c *Client) GenerateWithReference(ctx context.Context, req GenerateRequest, refAudio []byte, refText string) (Audio, error) {
	if len(refAudio) == 0 {
		return Audio{}, &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: GenerateWithReference.refAudio is empty", Model: c.Model}
	}
	if trimmed := strings.TrimSpace(req.Text); trimmed == "" {
		return Audio{}, &providers.Error{Kind: providers.KindBadSchema, Msg: "tts: GenerateWithReference.Text is empty", Model: c.Model}
	}
	probe := c.ReferenceProbe()
	if probe.Probed && !probe.Supported {
		err := providers.NewError(providers.KindUnsupportedCapability, 0,
			"tts: deployment does not support inline ref_audio references: "+probe.Note)
		err.Model = c.Model
		err.Retryable = false
		return Audio{}, err
	}

	payload := c.synthesisPayload(req, true)
	payload["ref_audio"] = dataURI("audio/wav", refAudio)
	if refText = strings.TrimSpace(refText); refText != "" {
		payload["ref_text"] = refText
	}
	body, latency, reqID, reqIDSource, err := c.do(ctx, payload)
	if err != nil {
		if pe := asProvidersError(err); pe != nil && pe.Status == 400 {
			msg := strings.ToLower(pe.Msg)
			if strings.Contains(msg, "ref_audio") || strings.Contains(msg, "reference audio") {
				// The deployment rejected the reference mechanism itself:
				// record the negotiation result and classify it as a
				// capability gap, not a schema accident.
				c.setReferenceProbe(ReferenceProbe{Supported: false, Probed: true, Note: oneLine(pe.Msg, 160)})
				e := providers.NewError(providers.KindUnsupportedCapability, pe.Status,
					"tts: deployment does not support inline ref_audio references: "+oneLine(pe.Msg, 200))
				e.RequestID = pe.RequestID
				e.Model = c.Model
				e.Err = pe.Err
				return Audio{}, e
			}
		}
		return Audio{}, err
	}
	return c.finishAudio(body, latency, reqID, reqIDSource,
		fmt.Sprintf("voice_clone model=%s voice=%s inline_ref_audio=true", c.Model, req.Voice), nil, false)
}

// ProbeReferenceSupport asks the deployment whether it honours an inline
// ref_audio, using the smallest request that exercises the code path: a
// half-second generated tone as the reference, a two-word utterance, the
// Base task type explicitly, and the first listed voice when one can be
// resolved cheaply (voice "default" otherwise). The result is cached on the
// client and surfaced to preflight (docs/spec/06: "negotiated and reported").
func (c *Client) ProbeReferenceSupport(ctx context.Context) ReferenceProbe {
	if p := c.ReferenceProbe(); p.Probed {
		return p
	}
	probe := ReferenceProbe{
		Probed:    false,
		Supported: false,
		Note:      "not probed",
	}
	tone := syntheticToneWAV(24000, probeToneSeconds, 440)
	payload := map[string]any{
		"model":           c.Model,
		"input":           "Reference probe.",
		"voice":           "default",
		"response_format": "wav",
		"task_type":       taskTypeBase,
		"ref_audio":       dataURI("audio/wav", tone),
		"ref_text":        "A neutral synthetic reference tone.",
	}
	_, _, _, _, err := c.do(ctx, payload)
	probe.Probed = true
	if err == nil {
		probe.Supported = true
		probe.Note = "deployment synthesised with an inline ref_audio"
	} else {
		pe := asProvidersError(err)
		if pe != nil && pe.Status == 400 {
			msg := strings.ToLower(pe.Msg)
			if strings.Contains(msg, "ref_audio") || strings.Contains(msg, "reference audio") {
				probe.Note = "deployment rejected ref_audio: " + oneLine(pe.Msg, 160)
			} else {
				// A 400 about something else (voice, task) is inconclusive for
				// the reference capability specifically; say so rather than
				// pretending it was tested.
				probe.Note = "inconclusive (server 400 about a different field): " + oneLine(pe.Msg, 160)
			}
		} else {
			probe.Note = "probe request failed: " + oneLine(errString(err), 160)
		}
	}
	c.setReferenceProbe(probe)
	return probe
}

// ReferenceProbe returns the cached negotiation result. A zero-value result
// (Probed=false) means the deployment has not been asked yet.
func (c *Client) ReferenceProbe() ReferenceProbe {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refAudioProbe
}

func (c *Client) setReferenceProbe(p ReferenceProbe) {
	c.mu.Lock()
	c.refAudioProbe = p
	c.refAudioProbed = p.Probed
	c.mu.Unlock()
}

// dataURI renders the base64 data-URI form the checkpoints accept for
// ref_audio (openapi: "URL, base64, or file URI").
func dataURI(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

// syntheticToneWAV renders a 16-bit mono PCM WAV of a sine tone — the probe
// reference and a test fixture generator in one. Deterministic.
func syntheticToneWAV(sampleRate int, seconds float64, freqHz float64) []byte {
	n := int(seconds * float64(sampleRate))
	data := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(12000 * math.Sin(2*math.Pi*freqHz*float64(i)/float64(sampleRate)))
		data[2*i] = byte(s)
		data[2*i+1] = byte(s >> 8)
	}
	out := make([]byte, 0, 44+len(data))
	out = append(out, "RIFF"...)
	out = appendU32(out, 36+uint32(len(data)))
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	out = appendU32(out, 16)
	out = appendU16(out, 1) // PCM
	out = appendU16(out, 1) // mono
	out = appendU32(out, uint32(sampleRate))
	out = appendU32(out, uint32(sampleRate)*2)
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

// errString renders an error for report lines without its cause chain (the
// cause can carry a transport URL that must not reach a report line).
func errString(err error) string {
	if pe := asProvidersError(err); pe != nil {
		return string(pe.Kind) + ": " + pe.Msg
	}
	return "unclassified transport error"
}
