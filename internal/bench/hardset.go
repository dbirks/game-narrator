package bench

// hardset.go is the class (b″) calibration data of ADR-0058 amendment 4:
// the captured fixture's frames replayed through the same real pipeline,
// against a profile whose candidate pool is augmented with synthetic
// near-duplicate distractor lines generated deterministically from each
// truth line (one-word swaps, dropped clause, number change, punctuation-only
// variants, word-boundary prefixes) and re-degraded at bench time at levels
// heavier than the class (b′) set, including right-edge clips that truncate
// the line's tail while leaving its head crisp (the partial-visibility pass).
//
// Provenance (AGENTS.md rule 3, "No simulation theater"): these are class
// (b″) — captured frames + adversarial candidate pool. They are NEVER
// reported as clean captures or as the class (b′) degraded renders: every
// hard-set source carries the b″ label in the calibration's per-source
// breakdown. The distractor texts are generated in memory at bench time
// from the fixture's own truth lines (a derivation, not game material):
// they are never stored, never committed and never printed (AGENTS.md
// rule 5) — the generated text lives only in the run's index and the
// artifact under artifacts/ (gitignored).
//
// Why this is the fit's raw material (ADR-0035 "Calibration"): a fit needs
// samples where the matcher's top-1 is genuinely wrong, in a way no
// temperature can be accused of manufacturing. On clean and b′ frames the
// pipeline reads the truth line near-verbatim and the truth always ranks
// first (203 fit samples, 0 incorrect top-1 — ADR-0058). Here the candidate
// set contains lines that differ from the truth by a word swap, a dropped
// clause or a digit: under the heavier degradation the OCR reading is a
// noisy partial or near-verbatim read, and the ranking of the truth against
// its near-duplicates is decided by the matcher's own scoring of the
// pipeline's own reading. When a near-duplicate outranks the truth, that is
// a real incorrect top-1 produced by the real pipeline on real frames —
// labelled by the fixture's oracle, generated at bench time, reproducible
// from the fixture alone (deterministic transformations, no RNG).
//
// The hard set is never a fit source on its own: CalibrateCombined always
// fits the mixture clean (a + b) + b′ + b″, so a b″-shaped optimum cannot
// be mistaken for the product's calibration (ADR-0058 amendment 4).

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/matcher"
)

// ClassHard is the AGENTS.md rule-3 provenance class of the hard-set
// sources: captured frames + adversarial candidate pool. It is reported
// separately from class (b) clean captures and class (b′) degraded renders
// and never labelled as either.
const ClassHard = "b″"

// hardsetDistractor is one synthetic near-duplicate line generated from a
// truth line: its kind (the construction, for the per-kind accounting) and
// the generated text. The text is a derivation of game text and must never
// leave the process (AGENTS.md rule 5): it is used only as an in-memory
// index row and is never logged or written to a tracked file.
type hardsetDistractor struct {
	kind string
	text string
}

// hardsetKinds is the construction order, fixed so a line's distractor set
// is a deterministic function of its text (re-benches agree, no RNG).
var hardsetKinds = []string{"swap", "clause-last", "clause-first", "prefix-half", "prefix-34", "number", "punct"}

