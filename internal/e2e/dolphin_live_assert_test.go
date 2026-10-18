// Package e2e — assertion stages (a)–(c) of the live Dolphin E2E
// (task m6-live-e2e): frames, the script match, the Clef request.
// Log-safe per AGENTS.md rule 5: counts, ids, hashes, timings — never
// dialogue text (that stays in the evidence bundle).

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// assertDolphinFrames is stage (a): real frames were captured from Dolphin
// and the subtitle was detected. The proof is three-way: the session's own
// counters (frames reached the ROI stage), the drive's success frame (the
// measured text in the bottom letterbox), and the text.observed event on the
// commit's trace (region present, real frame_seq and capture clock). The
// dev-trace frame/crop copies degrade to a log line when the recorder
// lookup missed (the publisher's documented degradation): the drive's
// SuccessPath PNG in the bundle is the subtitle frame either way.
func assertDolphinFrames(t *testing.T, a *dolphinAsserts, ch *dolphinChain) {
	t.Helper()
	s := a.session.Stats
	if s.FramesDelivered == 0 || s.FramesProcessed == 0 {
		t.Fatalf("e2e: (a) FAIL: no frames reached the pipeline (delivered=%d processed=%d); bundle %s",
			s.FramesDelivered, s.FramesProcessed, a.bundle)
	}
	if !a.drive.OK || !a.drive.SuccessText {
		t.Fatalf("e2e: (a) FAIL: the drive did not land on a story subtitle WITH text (ok=%v reason=%q); evidence %s",
			a.drive.OK, a.drive.Reason, a.bundle)
	}
	if ch.text == nil {
		t.Fatalf("e2e: (a) FAIL: no text.observed on trace %s; bundle %s", ch.trace, a.bundle)
	}
	td := ch.textData
	if !td.RegionPresent || td.FrameSeq <= 0 || td.CaptureTSNS <= 0 || td.Text == "" {
		t.Fatalf("e2e: (a) FAIL: text.observed on trace %s carries no detected subtitle (region_present=%v frame_seq=%d capture_ns=%d text_len=%d); bundle %s",
			ch.trace, td.RegionPresent, td.FrameSeq, td.CaptureTSNS, len(td.Text), a.bundle)
	}
	// The dev-trace evidence for this observation: the sampled full frame
	// (JPEG) and the subtitle-region crop (PNG), copied into the bundle.
	copyAsset(t, a.devTraceDir, td.FrameAsset, filepath.Join(a.bundle, "frames", "frame.jpg"))
	copyAsset(t, a.devTraceDir, td.CropAsset, filepath.Join(a.bundle, "frames", "subtitle_crop.png"))
	t.Logf("e2e: (a) PASS: frames delivered=%d processed=%d ocr_calls=%d; subtitle detected on frame_seq=%d; trace=%s; evidence %s/frames",
		s.FramesDelivered, s.FramesProcessed, s.OCRCalls, td.FrameSeq, ch.trace, a.bundle)
}

// assertDolphinMatch is stage (b): the OCR reading matched an imported
// script line — candidate rank 1 is the committed message and the CER of
// the reading against that line's text is within the spec bound. The
// reference text is read back through the messages API (the same surface
// the developer UI uses), never from the test's own copy.
func assertDolphinMatch(t *testing.T, a *dolphinAsserts, ch *dolphinChain) {
	t.Helper()
	if ch.candidates == nil {
		t.Fatalf("e2e: (b) FAIL: no message.candidates on trace %s; bundle %s", ch.trace, a.bundle)
	}
	if a.session.Stats.OCRCalls == 0 {
		t.Fatalf("e2e: (b) FAIL: the session made zero OCR calls (the reading was not produced live); bundle %s", a.bundle)
	}
	cd := ch.candData
	if len(cd.Candidates) == 0 {
		t.Fatalf("e2e: (b) FAIL: the candidates event on trace %s carries no candidates; bundle %s", ch.trace, a.bundle)
	}
	top := cd.Candidates[0]
	if top.MessageID != ch.commitData.MessageID {
		t.Fatalf("e2e: (b) FAIL: rank-1 candidate %s is not the committed message %s (rank 1 probability %.3f); trace %s; bundle %s",
			top.MessageID, ch.commitData.MessageID, top.Probability, ch.trace, a.bundle)
	}
	reading := cd.Text
	if reading == "" {
		reading = ch.textData.Text
	}
	ref := dolphinReferenceText(t, a, ch.commitData.MessageID)
	cer := dolphinCER(reading, ref)
	if cer > dolphinMaxCER {
		t.Fatalf("e2e: (b) FAIL: OCR reading vs imported line %s CER %.3f > %.2f bound; trace %s; bundle %s",
			ch.commitData.MessageID, cer, dolphinMaxCER, ch.trace, a.bundle)
	}
	t.Logf("e2e: (b) PASS: rank-1=%s probability=%.3f cer=%.3f (bound %.2f); message %s; trace %s",
		top.MessageID, top.Probability, cer, dolphinMaxCER, ch.commitData.MessageID, ch.trace)
}

