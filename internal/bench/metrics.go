package bench

import (
	"fmt"
	"sort"

	"github.com/dbirks/game-narrator/internal/vision/metrics"
)

// The provisional objectives are docs/spec/12's, quoted for the report. They
// are deliberately provisional: the spec requires them to be calibrated on
// actual content and revised via ADR with benchmark evidence, never presented
// as achieved. met/not_met/n-a is the only verdict vocabulary a run may use.
const (
	objRecall     = "≥95% recall on intentionally engaged conventional dialogue boxes (provisional)"
	objTop1       = "≥95% top-1 exact canonical message selection on held-out named dialogue (provisional)"
	objSpeaker    = "≥95% named speaker accuracy on committed named-voice utterances (provisional)"
	objSpeechPrec = "≥99% precision for events that trigger speech (provisional)"
	objNoDup      = "no duplicated speech for repeated observations of the same dialogue occurrence"
	objNoStale    = "zero stale audio starts after a new committed dialogue event"
)

// RunMetrics computes the report's metric rows over one run's sample outcomes.
// Every row carries its sample count; a row with no samples is n/a, never a
// 0% or 100% reading (docs/spec/12).
func RunMetrics(outcomes []sampleOutcomes) []Metric {
	var (
		inScript                      int
		recallHit, precDenom, precHit int
		cerSum, werSum                float64
		ocrN                          int
		top1N, top1Hit                int
		abstain                       int
		speakerN, speakerHit          int
		speechN, speechHit            int
		intendedN, missed             int
		dupOcc, dupExtras             int
		staleN, staleHit              int
		lat                           []float64
	)
	probs, labels := make([]float64, 0, 64), make([]bool, 0, 64)

	for _, o := range outcomes {
		s := o.sample
		firstCommit := ""
		if len(o.commitMsgIDs) > 0 {
			firstCommit = o.commitMsgIDs[0]
		}
		isInScript := s.msgID != ""
		if isInScript {
			inScript++
		}
		// Recognition: the sample's commit is its first commit, if any.
		if firstCommit != "" {
			precDenom++
			if firstCommit == s.msgID {
				precHit++
			}
		}
		if isInScript && firstCommit == s.msgID {
			recallHit++
		}
		if isInScript {
			if firstCommit == "" || firstCommit == unknownMsgID {
				abstain++
			}
			if o.topID != "" {
				top1N++
				if o.topID == s.msgID {
					top1Hit++
				}
				probs = append(probs, o.topProb)
				labels = append(labels, o.topID == s.msgID)
			}
		}
		if s.textKind == "dialogue" {
			reading := o.ocrText
			if firstCommit != "" && len(o.commitRaw) > 0 {
				reading = o.commitRaw[0]
			}
			cerSum += metrics.CER(s.text, reading)
			werSum += metrics.WER(s.text, reading)
			ocrN++
		}
		// Speech side (sidecar): the speech scheduling layer, fed the sample's
		// truth. It measures whether the layer speaks the right line exactly
		// once and never speaks stale generations.
		for i, sp := range o.speechStarted {
			speechN++
			if sp.messageID == s.msgID && s.msgID != "" {
				speechHit++
			}
			if i > 0 {
				dupExtras++ // more than one start for the one occurrence
			}
		}
		if len(o.speechStarted) > 0 {
			dupOcc++
		}
		// Named speaker: the scripted decision provider's answer for what the
		// pipeline actually committed. Right message, right speaker; a wrong
		// commit drags this down exactly like it should.
		if firstCommit != "" && firstCommit != unknownMsgID && s.speaker != SpeakerUnknown {
			speakerN++
			if o.speakerID == s.speaker {
				speakerHit++
			}
		}
		if len(o.commitTSNS) > 0 && s.firstTextNS > 0 {
			lat = append(lat, float64(o.commitTSNS[0]-s.firstTextNS)/1e6)
		}
		// Stale speech: a start whose generation is older than a commit that
		// already happened. The sidecar answers synchronously, so a nonzero
		// count is a fence defect, not a timing measurement.
		maxGen := int64(0)
		for _, sp := range o.speechStarted {
			if sp.generation < maxGen {
				staleHit++
			}
			if sp.generation > maxGen {
				maxGen = sp.generation
			}
		}
		staleN += len(o.speechStarted)
		// Missed narration: a fence on recall, measured on the pipeline. An
		// intended line that produced no commit at all was never narrated.
		if s.intended {
			intendedN++
			if firstCommit == "" {
				missed++
			}
		}
	}

	m := []Metric{
		ratioMetric("recognition_recall", float64(recallHit)/float64(inScript), inScript, "ratio", objRecall,
			fmt.Sprintf("%d/%d in-script samples committed to their exact message", recallHit, inScript)),
		ratioMetric("recognition_precision", float64(precHit)/float64(precDenom), precDenom, "ratio", "",
			fmt.Sprintf("%d/%d committing samples committed to the truth message (empty-box FPs fail it)", precHit, precDenom)),
		ratioMetric("ocr_cer", cerSum/ocrNOr(float64(ocrN)), ocrN, "ratio", "", "mean character error rate, truth text vs committed reading"),
		ratioMetric("ocr_wer", werSum/ocrNOr(float64(ocrN)), ocrN, "ratio", "", "mean word error rate, truth text vs committed reading"),
		ratioMetric("top1_exact_accuracy", float64(top1Hit)/float64(top1N), top1N, "ratio", objTop1,
			fmt.Sprintf("%d/%d ranked in-script samples ranked the truth message first", top1Hit, top1N)),
		ratioMetric("abstention_rate", float64(abstain)/float64(inScript), inScript, "ratio", "",
			"in-script samples with no confident in-script commit (abstain or no commit)"),
		naBrier("calibration_brier", probs, labels, ""),
		naECE("calibration_ece_10bin", probs, labels, ""),
		ratioMetric("named_speaker_accuracy", float64(speakerHit)/float64(speakerN), speakerN, "ratio", objSpeaker,
			"pipeline commits with a named script speaker, attributed by the scripted decision"),
		ratioMetric("positive_speech_precision", float64(speechHit)/float64(speechN), speechN, "ratio", objSpeechPrec,
			"sidecar speech layer: speech.started events that named the truth message"),
		countMetric("missed_narrations", missed, intendedN, "count", "",
			"pipeline fence on recall: intended in-script samples with no commit at all"),
		countMetric("duplicate_speech", dupExtras, dupOcc, "count", objNoDup,
			fmt.Sprintf("%d extra speech.started beyond the first, across %d occurrences", dupExtras, dupOcc)),
		countMetric("stale_speech_after_advance", staleHit, staleN, "count", objNoStale,
			fmt.Sprintf("%d speech.started older than an already-applied commit (of %d starts)", staleHit, staleN)),
	}
	m = append(m, percentileMetric("commit_latency_p50_ms", lat, 0.50, "ms", "", "capture clock: first text frame → commit transition"))
	m = append(m, percentileMetric("commit_latency_p95_ms", lat, 0.95, "ms", "", "capture clock: first text frame → commit transition"))
	return m
}

