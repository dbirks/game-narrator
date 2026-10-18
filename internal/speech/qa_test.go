package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

func TestNormalizeText(t *testing.T) {
	cases := map[string]string{
		"Hello,  World!":       "hello world",
		"Let's go":             "lets go",
		"  THE   HARBOUR   ":   "the harbor",
		"Harbour and harbour":  "harbor and harbor",
		"colour/colourful":     "color colorful",
		"The colour was grey.": "the color was gray",
		"1996 (one-thousand)":  "1996 one thousand",
		"a b/c":                "a b c",
	}
	for in, want := range cases {
		if got := normalizeText(in); got != want {
			t.Errorf("normalizeText(%q) = %q, want %q", in, got, want)
		}
	}
	// The map is symmetric in EFFECT: whichever side carries the variant,
	// the two sides normalize equal.
	if normalizeText("the harbour") != normalizeText("the harbor") {
		t.Error("harbour/harbor must normalize equal")
	}
	if normalizeText("a colourful road") != normalizeText("a colorful road") {
		t.Error("colourful/colorful must normalize equal")
	}
}

func TestWERAndCER(t *testing.T) {
	if got := wer("the road was long", "the road was long"); got != 0 {
		t.Errorf("identical wer = %f, want 0", got)
	}
	// One substitution in a four-word line: 1/4.
	if got := wer("the road was long", "the road was short"); got != 0.25 {
		t.Errorf("substitution wer = %f, want 0.25", got)
	}
	// One deletion in a four-word line: 1/4.
	if got := wer("a b c d", "a b d"); got != 0.25 {
		t.Errorf("deletion wer = %f, want 0.25", got)
	}
	// One insertion against a three-word expectation: 1/3.
	if got := wer("a b c", "a x b c"); got != 1.0/3.0 {
		t.Errorf("insertion wer = %f, want 1/3", got)
	}
	if got := wer("", "anything"); got != 0 {
		t.Errorf("empty expected wer = %f, want 0", got)
	}
	if got := cer("abcd", "abxd"); got != 0.25 {
		t.Errorf("cer = %f, want 0.25", got)
	}
	if got := cer("", "xyz"); got != 0 {
		t.Errorf("empty expected cer = %f, want 0", got)
	}
}

func TestEvaluateBounds(t *testing.T) {
	t.Setenv(envWERBound, "0.2")
	rec := Evaluate("the road ahead was long", "the road ahead was long")
	if !rec.Pass || rec.WER != 0 || rec.CER != 0 {
		t.Fatalf("exact match = %+v, want pass with zero metrics", rec)
	}
	rec = Evaluate("the road ahead was long and the light held",
		"the road ahead was long and the water held")
	if rec.WER < 0.10 || rec.WER > 0.12 {
		t.Fatalf("one-word-in-nine wer = %f, want ~0.111", rec.WER)
	}
	if !rec.Pass {
		t.Fatalf("wer %f must pass the 0.2 bound", rec.WER)
	}
	rec = Evaluate("a b c d e f g h i j", "x x x x e f g h i j")
	if rec.Pass {
		t.Fatalf("wer %f must fail the 0.2 bound", rec.WER)
	}
}

// fakeASR is the scripted ASRClient for the engine tests.
type fakeASR struct {
	// scripts: one entry per call (Text or Err); the last repeats.
	scripts  []fakeASRCall
	calls    int
	language string
}

type fakeASRCall struct {
	Text string
	Err  error
}

func (f *fakeASR) TranscribeText(ctx context.Context, wav []byte, language string) (Transcription, error) {
	f.calls++
	f.language = language
	var c fakeASRCall
	if f.calls-1 < len(f.scripts) {
		c = f.scripts[f.calls-1]
	} else {
		c = f.scripts[len(f.scripts)-1]
	}
	if c.Err != nil {
		return Transcription{}, c.Err
	}
	return Transcription{
		Text: c.Text, Model: "fake-asr",
		RequestID: fmt.Sprintf("req-%d", f.calls), RequestIDSource: "client",
	}, nil
}

// committedEntry runs the cache's real commit path for key, so the engine
// tests start from exactly the state QA-on-insert sees: a freshly committed
// entry the engine may record a verdict on.
func committedEntry(t *testing.T, c *Cache, key CacheKey, audio tts.Audio) Result {
	t.Helper()
	res, err := c.GetOrGenerate(context.Background(), key, PriorityCurrent,
		func(context.Context) (tts.Audio, error) { return audio, nil })
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return res
}

