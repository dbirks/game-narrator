package bench

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// --- split determinism -------------------------------------------------------

// The split is a pure function of the sample id: the same id always lands on
// the same side, and no run can move a labelled sample between tuning and
// holdout (docs/spec/12 holdout discipline).
func TestSplitForIDDeterministic(t *testing.T) {
	for i := 1; i <= 200; i++ {
		id := fmt.Sprintf("bn%04d", i)
		a := SplitForID(id)
		b := SplitForID(id)
		if a != b {
			t.Fatalf("SplitForID(%s) moved: %s then %s", id, a, b)
		}
		if a != SplitTuning && a != SplitHoldout {
			t.Fatalf("SplitForID(%s) = %q, want tuning or holdout", id, a)
		}
	}
	// The generated matrix must actually populate both sides.
	holdout, tuning := 0, 0
	for i := 1; i <= 144; i++ {
		switch SplitForID(fmt.Sprintf("bn%04d", i)) {
		case SplitHoldout:
			holdout++
		case SplitTuning:
			tuning++
		}
	}
	if holdout == 0 || tuning == 0 {
		t.Fatalf("split is degenerate: holdout=%d tuning=%d", holdout, tuning)
	}
	if holdout*10 < 144/2 || holdout*2 > 144 { // loosely 30% ± a third
		t.Fatalf("holdout share %d/144 is far from the intended 30%%", holdout)
	}
}

// --- loader validation --------------------------------------------------------

// validSet is a minimal set that passes Validate; the cases mutate it.
func validSet() *TruthSet {
	msg := "m1"
	return &TruthSet{
		SchemaVersion: SchemaVersion,
		Set:           "t",
		Origin:        "generated_synthetic",
		SeedBase:      7,
		ProfileFile:   "profile.json",
		Samples: []Sample{
			{
				ID: "bn0001", Frames: []Frame{{File: "a.png", TsNS: 100}},
				SourceText: "hello world", MessageID: &msg, Speaker: "keeper",
				TextKind: "dialogue", Scene: SceneID, IntendedSpeech: true,
				Split: SplitForID("bn0001"),
			},
		},
	}
}

func TestTruthValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(ts *TruthSet)
		wantErr string
	}{
		{"valid", func(ts *TruthSet) {}, ""},
		{"bad schema", func(ts *TruthSet) { ts.SchemaVersion = 99 }, "schema_version"},
		{"no set", func(ts *TruthSet) { ts.Set = "" }, "set is required"},
		{"not synthetic", func(ts *TruthSet) { ts.Origin = "captured" }, "generated_synthetic"},
		{"zero seed", func(ts *TruthSet) { ts.SeedBase = 0 }, "seed_base"},
		{"no profile", func(ts *TruthSet) { ts.ProfileFile = "" }, "profile_file"},
		{"no samples", func(ts *TruthSet) { ts.Samples = nil }, "no samples"},
		{"dup id", func(ts *TruthSet) { ts.Samples = append(ts.Samples, ts.Samples[0]) }, "duplicate id"},
		{"bad id shape", func(ts *TruthSet) { ts.Samples[0].ID = "xx0001" }, "bn"},
		{"no frames", func(ts *TruthSet) { ts.Samples[0].Frames = nil }, "frames"},
		{"non-monotonic", func(ts *TruthSet) {
			ts.Samples[0].Frames = []Frame{{File: "a.png", TsNS: 300}, {File: "b.png", TsNS: 200}}
		}, "monotonic"},
		{"zero ts", func(ts *TruthSet) { ts.Samples[0].Frames = []Frame{{File: "a.png", TsNS: 0}} }, "ts_ns"},
		{"bad split value", func(ts *TruthSet) { ts.Samples[0].Split = "train" }, "split"},
		{"split mismatch", func(ts *TruthSet) { ts.Samples[0].Split = flipSplit(ts.Samples[0].Split) }, "hash"},
		{"bad text kind", func(ts *TruthSet) { ts.Samples[0].TextKind = "narration" }, "text_kind"},
		{"bad scene", func(ts *TruthSet) { ts.Samples[0].Scene = "elsewhere" }, "scene"},
		{"empty speaker", func(ts *TruthSet) { ts.Samples[0].Speaker = "" }, "speaker"},
		{"dialogue without id", func(ts *TruthSet) { ts.Samples[0].MessageID = nil }, "message_id"},
		{"dialogue without text", func(ts *TruthSet) { ts.Samples[0].SourceText = "" }, "source_text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := validSet()
			tc.mutate(ts)
			err := ts.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want ok", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
	// Empty-box samples: no text, no id, never intended.
	empty := validSet()
	empty.Samples[0] = Sample{
		ID: "bn0002", Frames: []Frame{{File: "a.png", TsNS: 100}},
		Speaker: SpeakerUnknown, TextKind: "empty", Scene: SceneID,
		Split: SplitForID("bn0002"),
	}
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty sample: %v", err)
	}
	empty.Samples[0].SourceText = "text"
	if err := empty.Validate(); err == nil || !strings.Contains(err.Error(), "no source_text") {
		t.Fatalf("empty sample with text: %v", err)
	}
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func flipSplit(s string) string {
	if s == SplitTuning {
		return SplitHoldout
	}
	return SplitTuning
}

// --- metrics on hand-made inputs ----------------------------------------------

func TestMetricsEmptyIsNA(t *testing.T) {
	m := RunMetrics(nil)
	if len(m) == 0 {
		t.Fatal("no metrics")
	}
	for _, row := range m {
		if row.N != 0 {
			t.Errorf("%s: n = %d, want 0 on empty input", row.Name, row.N)
		}
		if row.Met != "n/a" {
			t.Errorf("%s: met = %q, want n/a on empty input", row.Name, row.Met)
		}
		if row.Value != 0 {
			t.Errorf("%s: value = %v, want 0 on empty input", row.Name, row.Value)
		}
	}
}

// oneDialogueSample is one grading surface: an in-script sample with a known
// speaker and an intended narration.
func oneDialogueSample(commitMsgID, ocrText, topID string, topProb float64) sampleOutcomes {
	o := sampleOutcomes{sample: truthSample{
		id: "bn0001", text: "abcd", msgID: "m1", speaker: "keeper",
		intended: true, textKind: "dialogue", firstTextNS: 300_000_000,
	}}
	if commitMsgID != "" {
		o.commitMsgIDs = []string{commitMsgID}
		o.commitRaw = []string{ocrText}
		o.commitTSNS = []int64{600_000_000}
	}
	o.ocrText = ocrText
	o.topID, o.topProb = topID, topProb
	o.speakerID = "keeper"
	return o
}

func findMetric(t *testing.T, m []Metric, name string) Metric {
	t.Helper()
	for _, row := range m {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("metric %s missing", name)
	return Metric{}
}

func TestMetricsRecallPrecision(t *testing.T) {
	out := []sampleOutcomes{
		oneDialogueSample("m1", "abcd", "m1", 0.9),                                                              // correct commit
		oneDialogueSample("m2", "abcd", "m2", 0.9),                                                              // wrong commit
		{sample: truthSample{id: "bn0003", text: "efgh", msgID: "m3", speaker: "keeper", textKind: "dialogue"}}, // no commit
		{sample: truthSample{id: "bn0004", textKind: "empty", speaker: SpeakerUnknown}},                         // empty box, no commit
	}
	m := RunMetrics(out)
	if got := findMetric(t, m, "recognition_recall"); got.Value != 1/3.0 || got.N != 3 {
		t.Errorf("recall = %v n=%d, want 1/3 n=3", got.Value, got.N)
	}
	if got := findMetric(t, m, "recognition_precision"); got.Value != 0.5 || got.N != 2 {
		t.Errorf("precision = %v n=%d, want 0.5 n=2", got.Value, got.N)
	}
}

func TestMetricsCERWER(t *testing.T) {
	// CER over "abcd" vs "abxd": one substitution in four characters.
	// WER: one word, one error -> 1.0.
	out := []sampleOutcomes{oneDialogueSample("m1", "abxd", "m1", 0.9)}
	m := RunMetrics(out)
	if got := findMetric(t, m, "ocr_cer"); got.Value != 0.25 || got.N != 1 {
		t.Errorf("cer = %v n=%d, want 0.25 n=1", got.Value, got.N)
	}
	if got := findMetric(t, m, "ocr_wer"); got.Value != 1.0 {
		t.Errorf("wer = %v, want 1.0", got.Value)
	}
	// A sample with no reading at all is a full miss, not a zero.
	out = []sampleOutcomes{{sample: truthSample{id: "bn0005", text: "abcd", msgID: "m1", speaker: "keeper", textKind: "dialogue"}}}
	if got := findMetric(t, RunMetrics(out), "ocr_cer"); got.Value != 1.0 {
		t.Errorf("cer on empty reading = %v, want 1.0", got.Value)
	}
}

func TestMetricsCalibration(t *testing.T) {
	// Brier: (0.2-1)^2 + (0.8-0)^2 over 2 = 0.64.
	probs := []float64{0.2, 0.8}
	labels := []bool{true, false}
	if got := naBrier("b", probs, labels, "").Value; abs64(got-0.64) > 1e-9 {
		t.Errorf("brier = %v, want 0.64", got)
	}
	// ECE: bin0 has p 0.2 y 1 (err 0.8, weight 0.5); bin8 has p 0.8 y 1 (err 0.2, weight 0.5) -> 0.5.
	probs = []float64{0.2, 0.8}
	labels = []bool{true, true}
	if got := naECE("e", probs, labels, "").Value; abs64(got-0.5) > 1e-9 {
		t.Errorf("ece = %v, want 0.5", got)
	}
	// Perfectly calibrated: p 0.5 right, p 0.5 wrong -> 0.
	if got := naECE("e", []float64{0.5, 0.5}, []bool{true, false}, "").Value; abs64(got) > 1e-9 {
		t.Errorf("ece = %v, want 0", got)
	}
	if naECE("e", nil, nil, "").Met != "n/a" || naBrier("b", nil, nil, "").Met != "n/a" {
		t.Error("empty calibration must be n/a")
	}
}

func TestMetricsCalibrationFitRow(t *testing.T) {
	// The synthetic set's shape: every ranked sample's top-1 is correct, so
	// the run's data cannot calibrate a temperature — the row must say
	// "not fitted" with the guard's requirement, not a 0.0% ECE.
	out := make([]sampleOutcomes, 0, 60)
	for i := 0; i < 60; i++ {
		o := oneDialogueSample("m1", "abcd", "m1", 0.9)
		o.sample.id = fmt.Sprintf("bn%04d", i+1)
		out = append(out, o)
	}
	row := findMetric(t, RunMetrics(out), "calibration")
	if row.N != 60 {
		t.Errorf("calibration n = %d, want 60 (every ranked in-script sample)", row.N)
	}
	if row.Met != "not_fitted" {
		t.Errorf("calibration met = %q, want not_fitted (0 incorrect top-1, 60 samples)", row.Met)
	}
	if !strings.Contains(row.Detail, "not fitted") || !strings.Contains(row.Detail, "requires ≥10") {
		t.Errorf("calibration detail %q does not state the refusal with the ≥10 requirement", row.Detail)
	}
	// A wrong top-1 is the fit's raw material: ten of them in ≥50 samples
	// makes the data fittable (the row then says so, and stays informational
	// — the run still uses the default T).
	for i := 0; i < 10; i++ {
		out[i].topID = "m2" // wrong top-1, still ranked
	}
	row = findMetric(t, RunMetrics(out), "calibration")
	if row.Met != "informational" {
		t.Errorf("calibration met = %q, want informational (10 incorrect top-1 of 60 passes the guard)", row.Met)
	}
	if !strings.Contains(row.Detail, "10 of 60") {
		t.Errorf("calibration detail %q does not carry the incorrect-top-1 count", row.Detail)
	}
	// Empty input: the row is n/a like every other row.
	empty := findMetric(t, RunMetrics(nil), "calibration")
	if empty.N != 0 || empty.Met != "n/a" || empty.Value != 0 {
		t.Errorf("empty calibration row = %+v, want n/a with n=0 value 0", empty)
	}
}

func TestMetricsPercentile(t *testing.T) {
	vals := []float64{10, 20, 30, 40, 50}
	if got := percentileMetric("p50", vals, 0.50, "ms", "", "").Value; got != 30 {
		t.Errorf("p50 = %v, want 30", got)
	}
	if got := percentileMetric("p95", vals, 0.95, "ms", "", "").Value; got != 50 {
		t.Errorf("p95 = %v, want 50", got)
	}
	if got := percentileMetric("p95", nil, 0.95, "ms", "", ""); got.Met != "n/a" {
		t.Errorf("empty percentile met = %q, want n/a", got.Met)
	}
}

func TestMetricsVerdict(t *testing.T) {
	const obj = "≥95% recall (provisional)"
	if verdict(0.96, obj, 10, false) != "met" {
		t.Error("0.96 vs ≥95% want met")
	}
	if verdict(0.94, obj, 10, false) != "not_met" {
		t.Error("0.94 vs ≥95% want not_met")
	}
	if verdict(0.96, obj, 0, false) != "n/a" {
		t.Error("n=0 want n/a")
	}
	if verdict(0, objNoDup, 3, true) != "met" {
		t.Error("count 0 vs count objective want met")
	}
	if verdict(1, objNoDup, 3, true) != "not_met" {
		t.Error("count 1 vs no-dup want not_met")
	}
	if verdict(0.5, "", 5, false) != "informational" {
		t.Error("no objective want informational")
	}
}

func TestMetricsSpeech(t *testing.T) {
	// One sample: a correct commit that speaks, plus a duplicate start on the
	// same occurrence and one older-generation start.
	o := oneDialogueSample("m1", "abcd", "m1", 0.9)
	o.speechStarted = []speechEvent{
		{occurrenceID: "oc1", messageID: "m1", generation: 1},
		{occurrenceID: "oc1", messageID: "m1", generation: 1}, // duplicate
		{occurrenceID: "oc1", messageID: "m1", generation: 0}, // stale: older than gen 1
	}
	m := RunMetrics([]sampleOutcomes{o})
	if got := findMetric(t, m, "positive_speech_precision"); got.Value != 1.0 || got.N != 3 {
		t.Errorf("speech precision = %v n=%d, want 1.0 n=3", got.Value, got.N)
	}
	if got := findMetric(t, m, "duplicate_speech"); got.Value != 2 || got.N != 1 {
		t.Errorf("duplicate = %v n=%d, want 2 n=1", got.Value, got.N)
	}
	if got := findMetric(t, m, "stale_speech_after_advance"); got.Value != 1 || got.N != 3 {
		t.Errorf("stale = %v n=%d, want 1 n=3", got.Value, got.N)
	}
	if got := findMetric(t, m, "missed_narrations"); got.Value != 0 || got.N != 1 {
		t.Errorf("missed = %v n=%d, want 0 n=1", got.Value, got.N)
	}
	if got := findMetric(t, m, "named_speaker_accuracy"); got.Value != 1.0 || got.N != 1 {
		t.Errorf("speaker = %v n=%d, want 1.0 n=1", got.Value, got.N)
	}
	// A missed narration: intended, but the pipeline committed nothing at all
	// (a wrong commit is a recall/precision failure, not a miss).
	miss := oneDialogueSample("", "abcd", "", 0.0)
	if got := findMetric(t, RunMetrics([]sampleOutcomes{miss}), "missed_narrations"); got.Value != 1 || got.N != 1 {
		t.Errorf("missed = %v n=%d, want 1 n=1", got.Value, got.N)
	}
}

// --- committed set round-trip ---------------------------------------------------

func TestCommittedSetLoads(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "bench", "bench-synthetic-001")
	ts, err := LoadTruth(dir)
	if err != nil {
		t.Fatalf("LoadTruth: %v", err)
	}
	if len(ts.Samples) != 144 {
		t.Fatalf("samples = %d, want 144 (regenerate with bench --generate)", len(ts.Samples))
	}
	prof, err := LoadScriptProfile(dir)
	if err != nil {
		t.Fatalf("LoadScriptProfile: %v", err)
	}
	if len(prof.Messages) != 14 {
		t.Fatalf("messages = %d, want 14", len(prof.Messages))
	}
	idx, err := prof.BuildIndex()
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if idx.MessageCount() != 14 {
		t.Fatalf("index messages = %d, want 14", idx.MessageCount())
	}
	// Every sample's message id must resolve in the profile.
	for _, s := range ts.Samples {
		if id := s.MessageInScript(); id != "" && !prof.InScript(id) {
			t.Fatalf("sample %s names unknown message %q", s.ID, id)
		}
	}
}

func TestGenerateSamplesDeterministic(t *testing.T) {
	a, _, err := GenerateSamples(filepath.Join("..", "..", "testdata", "synthetic", "profiles", "dialogue-advance-001.json"), 20260709)
	if err != nil {
		t.Fatalf("GenerateSamples: %v", err)
	}
	b, _, err := GenerateSamples(filepath.Join("..", "..", "testdata", "synthetic", "profiles", "dialogue-advance-001.json"), 20260709)
	if err != nil {
		t.Fatalf("GenerateSamples (2): %v", err)
	}
	if len(a.Samples) != 144 || len(b.Samples) != 144 {
		t.Fatalf("samples = %d/%d, want 144/144", len(a.Samples), len(b.Samples))
	}
	for i := range a.Samples {
		if a.Samples[i].ID != b.Samples[i].ID || a.Samples[i].Split != b.Samples[i].Split ||
			a.Samples[i].MessageInScript() != b.Samples[i].MessageInScript() {
			t.Fatalf("sample %d differs between generations", i)
		}
	}
	if a.SeedBase != b.SeedBase {
		t.Fatal("seed base moved")
	}
}
