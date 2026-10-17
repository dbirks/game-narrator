package jobs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/store"
)

// Re-exec protocol: the parent launches THIS test binary as a child with
// powerLossEnv set; the child does the work, announces readiness, and is
// SIGKILLed mid-work by the parent. A SIGKILL is the only "power outage" a
// test can produce honestly: no cleanup runs, no deferreds run, the process
// simply stops — which is exactly what a power cut does to the WAL and to the
// in-flight staging file.
const (
	powerLossEnv      = "GN_JOBS_POWERLOSS_DIR"
	powerLossReadyTag = "READY"
	// The child's lease: long enough that recovery is decided by the
	// store's crash-recovery semantics (RecoverOnStartup), not by a lease
	// that expires while the parent is still asserting.
	powerLossLease = 30 * time.Second

	powerLossKind   = "import"
	powerLossWorker = "powerloss-parent"

	powerLossAssetSize = 1 << 20 // 1 MiB in-flight asset write
	powerLossAssetSeed = 42
)

// powerLossBytes is a deterministic byte source shared by child and parent:
// the child stages it as the in-flight asset write, the parent hashes the same
// bytes to prove the partial content is NOT visible under its final name.
func powerLossBytes(seed uint64, size int) []byte {
	b := make([]byte, size)
	var x uint64 = seed
	for i := range b {
		x = x*6364136223846793005 + 1442695040888963407
		b[i] = byte(x >> 32)
	}
	return b
}

// TestPowerLossKilledMidWorkRecoversJobsAndAssets is the storage half of
// docs/spec/13's "Power outage" row: "Recover transactions and jobs, stop
// orphan speech". A child process opens the store, enqueues and claims a job
// (starting a lease and an in-flight attempt), and begins an asset write; the
// parent SIGKILLs it mid-work. Reopening must find: the WAL recovered with the
// committed transaction intact (integrity ok, the job row present), the
// in-flight job recovered per internal/jobs semantics (marked recoverable,
// its attempt recorded crashed, requeued because the kind is resumable, and
// claimable again by a fresh worker), and the orphan staging file discarded at
// startup rather than mistaken for a committed asset.
func TestPowerLossKilledMidWorkRecoversJobsAndAssets(t *testing.T) {
	if dir := os.Getenv(powerLossEnv); dir != "" {
		runPowerLossChild(t, dir)
		return
	}

	dir := t.TempDir()
	prog, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary path: %v", err)
	}
	cmd := exec.Command(prog, "-test.run", "^TestPowerLossKilledMidWorkRecoversJobsAndAssets$", "-test.count", "1")
	cmd.Env = append(os.Environ(), powerLossEnv+"="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	// Wait for the child to be mid-work (job claimed, asset staged), then kill.
	type readyMsg struct {
		jobID, stagingID string
		err              error
	}
	ready := make(chan readyMsg, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if !sc.Scan() {
			ready <- readyMsg{err: fmt.Errorf("child produced no READY line: %s", stderr.String())}
			return
		}
		f := strings.Fields(sc.Text())
		if len(f) != 3 || f[0] != powerLossReadyTag {
			ready <- readyMsg{err: fmt.Errorf("unexpected child line %q: %s", sc.Text(), stderr.String())}
		} else {
			ready <- readyMsg{jobID: f[1], stagingID: f[2]}
		}
	}()

	var jobID, stagingID string
	select {
	case r := <-ready:
		if r.err != nil {
			_ = cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("child did not reach the kill point: %v", r.err)
		}
		jobID, stagingID = r.jobID, r.stagingID
		// The power cuts.
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("SIGKILL child: %v", err)
		}
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("child never became ready (stderr: %s)", stderr.String())
	}
	if err := cmd.Wait(); err == nil {
		t.Log("note: child exited before the kill landed; the on-disk state is the same (unfinalized job, uncommitted staging)")
	}

	// ── reopen: the "next boot" ────────────────────────────────────────────
	ctx := context.Background()
	s, err := store.Open(ctx, store.DBPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatalf("reopen store after power loss: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate after reopen: %v", err)
	}
	// The WAL held the child's committed job transaction; recovery is what
	// makes it visible. Integrity first.
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatalf("integrity after power loss (WAL must have recovered): %v", err)
	}
	js := store.NewJobsStore(s)

	job, err := js.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("committed job row missing after power loss: %v", err)
	}
	if job.State != store.JobRunning {
		t.Fatalf("in-flight job state after power loss = %q, want %q (recovery has not run yet)", job.State, store.JobRunning)
	}
	if job.Attempts != 1 || job.LeaseOwner == "" {
		t.Fatalf("job evidence after power loss: attempts=%d leaseOwner=%q, want attempts=1 with a lease", job.Attempts, job.LeaseOwner)
	}

	// Orphan staging: the startup sweep discards it, and the partial content is
	// not visible under its final name.
	as, err := assets.NewLocal(filepath.Join(dir, "assets"))
	if err != nil {
		t.Fatalf("open asset store after power loss: %v", err)
	}
	discarded, err := as.RecoverStaging(ctx)
	if err != nil {
		t.Fatalf("RecoverStaging after power loss: %v", err)
	}
	if discarded != 1 {
		t.Fatalf("startup sweep discarded %d staging leftovers, want exactly 1 (the in-flight write)", discarded)
	}
	sum := sha256.Sum256(powerLossBytes(powerLossAssetSeed, powerLossAssetSize))
	hash := hex.EncodeToString(sum[:])
	if _, _, err := as.Get(ctx, hash); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("orphan staging content is visible as a committed asset: Get(%s…) = %v, want not-found", hash[:12], err)
	}
	if _, err := as.Commit(ctx, assets.StagingID(stagingID)); err == nil {
		t.Fatal("the orphaned staging ID is still committable after the startup sweep")
	}

	// Job recovery per internal/jobs semantics: a job a previous process left
	// running becomes recoverable, its in-flight attempt is recorded crashed,
	// and a resumable kind with budget left goes back to the queue — that is
	// the "resume" the matrix asks for (as opposed to failing the job).
	report, err := js.RecoverOnStartup(ctx, []string{powerLossKind})
	if err != nil {
		t.Fatalf("RecoverOnStartup: %v", err)
	}
	if !containsString(report.Marked, jobID) {
		t.Fatalf("recovery report marked %v, want %s to be marked recoverable", report.Marked, jobID)
	}
	if !containsString(report.Requeued, jobID) {
		t.Fatalf("recovery report requeued %v, want %s requeued (kind is resumable, budget remains)", report.Requeued, jobID)
	}
	job, err = js.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("get job after recovery: %v", err)
	}
	if job.State != store.JobQueued {
		t.Fatalf("job state after recovery = %q, want %q (resumed on the queue)", job.State, store.JobQueued)
	}
	attempts, err := js.ListJobAttempts(ctx, jobID)
	if err != nil {
		t.Fatalf("list attempts after recovery: %v", err)
	}
	if len(attempts) == 0 {
		t.Fatal("no attempt history after power loss")
	}
	last := attempts[len(attempts)-1]
	if last.Outcome != store.AttemptCrashed {
		t.Fatalf("in-flight attempt outcome = %q, want %q (the crash evidence)", last.Outcome, store.AttemptCrashed)
	}
	if last.Error == "" {
		t.Fatal("crashed attempt has no reason recorded")
	}

	// A fresh worker can now claim the recovered job: work resumes.
	claimed, err := js.Claim(ctx, powerLossWorker, []string{powerLossKind}, powerLossLease)
	if err != nil {
		t.Fatalf("claim recovered job: %v", err)
	}
	if claimed == nil || claimed.ID != jobID {
		t.Fatalf("fresh worker claimed %v, want the recovered job %s", claimed, jobID)
	}

	t.Logf("resilience: power_loss: SIGKILL mid-job+mid-asset; WAL recovered (integrity ok, job row intact), job marked recoverable with crashed attempt (%q) and requeued as resumable, orphan staging swept (%d discarded), fresh worker resumed it", last.Error, discarded)
}