// sameResult compares two Results field-by-field (Result holds []byte and so
// is not comparable).
func sameResult(a, b Result) bool {
	if a.SHA256 != b.SHA256 || a.Duration != b.Duration || a.SampleRate != b.SampleRate ||
		a.QA != b.QA || a.FromCache != b.FromCache || !bytes.Equal(a.WAV, b.WAV) {
		return false
	}
	return true
}

func TestQAEngineNotRunWhenASRAbsent(t *testing.T) {
	c, _, idx := newTestCache(t, 1)
	engine := NewQAEngine(c, nil)
	res := committedEntry(t, c, testKey(t, "none"), toneAudio())
	out, rec := engine.QAInsert(context.Background(), testKey(t, "none"), "the line", res, nil)
	if !sameResult(out, res) {
		t.Fatalf("result changed without QA: %+v", out)
	}
	if !rec.CheckedAt.IsZero() {
		t.Fatalf("record = %+v, want the not-run record", rec)
	}
	if e, ok := idx.Lookup(testKey(t, "none")); ok && e.QA != QAUnknown {
		t.Fatalf("entry QA = %s, want unknown", e.QA)
	}
}

func TestQAEnginePassMarksEntry(t *testing.T) {
	c, _, idx := newTestCache(t, 1)
	fake := &fakeASR{scripts: []fakeASRCall{{Text: "the line"}}}
	engine := NewQAEngine(c, fake)
	key := testKey(t, "pass")
	res := committedEntry(t, c, key, toneAudio())
	out, rec := engine.QAInsert(context.Background(), key, "the line", res, nil)
	if !rec.Pass || rec.Model != "fake-asr" || rec.RequestID == "" {
		t.Fatalf("record = %+v, want a pass with correlation", rec)
	}
	if !sameResult(out, res) {
		t.Fatalf("a pass must play the original bytes: %+v", out)
	}
	if fake.calls != 1 {
		t.Fatalf("asr calls = %d, want 1", fake.calls)
	}
	e, ok := idx.Lookup(key)
	if !ok || e.QA != QAPassed || e.QATranscript != "the line" || e.QAModel != "fake-asr" || e.QACheckedAt == "" {
		t.Fatalf("entry = %+v ok=%v, want the pass verdict with evidence", e, ok)
	}
}

func TestQAEngineHitIsNotRechecked(t *testing.T) {
	c, _, _ := newTestCache(t, 1)
	fake := &fakeASR{scripts: []fakeASRCall{{Text: "the line"}}}
	engine := NewQAEngine(c, fake)
	res := Result{WAV: []byte("x"), FromCache: true, SHA256: "a"}
	_, rec := engine.QAInsert(context.Background(), testKey(t, "hit"), "the line", res, nil)
	if fake.calls != 0 || !rec.CheckedAt.IsZero() {
		t.Fatalf("a cache hit must not spend an ASR call (calls=%d rec=%+v)", fake.calls, rec)
	}
}

func TestQAEngineRegeneratesOnceAndPasses(t *testing.T) {
	c, _, idx := newTestCache(t, 1)
	var gens int
	gen := func(ctx context.Context) (tts.Audio, error) {
		gens++
		return tts.Audio{WAV: MakeToneWAV(440+float64(gens), 0.2, 22050), Duration: 200 * time.Millisecond}, nil
	}
	// First attempt over the bound, regenerated audio passes.
	fake := &fakeASR{scripts: []fakeASRCall{{Text: "completely wrong"}, {Text: "the line"}}}
	engine := NewQAEngine(c, fake)
	key := testKey(t, "regen-pass")
	res, err := c.GetOrGenerate(context.Background(), key, PriorityCurrent, gen)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	out, rec := engine.QAInsert(context.Background(), key, "the line", res, gen)
	if gens != 2 {
		t.Fatalf("generations = %d, want exactly one regeneration", gens)
	}
	if !rec.Pass {
		t.Fatalf("final record = %+v, want the regenerated pass", rec)
	}
	if out.SHA256 == res.SHA256 {
		t.Fatal("the regenerated bytes must replace the flagged entry")
	}
	e, _ := idx.Lookup(key)
	if e.QA != QAPassed || e.QATranscript != "the line" || e.SHA256 != out.SHA256 {
		t.Fatalf("entry = %+v, want the pass verdict on the regenerated bytes", e)
	}
}