// hardsetDistractors builds the distractor set of one truth line. Every
// construction is a pure function of the line's text:
//
//   - swap:        the first and last word exchange places (the line's own
//     vocabulary reordered — the near-duplicate a reader of a garbled
//     reading cannot separate from the truth by word order alone);
//   - clause-last: the line's last clause (delimited by . ; :) is dropped —
//     the degraded frame that loses the line's ending scores the shorter
//     candidate higher (the Dice denominator); with a single clause the
//     last word is dropped instead (the partial-line fallback);
//   - clause-first: the line's first clause is dropped — the partial read
//     that lost the opening instead; the two clause variants are the
//     one-clause-drop at each end, which a degraded frame can lose either;
//   - prefix-half: the line's first half of its words (word boundary);
//   - prefix-34:   the line's first three quarters of its words; the two
//     prefix variants are the truncated-prefix near-duplicates a real
//     script carries (a short line that ends where a long line continues).
//     They are the constructions that genuinely beat the truth: on a
//     truncated read (typewriter reveal, tail lost to a partial-visibility
//     frame) the read matches the shorter prefix strictly better than the
//     full line on every text component (shared trigrams and tokens over a
//     smaller denominator, a shorter edit distance), so the nearest prefix
//     is a real incorrect top-1 while the truth stays retrieved in the set
//     (not a retrieval miss). On a verbatim read the truth's exact key keeps
//     it on top — the prefixes compete only on trigrams and lose;
//   - number:      the first digit is advanced by one (wrap 9→0) — the
//     misread-digit confusion; absent when the line has no digit;
//   - punct:       one deterministic punctuation-only edit (trailing . → !,
//     else last . → ?, else last , removed, else ! appended) — the
//     punctuation-identical near-duplicate, whose folded key equals the
//     truth line's key on purpose (a real script carries such pairs).
//
// Same-key constructions tie, they do not win: the index's fold already
// rewrites the OCR-confusion pairs (0/1/i → l, rn → m, textnorm ocrFold),
// so a confusion-spelled variant folds to the truth's key exactly as the
// punct variant does — identical text component, and the deterministic
// ordinal tie-break keeps the truth on top (measured: ADR-0058 amendment 5).
// Only a strictly shorter key (the prefixes) can outrank the truth, and only
// on a read that is itself truncated.
func hardsetDistractors(text string) []hardsetDistractor {
	base := strings.TrimSpace(text)
	var out []hardsetDistractor
	seen := map[string]bool{base: true}
	add := func(kind, v string) {
		v = strings.TrimSpace(v)
		if v == "" || v == base || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, hardsetDistractor{kind: kind, text: v})
	}
	// swap
	if words := strings.Fields(base); len(words) >= 2 && words[0] != words[len(words)-1] {
		words[0], words[len(words)-1] = words[len(words)-1], words[0]
		add("swap", strings.Join(words, " "))
	}
	// clause-last / clause-first: the one-clause drop at each end.
	if clauses := splitClauses(base); len(clauses) >= 2 {
		add("clause-last", strings.Join(clauses[:len(clauses)-1], " "))
		add("clause-first", strings.Join(clauses[1:], " "))
	} else if words := strings.Fields(base); len(words) >= 2 {
		add("clause-last", strings.Join(words[:len(words)-1], " "))
	}
	// number
	for i := 0; i < len(base); i++ {
		if base[i] >= '0' && base[i] <= '9' {
			v := []byte(base)
			if v[i] == '9' {
				v[i] = '0'
			} else {
				v[i]++
			}
			add("number", string(v))
			break
		}
	}
	// prefix-half / prefix-34: the word-boundary prefixes, guarded to two
	// words minimum and strictly shorter than the line (the seen map drops
	// a prefix that coincides with an earlier variant, e.g. clause-last on a
	// two-clause line whose first clause is exactly half the words).
	if words := strings.Fields(base); len(words) >= 4 {
		n1 := len(words) / 2
		if n1 >= 2 {
			add("prefix-half", strings.Join(words[:n1], " "))
		}
		n2 := 3 * len(words) / 4
		if n2 > n1 && n2 < len(words) {
			add("prefix-34", strings.Join(words[:n2], " "))
		}
	}
	// punct
	var v string
	switch {
	case strings.HasSuffix(base, "."):
		v = base[:len(base)-1] + "!"
	case strings.Contains(base, "."):
		v = strings.TrimSuffix(base, ".") + "?"
	case strings.Contains(base, ","):
		v = strings.TrimSuffix(base, ",")
	default:
		v = base + "!"
	}
	add("punct", v)
	return out
}