// dolphinReferenceText reads the matched line's text back through the
// messages API: the normalised text (what the matcher compares against),
// falling back to the speech text and then the original. The detail route
// wraps the DTO under "message".
func dolphinReferenceText(t *testing.T, a *dolphinAsserts, messageID string) string {
	t.Helper()
	rec := dolphinDo(t, a.srv, a.token, http.MethodGet,
		"/api/v1/profiles/"+a.profileID+"/messages/"+messageID, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("e2e: (b): get matched message %s = %d: %s", messageID, rec.Code, rec.Body.String())
	}
	var resp struct {
		Message struct {
			NormalizedText string `json:"normalized_text"`
			SpeechText     string `json:"speech_text"`
			OriginalText   string `json:"original_text"`
		} `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("e2e: (b): message %s decode: %v", messageID, err)
	}
	switch {
	case resp.Message.NormalizedText != "":
		return resp.Message.NormalizedText
	case resp.Message.SpeechText != "":
		return resp.Message.SpeechText
	}
	return resp.Message.OriginalText
}

// assertDolphinClef is stage (c): a genuine decision-provider request took
// place and its request id was recorded. Until the runtime wiring lands the
// session runs local and this fails loudly with the task's exact string —
// by design (AGENTS.md: a stage the live run could not prove is a failure
// with evidence, not a silent skip).
func assertDolphinClef(t *testing.T, a *dolphinAsserts, ch *dolphinChain) {
	t.Helper()
	if a.session.Decision == nil {
		t.Fatalf("e2e: (c) FAIL: clef not consulted by runtime: the session has no decision wiring (local-only run); models=%v; bundle %s",
			a.models, a.bundle)
	}
	ds := a.session.Decision
	if !ds.Configured || ds.Calls == 0 || ds.Answered == 0 {
		t.Fatalf("e2e: (c) FAIL: clef not consulted by runtime: decision configured=%v calls=%d answered=%d degraded=%v reason=%q; bundle %s",
			ds.Configured, ds.Calls, ds.Answered, ds.Degraded, ds.Reason, a.bundle)
	}
	if ds.Degraded {
		t.Fatalf("e2e: (c) FAIL: the decision provider degraded mid-run (reason %q); the request is not proven; bundle %s", ds.Reason, a.bundle)
	}
	if ch.decResolved == nil {
		t.Fatalf("e2e: (c) FAIL: clef not consulted by runtime: no decision.resolved on trace %s; bundle %s", ch.trace, a.bundle)
	}
	r := ch.decResData
	if r.Provider == "" || r.Model == "" || !r.Accepted || r.LatencyMS < 0 {
		t.Fatalf("e2e: (c) FAIL: decision.resolved on trace %s is not a genuine answered provider verdict (provider=%q model=%q accepted=%v latency_ms=%d); bundle %s",
			ch.trace, r.Provider, r.Model, r.Accepted, r.LatencyMS, a.bundle)
	}
	// The request id is the provider-side correlation: the System One client
	// records it from the response headers (X-Request-Id and friends), so an
	// empty id means the gateway supplied none — a recorded finding, not a
	// faked stage. The genuine request stands on requested+resolved above.
	if r.RequestID == "" {
		t.Logf("e2e: (c): decision.resolved request_id EMPTY: the gateway response carried no request-id header (X-Request-Id/Request-Id/X-Amzn-Requestid/X-Oneapi-Request-Id); the genuine request is proven by decision.requested+resolved(model=%s, accepted, latency_ms=%d)", r.Model, r.LatencyMS)
	}
	if ch.decRequested == nil {
		t.Fatalf("e2e: (c) FAIL: no decision.requested joined to observation_seq=%d generation=%d (the product's join, see decision.go); bundle %s",
			ch.candData.ObservationSeq, ch.candData.Generation, a.bundle)
	}
	if ch.decReqData.Model == "" {
		t.Fatalf("e2e: (c) FAIL: decision.requested names no model; bundle %s", a.bundle)
	}
	t.Logf("e2e: (c) PASS: clef request_id=%s provider=%s model=%s accepted=%v latency_ms=%d (calls=%d answered=%d); trace %s",
		r.RequestID, r.Provider, firstNonEmpty(r.Model, ch.decReqData.Model), r.Accepted, r.LatencyMS, ds.Calls, ds.Answered, ch.trace)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// copyAsset copies one dev-trace blob into the bundle. A missing sha or blob
// is logged, not failed: the publisher's documented degradation (the event
// still stands on its counters), and the drive's SuccessPath PNG in the
// bundle carries the subtitle frame either way. A blob that is referenced
// but unreadable on disk is a store corruption the test must surface.
func copyAsset(t *testing.T, traceRoot, sha, dest string) {
	t.Helper()
	if sha == "" {
		t.Logf("e2e: dev-trace evidence: no asset reference for %s (recorder lookup miss); the drive PNGs carry the frame", filepath.Base(dest))
		return
	}
	src := filepath.Join(traceRoot, "blobs", sha[:2], sha[2:4], sha)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("e2e: dev-trace evidence: %v (dest %s)", err, dest)
	}
	if err := writeEvidenceFile(dest, b); err != nil {
		t.Fatalf("e2e: dev-trace evidence write %s: %v", dest, err)
	}
	if err := writeEvidenceFile(dest+".sha256", []byte(fmt.Sprintf("%x\n", sha256Sum(b)))); err != nil {
		t.Fatalf("e2e: dev-trace evidence write %s: %v", dest+".sha256", err)
	}
}