// runPowerLossChild is the child side: open everything, claim the job, stage
// the asset, announce readiness, then keep "working" until killed.
func runPowerLossChild(t *testing.T, dir string) {
	ctx := context.Background()
	s, err := store.Open(ctx, store.DBPath(filepath.Join(dir, "data")))
	if err != nil {
		t.Fatalf("child: open store: %v", err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("child: migrate: %v", err)
	}
	js := store.NewJobsStore(s)
	job, _, err := js.Enqueue(ctx, store.EnqueueParams{Kind: powerLossKind, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("child: enqueue: %v", err)
	}
	if _, err := js.Claim(ctx, "powerloss-child", []string{powerLossKind}, powerLossLease); err != nil {
		t.Fatalf("child: claim: %v", err)
	}

	as, err := assets.NewLocal(filepath.Join(dir, "assets"))
	if err != nil {
		t.Fatalf("child: open assets: %v", err)
	}
	id, _, err := as.PutStaged(ctx, bytes.NewReader(powerLossBytes(powerLossAssetSeed, powerLossAssetSize)), "")
	if err != nil {
		t.Fatalf("child: stage asset: %v", err)
	}
	// Ready to be killed: the job is claimed (lease live, attempt in flight)
	// and the asset is staged but NOT committed — the on-disk shape of a power
	// cut mid-import. Straight to stdout (not through *testing.T, which logs
	// to stderr): the parent reads this line to time the kill.
	fmt.Fprintf(os.Stdout, "%s %s %s\n", powerLossReadyTag, job.ID, string(id))
	// Keep "working" so the kill lands mid-stream; if it never arrives the
	// child simply exits with the same on-disk state.
	time.Sleep(10 * time.Second)
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