// splitClauses cuts a line at . ; : delimiters, keeping each delimiter with
// the clause it terminates. The cut points (not the clause texts) are what
// the construction needs; the join back is space-separated, which matches
// the oracle's single-spaced line shape.
func splitClauses(s string) []string {
	var (
		clauses []string
		cur     strings.Builder
	)
	flush := func() {
		if c := strings.TrimSpace(cur.String()); c != "" {
			clauses = append(clauses, c)
		}
		cur.Reset()
	}
	for _, r := range s {
		switch r {
		case '.', ';', ':':
			cur.WriteRune(r)
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return clauses
}

// hardsetMessages augments the imported script with one distractor message
// per construction, for every message that is one of the fixture's truth
// lines (truthByMsgID maps the truth line's message id to its oracle text;
// only truth lines carry distractors — the rest of the script stays exactly
// the product's candidate population). Matching on the run's own resolved
// message id (not on a re-folded key) keeps a line the oracle attached by
// fuzzy match in the adversarial pool. The distractors are appended directly
// after their truth message, so on any exact-key tie the truth's ordinal is
// lower and the deterministic comparator keeps the truth on top (the
// tie-break the matcher's doc fixes: score, ordinal, ID).
// Distractor IDs are "<truth id>:hard-<kind>": unique (truth IDs are unique,
// kinds distinct) and stable across re-benches.
func hardsetMessages(msgs []games.Message, truthByMsgID map[string]string) []games.Message {
	out := make([]games.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m)
		if _, ok := truthByMsgID[m.ID]; !ok {
			continue
		}
		for _, d := range hardsetDistractors(m.OriginalText) {
			out = append(out, games.Message{
				ID:             m.ID + ":hard-" + d.kind,
				ProfileID:      m.ProfileID,
				OriginalText:   d.text,
				NormalizedText: textnorm.Normalize(d.text, textnorm.Options{}),
				SpeechText:     d.text,
				SceneID:        m.SceneID,
				SpeakerID:      m.SpeakerID,
				TextKind:       m.TextKind,
			})
		}
	}
	return out
}

// HardsetVariants is the heavier degradation set of the hard sources —
// heavier than the class (b′) set on purpose: b′ keeps the OCR recipe's
// reading mostly intact (its variants are the fit's mild-error material and
// its measured miss counts are NOT FITTED evidence, ADR-0058 amendment 1),
// while b″ exists to break the reading enough that the truth line competes
// with its near-duplicates for the top rank. Composites of the same
// deterministic primitives as b′ (fixed quality, fixed factor, fixed seed,
// fixed offset — re-benches over the same fixture reproduce the same
// frames and the same fit data).
func HardsetVariants() []DegradedVariant {
	return []DegradedVariant{
		// The partial-visibility passes (the error-producing ones): the frame's
		// right side clipped so the OCR region's right edge lands inside the
		// line. No noise, no re-encode — the clipped read is a clean truncated
		// prefix (typewriter shape), which is the reading where the prefix
		// near-duplicates outrank the truth. Measured on the recorded subtitle
		// frames the 68% clip leaves ~80% of the line's characters readable and
		// the 52% clip ~55%: two truncation depths, matched by the two prefix
		// lengths (amendment 5).
		{Name: "hard-clip68", fn: rightClip(0.68)},
		{Name: "hard-clip52", fn: rightClip(0.52)},
		// Strong codec artefacts plus sensor noise: low-quality JPEG ringing
		// on top of the pixels, then uncorrelated noise that the Otsu/median
		// recipe cannot re-separate from glyph strokes.
		{Name: "hard-jpeg28-noise12", fn: compose(jpegReencode(28), gaussianNoise(41, 12))},
		// Stroke destruction plus codec: the 3x downscale already loses most
		// glyph edges (amendment 1 measured 5/12 line frames still rankable),
		// the q45 re-encode adds blockiness on top of the blocky upscales.
		{Name: "hard-down3-jpeg45", fn: compose(downscaleUpscale(3), jpegReencode(45))},
		// The strongest geometric pass plus offset and noise: 4x scale loss
		// (0/12 line frames rankable at the truth, amendment 1), the line
		// shifted off the crop's centre, noise on top.
		{Name: "hard-down4-crop8-noise8", fn: compose(downscaleUpscale(4), cropOffset(8, 8), gaussianNoise(42, 8))},
	}
}

// compose runs the frame transformations in order (left to right).
func compose(fns ...func(image.Image) image.Image) func(image.Image) image.Image {
	return func(img image.Image) image.Image {
		out := img
		for _, f := range fns {
			out = f(out)
		}
		return out
	}
}

// hardsetProfile is the sidecar script of the hard sources: the imported
// lines plus the distractor lines (so a commit on a distractor is in-script
// and the speech lifecycle behaves as it would on a real near-duplicate
// pair — the ADR-0058 threat model — instead of reading as a script miss).
// Built in memory, never written (AGENTS.md rule 5). The imported lines keep
// the script's own speaker attribution (speakerOf); a distractor line is a
// bench artifact with no source attribution, so it stays unattributed.
func hardsetProfile(fx *CapturedFixture, augMsgs []games.Message, speakerOf func(msgID string) string) *ScriptProfile {
	p := CapturedProfile(fx, augMsgs, speakerOf)
	p.Description = "Hard-set (class b″) script: the imported game messages plus the deterministic near-duplicate distractors generated at bench time (in-memory, never stored; AGENTS.md rule 5)."
	return p
}

// sampleFrameFiles is the union of the samples' frame files in the
// fixture's manifest order (the segments partition the fixture's rows, so
// this is the fixture's whole frame list). Shared by the b′ and b″ sources.
func sampleFrameFiles(samples []Sample) []string {
	seen := map[string]bool{}
	files := make([]string, 0, len(samples))
	for _, s := range samples {
		for _, f := range s.Frames {
			if !seen[f.File] {
				seen[f.File] = true
				files = append(files, f.File)
			}
		}
	}
	return files
}

// hardsetFitSources prepares the class (b″) fit sources of one fixture: the
// fixture's samples replayed over the hard-variant degraded frames, ranked
// against the adversarial candidate pool (the imported script plus the
// near-duplicate distractors of the fixture's truth lines). Each variant is
// its own fit source with the b″ class, so the calibration's per-source
// breakdown says exactly which degradation contributed what.
//
// The augmented index and profile are built once and shared by the variant
// sources (the index is immutable and Rank is concurrency-safe; the
// profile is installed by the fit loop before each source's samples run).
// The variant frames are prepared and freed one at a time by the fit loop
// (ADR-0064, as for b′): the disk peak is one variant's frames.
func hardsetFitSources(fx *CapturedFixture, samples []Sample, msgs []games.Message, speakerOf func(msgID string) string) ([]fitSource, error) {
	// The fixture's truth lines by the run's own resolved message id: the
	// lines whose distractors enter the pool. A dialogue sample the oracle
	// could not attach to the script (MessageID nil/empty) gets no
	// distractors — there is no truth message for them to near-duplicate.
	truthByMsgID := map[string]string{}
	for _, s := range samples {
		if s.TextKind != "dialogue" || s.MessageID == nil || *s.MessageID == "" {
			continue
		}
		truthByMsgID[*s.MessageID] = s.SourceText
	}
	if len(truthByMsgID) == 0 {
		// No dialogue sample resolved to the script: nothing to build the
		// adversarial pool around. Not an error — the fixture contributes no
		// b″ source (its clean/b′ sources still run).
		return nil, nil
	}
	augMsgs := hardsetMessages(msgs, truthByMsgID)
	idx, err := matcher.Build(augMsgs, nil, matcher.IndexOptions{})
	if err != nil {
		return nil, fmt.Errorf("bench: hardset matcher index: %v", err)
	}
	prof := hardsetProfile(fx, augMsgs, speakerOf)
	regions := prof.BuildRegions()

	files := sampleFrameFiles(samples)
	var srcs []fitSource
	for _, v := range HardsetVariants() {
		var (
			dir   string
			names map[string]string
			work  string
		)
		srcs = append(srcs, fitSource{
			name:    fmt.Sprintf("%s (captured + hardset %s + adversarial pool — class b″, %d samples)", fx.ID, v.Name, len(samples)),
			class:   ClassHard,
			samples: samples,
			prepare: func() error {
				d, n, err := degradedFramesDir(fx.Dir, v, files)
				if err != nil {
					return err
				}
				w, err := os.MkdirTemp("", "bench-calibrate-hardset-ts-")
				if err != nil {
					os.RemoveAll(d)
					return fmt.Errorf("bench: hardset %s: temp dir: %v", v.Name, err)
				}
				dir, names, work = d, n, w
				return nil
			},
			cleanup: func() {
				os.RemoveAll(dir)
				os.RemoveAll(work)
			},
			frames: func(id string) (string, string, bool) {
				for _, s := range samples {
					if s.ID != id {
						continue
					}
					// Same frames, same timestamps — the degradation changes
					// pixels, never the recording's clock — renamed to the
					// variant's files (as for b′).
					frames := make([]Frame, len(s.Frames))
					for i, f := range s.Frames {
						frames[i] = f
						if n, ok := names[f.File]; ok {
							frames[i].File = n
						}
					}
					tsPath := filepath.Join(work, "ts-"+v.Name+"-"+id+".json")
					if err := writeManifest(tsPath, frames); err != nil {
						return "", "", false
					}
					return dir, tsPath, true
				}
				return "", "", false
			},
			idx:     idx,
			regions: regions,
			profile: prof,
		})
	}
	return srcs, nil
}
