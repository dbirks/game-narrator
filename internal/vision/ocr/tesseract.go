package ocr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Tesseract runs the `tesseract` binary over a pipe: the preprocessed crop goes
// in on stdin as a PNG, the TSV hypothesis stream comes back on stdout. Nothing
// is written to disk, so concurrent regions never contend over a temp file and no
// frame content is left behind for retention to clean up.
//
// The zero value is usable and means `tesseract`, `eng`, PSM 6, default OEM, no
// whitelist, DefaultTimeout — the docs/spec/05 recommendation for "uniform text
// blocks". PSM 7 (single line) and a whitelist are profile settings, exposed as
// fields rather than as a bespoke constructor because a profile stores them as
// JSON (regions.ocr_settings).
type Tesseract struct {
	// Path is the binary; empty means "tesseract" resolved on PATH. An absolute
	// path skips resolution, which is how a test points at a stub.
	Path string
	// Lang is a Tesseract language pack name ("eng"). A missing pack is the
	// engine's own startup error and arrives as ErrFailed with the message
	// `Failed loading language 'foo'` in Stderr, which is actionable text rather
	// than something to pre-check: the search path is
	// environment/config-dependent and guessing it would be an assumption of the
	// kind ADR-0012 tells the wrapper not to make.
	Lang string
	// PSM is the page segmentation mode; 0 means 6. docs/spec/05: "Tesseract PSM
	// 6 for uniform text blocks, PSM 7 for a single line".
	PSM int
	// OEM selects the engine. 0 means "let Tesseract decide", which in 5.x is
	// LSTM-only — the mode whose accuracy the fixtures were tuned against.
	OEM int
	// Whitelist restricts the character set, for digit-only HUD readouts.
	// Empty means unrestricted. It is applied as a Tesseract setting rather
	// than a filter here, so the engine's own language model still sees it.
	Whitelist string
	// Timeout bounds one recognition. Zero means DefaultTimeout. It is a hard
	// kill, not a hint: on a 4-core appliance one wedged child must not stall
	// the frame loop.
	Timeout time.Duration
	// StdoutCap bounds the TSV in bytes (default 8 MiB) and StderrTail bounds the
	// captured tail in bytes (default 2 KiB). Both exist so a misbehaving child
	// cannot grow the heap (docs/spec/13 "backpressure and memory ceilings").
	StdoutCap  int
	StderrTail int
}

// Defaults for the fields above, as constants rather than magic numbers so a
// profile-validation message can name them.
const (
	// DefaultTimeout is generous for a 550×110 crop (measured single-digit tens
	// of milliseconds on x86) while still bounding the frame loop's worst case.
	DefaultTimeout = 4 * time.Second
	// DefaultStdoutCap is the TSV size ceiling; a textbox page is a few KB.
	DefaultStdoutCap = 8 << 20
	// DefaultStderrTail is how much of the engine's complaint is kept verbatim.
	DefaultStderrTail = 2048
	// DefaultPSM is 6 (uniform block of text); DefaultPSMLine is 7.
	DefaultPSM     = 6
	DefaultPSMLine = 7
)

// Name implements Namer.
func (t Tesseract) Name() string { return "tesseract" }

