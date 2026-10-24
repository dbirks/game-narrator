package ocr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// threadEnvStub execs a fake engine that records the OpenMP variables it
// actually saw in its own environment — the argv/env contract of the
// per-process thread budget (issue #58 / ADR-0183): the knob must reach the
// child, and nothing else may change about how the child is exec'd.
func threadEnvStub(t *testing.T) string {
	t.Helper()
	return stubTesseract(t, `{
echo "OMP_NUM_THREADS=$OMP_NUM_THREADS"
echo "OMP_THREAD_LIMIT=$OMP_THREAD_LIMIT"
} > "$FAKE_ENV"
printf 'level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n5\t1\t1\t1\t1\t1\t1\t1\t4\t4\t99\thello\n'
exit 0`)
}

func envSeen(t *testing.T, path string) (numThreads, threadLimit string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("child did not record its env: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "OMP_NUM_THREADS="):
			numThreads = strings.TrimPrefix(line, "OMP_NUM_THREADS=")
		case strings.HasPrefix(line, "OMP_THREAD_LIMIT="):
			threadLimit = strings.TrimPrefix(line, "OMP_THREAD_LIMIT=")
		}
	}
	return numThreads, threadLimit
}

func runThreadStub(t *testing.T, knob string, envPath string) (numThreads, threadLimit string) {
	t.Helper()
	if knob != "" {
		t.Setenv(EnvOCRThreads, knob)
	}
	// The board's service env pins OMP_THREAD_LIMIT=1; simulate that so the
	// default case proves inheritance, not absence.
	t.Setenv("OMP_NUM_THREADS", "7")
	t.Setenv("OMP_THREAD_LIMIT", "7")
	t.Setenv("FAKE_ENV", envPath)
	stub := threadEnvStub(t)
	eng := Tesseract{Path: stub, TempDir: t.TempDir()}
	if _, err := eng.Recognize(context.Background(), tinyImage()); err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	return envSeen(t, envPath)
}

func TestOCRThreadsDefaultInheritsParent(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "env")
	num, limit := runThreadStub(t, "", envPath)
	if num != "7" || limit != "7" {
		t.Fatalf("knob unset: child env = OMP_NUM_THREADS=%q OMP_THREAD_LIMIT=%q, want the inherited 7/7 (default is the current behaviour)", num, limit)
	}
}

func TestOCRThreadsKnobOverridesChildOnly(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), "env")
	num, limit := runThreadStub(t, "3", envPath)
	if num != "3" || limit != "3" {
		t.Fatalf("knob=3: child env = OMP_NUM_THREADS=%q OMP_THREAD_LIMIT=%q, want 3/3 over the inherited 7/7", num, limit)
	}
	// The knob is a child-process budget: the parent environment is untouched.
	if got := os.Getenv("OMP_NUM_THREADS"); got != "7" {
		t.Fatalf("parent OMP_NUM_THREADS = %q, want the original 7 — the knob must not leak out of the child", got)
	}
}

func TestOCRThreadsInvalidKnobFallsBackToInheritance(t *testing.T) {
	for _, knob := range []string{"0", "-2", "abc", "1.5"} {
		t.Run(knob, func(t *testing.T) {
			envPath := filepath.Join(t.TempDir(), "env")
			num, limit := runThreadStub(t, knob, envPath)
			if num != "7" || limit != "7" {
				t.Fatalf("knob=%q: child env = %q/%q, want inherited 7/7", knob, num, limit)
			}
		})
	}
}
