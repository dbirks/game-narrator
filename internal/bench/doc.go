// Package bench implements docs/spec/12 "Benchmarks and ground truth" for the
// frame-replay milestone: labelled synthetic ground truth, a deterministic
// frame generator, the real pipeline (ModeReplay + Tesseract) per sample, and
// the provisional-objective metrics report.json / report.md.
//
// Synthetic-only by construction (ADR-0038): every frame, line and label is
// generated from the synthetic profile; real Wind Waker coverage arrives with
// the Dolphin fixture factory and reuses these formats and metrics. The
// provisional objectives in docs/spec/12 are reported as met/not met/n-a per
// run — never as achieved.
package bench

// SchemaVersion is the only report/truth schema this package understands.
const SchemaVersion = 1

// The AGENTS.md rule-3 provenance classes a bench run can be in. They are
// reported, never pooled: a synthetic-stub number and a captured-frame
// number are different measurements of different systems.
const (
	ClassSynthetic = "a" // class (a): synthetic stub
	ClassCaptured  = "b" // class (b): captured-frame replay
)

// Report is one benchmark run's machine-readable result (report.json).
type Report struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	Set           string `json:"set"`
	Split         string `json:"split"` // tuning | holdout | all
	Origin        string `json:"origin"`
	// Class is the AGENTS.md rule-3 provenance class of the run's frames:
	// "a" synthetic stub, "b" captured-frame replay (docs/spec/11 "Fixture
	// origins"). The two holdouts are reported separately and never pooled.
	Class       string      `json:"class"`
	Samples     int         `json:"samples"`
	Metrics     []Metric    `json:"metrics"`
	Diagnostics Diagnostics `json:"diagnostics"`
	GeneratedAt string      `json:"generated_at"`
}

// Metric is one report row: value, sample count, kind, the provisional
// objective it is compared against, and the verdict. N==0 means the metric
// had no samples and Value is 0 with Met "n/a" — never a 0 or 100% reading.
// ResolutionLimited marks a value quantized to a coarser grid than the unit
// suggests (the commit latency is on the capture frame grid): Bound is the
// upper bound on the true value and Met reads "resolution-limited". Kind
// "info" rows carry no numeric value (Value stays 0; Detail is the
// statement): the calibration row reports the temperature fit's status —
// Met "not_fitted" when this run's data cannot calibrate a temperature (the
// ECE/Brier rows are the default T = 1 numbers), "informational" when it
// could.
type Metric struct {
	Name              string  `json:"name"`
	Value             float64 `json:"value"`
	N                 int     `json:"n"`
	Kind              string  `json:"kind"` // ratio | percent | ms | count | seconds | info
	Objective         string  `json:"objective"`
	Met               string  `json:"met"` // met | not_met | n/a | informational | resolution-limited | not_fitted
	Detail            string  `json:"detail,omitempty"`
	ResolutionLimited bool    `json:"resolution_limited,omitempty"`
	Bound             float64 `json:"bound,omitempty"` // upper bound on the true value, Value's units (ResolutionLimited rows)
}

// Diagnostics carries the run's environment, so a number can be traced to the
// machine and engine that produced it.
type Diagnostics struct {
	GoVersion    string   `json:"go_version"`
	Tesseract    string   `json:"tesseract"`
	OS           string   `json:"os"`
	SeedBase     uint64   `json:"seed_base"`
	SetHash      string   `json:"set_hash"` // sha256 of the set's truth.json
	SampleIDs    []string `json:"sample_ids"`
	CPUTimeSecs  float64  `json:"cpu_time_secs"`
	PeakRSSMB    float64  `json:"peak_rss_mb"`
	DurationSecs float64  `json:"duration_secs"`
	// MatcherCalibration identifies the matcher settings the probabilities in
	// this run were produced under (temperature + calibration revision, ADR
	// 0035 "Calibration"): the identity any fixture or cache key that embeds
	// matcher settings must carry.
	MatcherCalibration string `json:"matcher_calibration,omitempty"`
	// NotRun lists the samples the harness failed to complete (sample id +
	// error, no game text). They are excluded from every metric's denominator,
	// and this list is the record that the set was not run in full — a holdout
	// that lost samples must never read as full coverage (a silently shrunken
	// denominator would grade absence as success).
	NotRun []NotRunSample `json:"not_run,omitempty"`
	// Top1Misses lists the ranked in-script samples whose top-1 candidate was
	// not the truth message (sample id + message ids, no game text): the
	// per-sample evidence behind the top-1 row.
	Top1Misses []Top1Miss `json:"top1_misses,omitempty"`
}

// NotRunSample is one bench sample the harness could not complete.
type NotRunSample struct {
	SampleID string `json:"sample_id"`
	Reason   string `json:"reason"`
}

// Top1Miss is one (sample, reading) whose top-1 was not the truth message.
// Reading names the graded reading the row was taken on (ADR-0058 amendment
// 3): "window" — the display-window reading top1_exact_accuracy grades — or
// "last_row" — the legacy last-ranking-row-of-the-span reading, kept visible
// so a trailing in-script message stays on the record. Message ids only:
// reports carry no game text (AGENTS.md rule 5).
const (
	Top1ReadingWindow  = "window"
	Top1ReadingLastRow = "last_row"
)

type Top1Miss struct {
	SampleID  string  `json:"sample_id"`
	MessageID string  `json:"message_id"` // the truth message
	Reading   string  `json:"reading"`    // window | last_row
	TopID     string  `json:"top_id"`     // the candidate the reading ranked first
	TopProb   float64 `json:"top_prob"`
}