// Recognize implements Engine.
func (t Tesseract) Recognize(ctx context.Context, img image.Image) (Result, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if img == nil || img.Bounds().Empty() {
		return Result{}, &Failure{Kind: ErrFailed, Engine: t.Name(), Err: errors.New("no pixels to read")}
	}

	pngBytes, err := encodePNG(img)
	if err != nil {
		return Result{}, &Failure{Kind: ErrFailed, Engine: t.Name(), Err: err}
	}

	bin, err := t.binary()
	if err != nil {
		return Result{}, &Failure{Kind: ErrEngineUnavailable, Engine: t.Name(), Err: err}
	}

	runCtx, cancel := context.WithTimeoutCause(ctx, t.deadline(), ErrTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, t.args()...)
	supervise(cmd)
	limit := t.StdoutCap
	if limit <= 0 {
		limit = DefaultStdoutCap
	}
	tail := t.StderrTail
	if tail <= 0 {
		tail = DefaultStderrTail
	}
	var stdout bytes.Buffer
	stdout.Grow(1 << 14)
	out := &stdoutWriter{buf: &stdout, cap: uint64(limit)}
	errW := &tailWriter{limit: tail}
	// Stdin is a *bytes.Reader, so exec copies it on its own goroutine and closes
	// the pipe; a PNG larger than Tesseract is willing to read yields a SIGPIPE
	// exit code rather than a deadlock.
	cmd.Stdin = bytes.NewReader(pngBytes)
	cmd.Stdout = out
	cmd.Stderr = errW

	startErr := cmd.Start()
	if startErr != nil {
		if errors.Is(startErr, exec.ErrNotFound) || errors.Is(startErr, os.ErrPermission) || errors.Is(startErr, os.ErrNotExist) {
			return Result{}, &Failure{Kind: ErrEngineUnavailable, Engine: t.Name(), Args: t.args(), Err: startErr}
		}
		return Result{}, &Failure{Kind: ErrFailed, Engine: t.Name(), Args: t.args(), Err: startErr}
	}
	waitErr := cmd.Wait()
	elapsed := time.Since(start)

	if cause := context.Cause(runCtx); cause != nil {
		kind := ErrCancelled
		if errors.Is(cause, ErrTimeout) {
			// The deadline fired, not the caller.
			kind = ErrTimeout
		}
		return Result{}, &Failure{
			Kind:   kind,
			Engine: t.Name(),
			Args:   t.args(),
			Stderr: errW.String(),
			Err:    cause,
		}
	}
	if out.over {
		return Result{}, &Failure{
			Kind:   ErrOutputTooLarge,
			Engine: t.Name(),
			Args:   t.args(),
			Stderr: errW.String(),
			Err:    fmt.Errorf("stdout exceeded %d bytes", limit),
		}
	}
	if waitErr != nil {
		return Result{}, &Failure{
			Kind:   ErrFailed,
			Engine: t.Name(),
			Args:   t.args(),
			Stderr: errW.String(),
			Err:    waitErr,
		}
	}

	res, perr := ParseTSV(stdout.Bytes())
	if perr != nil {
		return Result{}, &Failure{
			Kind:   ErrFailed,
			Engine: t.Name(),
			Args:   t.args(),
			Stderr: errW.String(),
			Err:    perr,
		}
	}
	res.Duration = elapsed
	res.Engine = t.Name()
	return res, nil
}

// deadline resolves the per-recognition bound.
func (t Tesseract) deadline() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return DefaultTimeout
}

// binary resolves the executable. Resolution happens here rather than by
// trusting a cached value because PATH changes between the systemd unit and a
// developer shell, and ADR-0012 §3 forbids assuming a path.
func (t Tesseract) binary() (string, error) {
	path := t.Path
	if path == "" {
		path = "tesseract"
	}
	if strings.ContainsRune(path, os.PathSeparator) {
		// An explicit path is honored as given; LookPath would silently rewrite a
		// relative path against CWD, which is not what a caller means.
		return path, nil
	}
	return exec.LookPath(path)
}

// args builds argv. The order mirrors the documented CLI: input, output,
// options, then the config file name — `tsv` last, because a trailing token is
// read as a config file and `-l eng` after it would be parsed as one too.
func (t Tesseract) args() []string {
	psm := t.PSM
	if psm == 0 {
		psm = DefaultPSM
	}
	lang := t.Lang
	if lang == "" {
		lang = "eng"
	}
	args := []string{"stdin", "stdout", "--psm", strconv.Itoa(psm)}
	if t.OEM > 0 {
		args = append(args, "--oem", strconv.Itoa(t.OEM))
	}
	args = append(args, "-l", lang)
	if t.Whitelist != "" {
		args = append(args, "-c", "tessedit_char_whitelist="+t.Whitelist)
	}
	return append(args, "tsv")
}

// encodePNG serializes the crop. Tesseract's leptonica reader takes PNG from a
// stream, and PNG is lossless, so what the engine sees is exactly the bytes the
// image hash covered.
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := &png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("vision/ocr: encode crop: %w", err)
	}
	return buf.Bytes(), nil
}

// stdoutWriter keeps the first `cap` bytes and reports overflow, while still
// letting io.Copy drain the pipe to EOF — a pipe that is not drained is how a
// child process wedges forever (ADR-0012 "pipe deadlock").
type stdoutWriter struct {
	buf  *bytes.Buffer
	cap  uint64
	over bool
}

