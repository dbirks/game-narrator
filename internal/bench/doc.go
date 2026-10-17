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

// Report is one benchmark run's machine-readable result (report.json).
type Report struct {
	SchemaVersion int         `json:"schema_version"`
	RunID         string      `json:"run_id"`
	Set           string      `json:"set"`
	Split         string      `json:"split"` // tuning | holdout | all
	Origin        string      `json:"origin"`
	Samples       int         `json:"samples"`
	Metrics       []Metric    `json:"metrics"`
	Diagnostics   Diagnostics `json:"diagnostics"`
	GeneratedAt   string      `json:"generated_at"`
}

// Metric is one report row: value, sample count, kind, the provisional
// objective it is compared against, and the verdict. N==0 means the metric
// had no samples and Value is 0 with Met "n/a" — never a 0 or 100% reading.
type Metric struct {
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	N         int     `json:"n"`
	Kind      string  `json:"kind"` // ratio | percent | ms | count | seconds
	Objective string  `json:"objective"`
	Met       string  `json:"met"` // met | not_met | n/a | informational
	Detail    string  `json:"detail,omitempty"`
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
}