func TestQAEngineRegeneratesOnceAndFails(t *testing.T) {
	c, _, idx := newTestCache(t, 1)
	var gens int
	gen := func(ctx context.Context) (tts.Audio, error) {
		gens++
		return tts.Audio{WAV: MakeToneWAV(500+float64(gens), 0.2, 22050), Duration: 200 * time.Millisecond}, nil
	}
	fake := &fakeASR{scripts: []fakeASRCall{{Text: "nope"}, {Text: "still nope"}}}
	engine := NewQAEngine(c, fake)
	key := testKey(t, "regen-fail")
	res, err := c.GetOrGenerate(context.Background(), key, PriorityCurrent, gen)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	out, rec := engine.QAInsert(context.Background(), key, "the line", res, gen)
	if gens != 2 {
		t.Fatalf("generations = %d, want exactly one regeneration", gens)
	}
	if rec.Pass {
		t.Fatalf("record = %+v, want a fail after the one regeneration", rec)
	}
	if out.SHA256 == res.SHA256 {
		t.Fatal("the regenerated (flagged) bytes are the ones that play")
	}
	e, _ := idx.Lookup(key)
	if e.QA != QAFailed || e.QATranscript != "still nope" || e.QAWER <= 0 {
		t.Fatalf("entry = %+v, want the flagged verdict with WER and transcript", e)
	}
}

func TestQAEngineASRErrorNeverBlocks(t *testing.T) {
	c, _, idx := newTestCache(t, 1)
	// A retryable kind: two attempts (bounded), then not-run.
	retryable := &providers.Error{Kind: providers.KindUnavailable, Retryable: true, Msg: "down"}
	fake := &fakeASR{scripts: []fakeASRCall{{Err: retryable}}}
	engine := NewQAEngine(c, fake)
	key := testKey(t, "err-retry")
	res := committedEntry(t, c, key, toneAudio())
	out, rec := engine.QAInsert(context.Background(), key, "the line", res, nil)
	if fake.calls != 2 {
		t.Fatalf("retryable error: calls = %d, want the bounded 2", fake.calls)
	}
	if !rec.CheckedAt.IsZero() {
		t.Fatalf("record = %+v, want not-run (the line plays)", rec)
	}
	if !sameResult(out, res) {
		t.Fatalf("the original bytes play when QA cannot run: %+v", out)
	}
	if e, ok := idx.Lookup(key); ok && e.QA != QAUnknown {
		t.Fatalf("entry QA = %s, want unknown (QA not run)", e.QA)
	}

	// A non-retryable kind: one attempt, no retry.
	auth := &providers.Error{Kind: providers.KindAuth, Retryable: false, Msg: "bad token"}
	fake2 := &fakeASR{scripts: []fakeASRCall{{Err: auth}}}
	engine2 := NewQAEngine(c, fake2)
	key2 := testKey(t, "err-auth")
	res2 := committedEntry(t, c, key2, toneAudio())
	if _, rec := engine2.QAInsert(context.Background(), key2, "the line", res2, nil); fake2.calls != 1 || !rec.CheckedAt.IsZero() {
		t.Fatalf("non-retryable: calls=%d rec=%+v, want one attempt and not-run", fake2.calls, rec)
	}
}

func TestQAEngineRegenerationErrorKeepsFirstVerdict(t *testing.T) {
	c, _, idx := newTestCache(t, 1)
	gen := func(ctx context.Context) (tts.Audio, error) {
		return tts.Audio{}, errors.New("tts down")
	}
	fake := &fakeASR{scripts: []fakeASRCall{{Text: "wrong"}}}
	engine := NewQAEngine(c, fake)
	key := testKey(t, "regen-err")
	res, err := c.GetOrGenerate(context.Background(), key, PriorityCurrent,
		func(context.Context) (tts.Audio, error) { return toneAudio(), nil })
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	out, rec := engine.QAInsert(context.Background(), key, "the line", res, gen)
	if rec.Pass || rec.Transcript != "wrong" {
		t.Fatalf("record = %+v, want the first (failing) verdict", rec)
	}
	if !sameResult(out, res) {
		t.Fatal("the original bytes play when regeneration fails")
	}
	e, _ := idx.Lookup(key)
	if e.QA != QAFailed {
		t.Fatalf("entry QA = %s, want the flagged first verdict", e.QA)
	}
}