func (w *stdoutWriter) Write(p []byte) (int, error) {
	if uint64(w.buf.Len())+uint64(len(p)) > w.cap {
		w.over = true
		room := int(w.cap) - w.buf.Len()
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// tailWriter keeps the last limit bytes of stderr.
type tailWriter struct {
	limit int
	buf   []byte
	total int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.total += len(p)
	if w.limit <= 0 {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	if keep := 4 * w.limit; len(w.buf) > keep {
		w.buf = append(w.buf[:0], w.buf[len(w.buf)-keep:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	s := string(w.buf)
	if len(s) > w.limit {
		s = s[len(s)-w.limit:]
	}
	s = strings.Join(strings.Fields(s), " ")
	if w.total > len(s) {
		return "… " + s
	}
	return s
}

// ParseTSV turns Tesseract's TSV into a Result. It is exported for the recorded
// output test and for replaying a captured engine response without the binary
// (docs/spec/12 "captured-frame replay").
//
// Row roles come from the numbering columns, not from `level`. Tesseract's level
// values shifted when the page row was introduced — 4.x/5.x emit 1=page … 5=word,
// 3.x emitted 1=block … 4=word — and ADR-0012 records that the Debian runtime
// carries 5.3 against this dev host's 5.5, i.e. precisely this drift is expected.
// `word_num > 0` with non-empty text is a word in every version, and
// (page, block, paragraph, line) identifies a line in every version, so the parse
// does not need to know which engine produced the stream.
//
// Fields are split on tabs by hand rather than with encoding/csv: Tesseract's TSV
// is tab-separated but **not** CSV-quoted, and a word containing a double quote —
// which is what a line of quoted dialogue is made of — would otherwise be read as
// the start of a quoted field, silently swallowing tabs and newlines and
// reporting a mangled hypothesis as a successful reading.
func ParseTSV(data []byte) (Result, error) {
	lines := splitTSVLines(data)
	if len(lines) == 0 {
		return Result{}, errors.New("vision/ocr: empty tsv output")
	}
	if head := strings.Split(lines[0], "\t"); len(head) == 0 || strings.TrimSpace(head[0]) != "level" {
		return Result{}, fmt.Errorf("vision/ocr: tsv header missing level column: %q", firstLine(data))
	}

	var res Result
	var cur []Word
	var curKey string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		line := Line{Box: cur[0].Box}
		texts := make([]string, 0, len(cur))
		var sum float64
		for _, w := range cur {
			texts = append(texts, w.Text)
			sum += w.Conf
			line.Box = line.Box.Union(w.Box)
		}
		line.Text = strings.Join(texts, " ")
		line.Conf = sum / float64(len(cur))
		res.Lines = append(res.Lines, line)
		res.Words = append(res.Words, cur...)
		cur = nil
	}
	for _, line := range lines[1:] {
		row := strings.Split(line, "\t")
		if len(row) < 12 {
			continue // a truncated tail from a killed process is not a parse failure
		}
		text := strings.TrimSpace(row[11])
		wordNum, err := strconv.Atoi(strings.TrimSpace(row[5]))
		if err != nil || wordNum <= 0 || text == "" {
			continue
		}
		conf, err := strconv.ParseFloat(strings.TrimSpace(row[10]), 64)
		if err != nil {
			conf = 0
		}
		if conf < 0 {
			conf = 0 // structural rows report -1; a word without a score is not a confident word
		}
		if conf > 100 {
			conf = 100
		}
		box, _ := boxOf(row[6:10]) // a bad box costs the word its geometry, not the reading
		key := strings.Join(row[1:5], ",")
		if curKey != "" && key != curKey {
			flush()
		}
		curKey = key
		cur = append(cur, Word{Text: text, Conf: conf / 100, Box: box})
	}
	flush()

	if len(res.Lines) > 0 {
		texts := make([]string, 0, len(res.Lines))
		for _, l := range res.Lines {
			if l.Text != "" {
				texts = append(texts, l.Text)
			}
		}
		res.Text = strings.Join(texts, "\n")
	} else {
		texts := make([]string, 0, len(res.Words))
		for _, w := range res.Words {
			texts = append(texts, w.Text)
		}
		res.Text = strings.Join(texts, " ")
	}
	if len(res.Words) > 0 {
		var sum float64
		for _, w := range res.Words {
			sum += w.Conf
		}
		res.MeanConf = sum / float64(len(res.Words))
	}
	return res, nil
}

func boxOf(fields []string) (image.Rectangle, error) {
	n := make([]int, 4)
	for i := 0; i < 4; i++ {
		v, err := strconv.Atoi(strings.TrimSpace(fields[i]))
		if err != nil {
			return image.Rectangle{}, fmt.Errorf("vision/ocr: bad box field %q: %w", fields[i], err)
		}
		n[i] = v
	}
	if n[2] <= 0 || n[3] <= 0 {
		return image.Rectangle{}, nil
	}
	return image.Rect(n[0], n[1], n[0]+n[2], n[1]+n[3]), nil
}

func firstLine(data []byte) string {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return string(data[:i])
	}
	return string(data)
}

// splitTSVLines splits the engine's output into rows, tolerating both line
// endings and a missing trailing newline. It exists instead of bufio.Scanner so a
// single enormous row (a 10 MB word, i.e. a broken engine) is returned rather than
// silently dropped by the scanner's token limit — the caller's stdout cap already
// bounds total memory, and dropping rows here would turn a failure into a
// partial result.
func splitTSVLines(data []byte) []string {
	s := string(data)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