func ocrNOr(n float64) float64 {
	if n == 0 {
		return 1
	}
	return n
}

// unknownMsgID is the session's "we do not know which line this is" value.
const unknownMsgID = "unknown"

// ratioMetric is one precision/recall/accuracy row; n==0 degrades to n/a with
// value 0 (a 0/0 ratio is not a measurement, and NaN must not reach the report).
func ratioMetric(name string, v float64, n int, kind, objective, detail string) Metric {
	if n == 0 {
		v = 0
	}
	m := Metric{Name: name, Value: v, N: n, Kind: kind, Objective: objective, Detail: detail}
	m.Met = verdict(v, objective, n, false)
	return m
}

// countMetric is an integer-better-as-zero row: the objective is met when the
// count is 0 and the count had samples to count (n>0).
func countMetric(name string, count, n int, kind, objective, detail string) Metric {
	m := Metric{Name: name, Value: float64(count), N: n, Kind: kind, Objective: objective, Detail: detail}
	m.Met = verdict(float64(count), objective, n, true)
	return m
}

// percentileMetric reports the p-th percentile of a latency list (nearest-rank
// on the sorted list); an empty list is n/a.
func percentileMetric(name string, vals []float64, p float64, kind, objective, detail string) Metric {
	m := Metric{Name: name, N: len(vals), Kind: kind, Objective: objective, Detail: detail}
	if len(vals) == 0 {
		m.Met = "n/a"
		return m
	}
	sort.Float64s(vals)
	rank := int64((p * float64(len(vals))) + 0.999999) // nearest rank, ceil
	if rank < 1 {
		rank = 1
	}
	m.Value = vals[rank-1]
	m.Met = "informational"
	return m
}

// naBrier is the Brier score of the matcher's top-1 probability: the mean
// squared error of p against the 0/1 label. Empty input is n/a.
func naBrier(name string, probs []float64, labels []bool, objective string) Metric {
	m := Metric{Name: name, N: len(probs), Kind: "ratio", Objective: objective, Detail: "mean (p - y)^2 over the top-1 probability"}
	if len(probs) == 0 {
		m.Met = "n/a"
		return m
	}
	var sum float64
	for i, p := range probs {
		y := 0.0
		if labels[i] {
			y = 1
		}
		d := p - y
		sum += d * d
	}
	m.Value = sum / float64(len(probs))
	m.Met = "informational"
	return m
}

// naECE is the expected calibration error with 10 bins: the mean, over the
// occupied bins, of |mean probability - mean label|, weighted by the bin's
// share of the samples. Empty input is n/a.
func naECE(name string, probs []float64, labels []bool, objective string) Metric {
	m := Metric{Name: name, N: len(probs), Kind: "ratio", Objective: objective,
		Detail: "10 equal-width probability bins, |mean p - mean y| weighted by bin share"}
	if len(probs) == 0 {
		m.Met = "n/a"
		return m
	}
	const bins = 10
	var psum, ysum [10]float64
	var pn [10]int
	for i, p := range probs {
		b := int(p * bins)
		if b >= bins {
			b = bins - 1
		}
		psum[b] += p
		pn[b]++
		if labels[i] {
			ysum[b] += 1
		}
	}
	var ece float64
	for b := 0; b < bins; b++ {
		if pn[b] == 0 {
			continue
		}
		mp := psum[b] / float64(pn[b])
		// mean y over the bin's samples: sum(y) / count(all samples in bin).
		my := ysum[b] / float64(pn[b])
		d := mp - my
		if d < 0 {
			d = -d
		}
		ece += d * float64(pn[b]) / float64(len(probs))
	}
	m.Value = ece
	m.Met = "informational"
	return m
}

// verdict compares a value against the objective's threshold. Objectives use
// the "≥X" convention; count objectives (noDup/noStale) are met at zero.
func verdict(v float64, objective string, n int, isCount bool) string {
	if n == 0 {
		return "n/a"
	}
	if objective == "" {
		return "informational"
	}
	if isCount {
		if v == 0 {
			return "met"
		}
		return "not_met"
	}
	threshold := -1.0
	if _, err := fmt.Sscanf(objective, "\u2265%f%%", &threshold); err == nil {
		threshold /= 100
	}
	if threshold < 0 {
		return "informational"
	}
	if v >= threshold {
		return "met"
	}
	return "not_met"
}
