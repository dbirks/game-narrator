package ocr

import (
	"context"
	"errors"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubTesseract writes a fake engine into t.TempDir() and returns its path. The
// milestone requires the supervision contract to be tested rather than asserted:
// ADR-0012 §2's promises (kill the process group, bounded pipes, typed failures)
// are only real if a child that ignores its parent can be shown to die.
//
// It is a shell script, which is fine: the *wrapper* is forbidden a shell
// (argv-only exec), and the fake is the thing being exec'd. Its behaviour is
// chosen by FAKE_MODE so one file covers echo, hang, spew and fail.
func stubTesseract(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-tesseract")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// tinyImage is any non-empty frame; the stubs do not read pixels.
func tinyImage() image.Image {
	i := image.NewGray(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		i.SetGray(x, 4, color.Gray{Y: 200})
	}
	return i
}

func TestTesseractRunsRealImageThroughInputFile(t *testing.T) {
	// The stub proves the input contract end to end: the crop arrives as a PNG
	// file operand (ADR-0095: file input is the measured-fast path on the A53),
	// the file decodes, and the engine's argv is what we said it would be.
	argPath := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKE_ARGV", argPath)
	stub := stubTesseract(t, `printf '%s\n' "$@" > "$FAKE_ARGV"
printf 'level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n5\t1\t1\t1\t1\t1\t1\t1\t4\t4\t99\thello\n'
exit 0`)
	eng := Tesseract{Path: stub, TempDir: t.TempDir()}
	res, err := eng.Recognize(context.Background(), tinyImage())
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if res.Text != "hello" || res.MeanConf != 0.99 {
		t.Errorf("res = %+v", res)
	}
	if res.Engine != "tesseract" {
		t.Errorf("Engine = %q", res.Engine)
	}
	if res.Duration <= 0 {
		t.Error("Duration was not measured")
	}
	raw, err := os.ReadFile(argPath)
	if err != nil {
		t.Fatalf("stub did not record argv: %v", err)
	}
	args := strings.Fields(string(raw))
	if len(args) < 3 {
		t.Fatalf("argv = %v; want the input file, stdout and the engine flags", args)
	}
	// The first operand is the crop's PNG file; the wrapper removed it once the
	// call returned (docs/spec/14: no frame content left behind).
	if _, err := os.Stat(args[0]); err == nil {
		t.Errorf("input file %q survived the call; the wrapper must remove it", args[0])
	}
	if args[1] != "stdout" {
		t.Errorf("second operand = %q, want stdout (the TSV hypothesis stream)", args[1])
	}
	want := []string{"--psm", "6", "-l", "eng", "tsv"}
	if strings.Join(args[2:], " ") != strings.Join(want, " ") {
		t.Errorf("argv after operands = %v, want %v (docs/spec/05 PSM 6 for a uniform block)", args[2:], want)
	}
	for _, a := range args {
		// "no sh -c anywhere" is ADR-0012 §1, asserted on the actual argv.
		if a == "sh" || a == "bash" || a == "-c" {
			t.Errorf("argv contains a shell token: %v", args)
		}
	}
}

func TestTesseractArgvCarriesProfileSettings(t *testing.T) {
	// A whitelist with spaces and a semicolon in it is the injection test: docs
	// /spec/12's "malicious input" family includes a profile field that a shell
	// would execute. It must arrive as one argument.
	argPath := filepath.Join(t.TempDir(), "argv0")
	t.Setenv("FAKE_ARGV", argPath)
	stub := stubTesseract(t, `printf '%s\n' "$@" > "$FAKE_ARGV"; printf 'level\ttext\n'; exit 0`)
	eng := Tesseract{Path: stub, PSM: DefaultPSMLine, OEM: 2, Lang: "eng", Whitelist: "0123456789 :;"}
	if _, err := eng.Recognize(context.Background(), tinyImage()); err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	raw, err := os.ReadFile(argPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	joined := strings.Join(lines, " ")
	for _, want := range []string{"--psm 7", "--oem 2", "-l eng"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", lines, want)
		}
	}
	if !containsExact(lines, "-c") || !containsExact(lines, "tessedit_char_whitelist=0123456789 :;") {
		t.Errorf("whitelist was not passed as one -c setting: %q", lines)
	}
	if lines[len(lines)-1] != "tsv" {
		t.Errorf("config file %q must be the final argument; a token after it is read as another config", lines[len(lines)-1])
	}
	// The first operand is the input file, not the literal token "stdin": the
	// file-based contract (ADR-0095) replaces the pipe.
	if lines[0] == "stdin" {
		t.Errorf("argv still uses stdin as the input operand: %v", lines)
	}
}

func containsExact(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestTesseractKillOnCancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	t.Setenv("FAKE_PIDS", pidFile)
	// The stub backgrounds a child before waiting, which is the case a naive
	// cmd.Process.Kill() gets wrong: the direct child dies and the grandchild
	// keeps a pipe open forever.
	stub := stubTesseract(t, `echo "$$" >> "$FAKE_PIDS"
sleep 60 &
echo "$!" >> "$FAKE_PIDS"
wait`)
	eng := Tesseract{Path: stub, Timeout: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, err := eng.Recognize(ctx, tinyImage())
		done <- outcome{err}
	}()
	time.Sleep(300 * time.Millisecond) // long enough for the stub to have recorded both pids
	cancel()

	var err error
	select {
	case o := <-done:
		err = o.err
	case <-deadline.C:
		t.Fatal("Recognize did not return after its context was cancelled")
	}
	if !errors.Is(err, ErrCancelled) {
		t.Errorf("error = %v, want ErrCancelled", err)
	}

	pids := readPIDs(t, pidFile)
	if len(pids) < 2 {
		t.Fatalf("stub recorded %d pids, want the script and its child: %v", len(pids), pids)
	}
	// The whole process group must be gone, not just the process we spawned.
	// processGone (pool_test.go) treats a zombie — killed, pending reap — as
	// gone: in a container whose PID 1 never calls wait (the default of a
	// `docker run` test sandbox), the grandchild re-parents to that PID 1 and
	// lingers as a zombie that Kill(pid, 0) keeps answering for (issue #21).
	cutoff := time.Now().Add(2 * time.Second)
	for _, pid := range pids {
		for {
			if processGone(pid) {
				break
			}
			if time.Now().After(cutoff) {
				t.Errorf("pid %d survived cancellation; the OCR child outlived its observation (ADR-0012 §2)", pid)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	var out []int
	for _, f := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("bad pid %q: %v", f, err)
		}
		out = append(out, pid)
	}
	return out
}

func TestTesseractTimeoutIsTyped(t *testing.T) {
	stub := stubTesseract(t, `sleep 30
exit 0`)
	eng := Tesseract{Path: stub, Timeout: 250 * time.Millisecond}
	start := time.Now()
	_, err := eng.Recognize(context.Background(), tinyImage())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	// The retryable/terminal distinction that ADR-0009's failure taxonomy and the
	// jobs backoff policy both key on.
	var f *Failure
	if !errors.As(err, &f) || !f.Timeout() {
		t.Errorf("error %v is not a *Failure reporting Timeout()", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v; the deadline was 250ms", elapsed)
	}
}

func TestTesseractMissingBinary(t *testing.T) {
	eng := Tesseract{Path: filepath.Join(t.TempDir(), "not-installed")}
	_, err := eng.Recognize(context.Background(), tinyImage())
	if !errors.Is(err, ErrEngineUnavailable) {
		t.Fatalf("error = %v, want ErrEngineUnavailable", err)
	}
	// A name that is not on PATH resolves the same way, which is what `doctor`
	// reports on a host without the package (docs/spec/10).
	t.Setenv("PATH", t.TempDir())
	_, err = Tesseract{}.Recognize(context.Background(), tinyImage())
	if !errors.Is(err, ErrEngineUnavailable) {
		t.Errorf("empty Path: error = %v, want ErrEngineUnavailable", err)
	}
}

func TestTesseractStderrIsTruncatedAndActionable(t *testing.T) {
	// A missing language pack is the real failure an operator hits, and the
	// message has to survive: 10 MB of noise must not be kept, but the words that
	// say what to install must be.
	stub := stubTesseract(t, `i=0
while [ $i -lt 400 ]; do
  echo "Emitting a great deal of stderr noise to overrun any buffer $i" >&2
  i=$((i+1))
done
echo "Error: Failed loading language 'fra'" >&2
exit 10`)
	eng := Tesseract{Path: stub, StderrTail: 256}
	_, err := eng.Recognize(context.Background(), tinyImage())
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("error = %v, want ErrFailed", err)
	}
	msg := err.Error()
	if len(msg) > 4096 {
		t.Errorf("error message is %d bytes; the stderr tail cap did not apply", len(msg))
	}
	if !strings.Contains(msg, "Failed loading language") {
		t.Errorf("stderr tail lost the actionable part: %s", lastPart(msg, 300))
	}
	if strings.Contains(msg, "Emitting a great deal of stderr noise to overrun any buffer 0\n") {
		t.Error("the head of the stderr dump was kept instead of the tail")
	}
	var f *Failure
	if errors.As(err, &f) && f.Stderr == "" {
		t.Error("Failure.Stderr is empty; nothing for the UI or the log to show")
	}
}

func lastPart(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func TestTesseractEmptyInputIsNotAProcessRun(t *testing.T) {
	// A crop that came back empty (region off-frame, black screen) must fail fast
	// rather than spawn a child to read nothing (docs/spec/13 "graceful degraded
	// modes").
	called := filepath.Join(t.TempDir(), "ran")
	stub := stubTesseract(t, `touch `+called+`; exit 3`)
	eng := Tesseract{Path: stub}
	if _, err := eng.Recognize(context.Background(), image.NewGray(image.Rectangle{})); !errors.Is(err, ErrFailed) {
		t.Errorf("empty image: error = %v, want ErrFailed", err)
	}
	if _, err := os.Stat(called); err == nil {
		t.Error("the engine was invoked for an empty crop")
	}
	if _, err := eng.Recognize(context.Background(), nil); !errors.Is(err, ErrFailed) {
		t.Errorf("nil image: error = %v, want ErrFailed", err)
	}
}

func TestTesseractHonoursCallerDeadline(t *testing.T) {
	stub := stubTesseract(t, "sleep 30")
	eng := Tesseract{Path: stub, Timeout: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := eng.Recognize(ctx, tinyImage())
	if !errors.Is(err, ErrCancelled) {
		t.Errorf("error = %v, want ErrCancelled from the caller's deadline", err)
	}
}

func TestEngineInterfaceIsImplemented(t *testing.T) {
	// The VLM fallback of docs/spec/05 must be substitutable; this is the compile
	// time assertion that the seam is an interface and not a struct.
	eng := Tesseract{}
	var e Engine = eng
	if e == nil || eng.Name() != "tesseract" {
		t.Errorf("Name = %q", eng.Name())
	}
}
