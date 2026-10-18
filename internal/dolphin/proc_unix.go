//go:build unix

package dolphin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/dbirks/game-narrator/internal/capture"
	"golang.org/x/sys/unix"
)

// The unix production seams. The whole bridge is a dev-host tool (Xvfb,
// x11grab, FIFOs, process-group signals); on other platforms the stub file
// answers "unix-only" so the package still compiles and the fake seams keep
// the unit tests running.

// supervisedAttr is the SysProcAttr every process the bridge spawns gets.
// Setpgid puts the child (and anything it in turn spawns) in its own process
// group so Stop can signal the whole tree at once. Pdeathsig makes the
// kernel SIGKILL the child when *this* process dies abruptly — kill, OOM,
// crash, orchestrator reaping the test worker. That abrupt-parent-death path
// is exactly where the original leak lived (a background real test got
// killed and its dolphin-emu survived as an orphan); Setpgid alone does not
// cover it, because nothing is left to send the group signal.
//
// Pdeathsig is a Linux implementation detail the kernel ignores on other
// unix platforms, where the Setpgid half still applies.
//
// Pdeathsig fires when the *thread* that forked the child exits, so every
// supervised spawn must go through spawnSupervised (locked thread held for
// the child's whole lifetime); otherwise Go's thread retirement can deliver
// the death signal at a random moment while the parent process is still up.
func supervisedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// reapTimeout bounds the final reap after the SIGKILL escalation: SIGKILL
// is uncatchable, so a child that is still present after this long is a
// kernel-level surprise, and Stop reports it instead of returning success
// over a child that is still in /proc.
const reapTimeout = 5 * time.Second

// stopProcessGroup is the shared body of every Stop closure the bridge owns
// (Xvfb, Dolphin): SIGTERM the group, wait up to grace for the reaper's
// buffered value, escalate to SIGKILL only if the child outlives the grace,
// and reap with a bounded timeout so Stop returns only once the child is
// really gone from /proc. The first select consumes done at most once (the
// old shape consumed it in the select and then double-received on the normal
// path, which hung Stop); the second receive after the escalation sees the
// buffered reap the same channel already absorbed, so no value is ever lost
// to a zombie.
func stopProcessGroup(pid int, grace time.Duration, done <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case err := <-done:
		// A signal death is the expected outcome of Stop (our SIGTERM, or the
		// escalation SIGKILL below), so it is not an error; a real exit code is.
		if stoppedBySignal(err) {
			return nil
		}
		return err
	case <-time.After(grace):
	}
	// Escalate: SIGKILL the whole group. ESRCH means the group id does not
	// exist — which happens when the child is not the group leader (Setpgid
	// never took effect). Never treat that as success while the leader may
	// still be alive: SIGKILL the leader directly.
	if err := capture.KillGroup(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if procAlive(pid) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("dolphin: SIGKILL pid %d after the group escalation: %w", pid, err)
		}
	}
	// Bounded reap: wait once more for the reaper's value so Stop does not
	// return while /proc/<pid> still exists. If the child died during the
	// grace the value is already buffered and this returns immediately.
	select {
	case err := <-done:
		if stoppedBySignal(err) {
			return nil
		}
		return err
	case <-time.After(reapTimeout):
		return fmt.Errorf("dolphin: pid %d still present %s after the SIGKILL escalation (stop incomplete)", pid, reapTimeout)
	}
}

// spawnSupervised starts cmd and reaps it on ONE locked OS thread held for
// the child's whole lifetime. Pdeathsig (supervisedAttr) is delivered by the
// kernel when the *thread* that forked the child exits — not when the
// process exits — and Go retires pooled threads at will: a Start issued from
// an ordinary goroutine can therefore SIGKILL its own child at a random
// moment seconds later (golang/go#10743). Locking the thread for spawn +
// Wait keeps the forking thread alive for exactly the child's lifetime and
// no longer (when Wait returns the goroutine exits and the runtime releases
// the thread, which is harmless because the child is already gone).
// The start result is synchronous so a start failure still propagates to the
// caller before it records the pid.
func spawnSupervised(cmd *exec.Cmd) (pid int, done <-chan error, err error) {
	type start struct {
		pid int
		err error
	}
	startCh := make(chan start, 1)
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			startCh <- start{0, err}
			return
		}
		startCh <- start{cmd.Process.Pid, nil}
		result <- cmd.Wait()
	}()
	s := <-startCh
	if s.err != nil {
		return 0, nil, s.err
	}
	return s.pid, result, nil
}

// stoppedBySignal reports whether a reaped child was killed by a signal
// (as opposed to exiting with a code).
func stoppedBySignal(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ProcessState == nil {
		return false
	}
	ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}

// realStartXvfb spawns Xvfb in its own process group (ADR-0012 supervision,
// shared with the ffmpeg pipe) and returns a stop function that SIGTERMs the
// group, waits 10 s and then SIGKILLs it.
func realStartXvfb(bin string) func(display, geometry string, logf io.Writer) (pid int, stop func() error, err error) {
	return func(display, geometry string, logf io.Writer) (pid int, stop func() error, err error) {
		path, err := exec.LookPath(bin)
		if err != nil {
			return 0, nil, fmt.Errorf("dolphin: %s not on PATH (dev dependency: xorg-server-xvfb): %w", bin, err)
		}
		cmd := exec.CommandContext(context.Background(), path, display, "-screen", "0", geometry, "-nolisten", "tcp")
		cmd.SysProcAttr = supervisedAttr()
		cmd.Stdout = logf
		cmd.Stderr = logf
		// Spawn and reap on one locked thread for the child's whole lifetime
		// (Pdeathsig is per-thread; see spawnSupervised).
		pid, done, err := spawnSupervised(cmd)
		if err != nil {
			return 0, nil, fmt.Errorf("dolphin: start Xvfb %s: %w", display, err)
		}
		stop = func() error { return stopProcessGroup(pid, 10*time.Second, done) }
		return pid, stop, nil
	}
}

// realStartDolphin is the production Options.StartDolphin seam. It inherits
// the environment plus DISPLAY, so the emulator shares the private display.
// Stop is graceful-first: Dolphin writes its Config/*.ini only on a clean
// exit (ADR-0007), so SIGTERM gets 15 s before the group is SIGKILLed.
func realStartDolphin(bin string) func(display string, args []string, logf io.Writer) (pid int, stop func() error, err error) {
	return func(display string, args []string, logf io.Writer) (pid int, stop func() error, err error) {
		path, err := exec.LookPath(bin)
		if err != nil {
			return 0, nil, fmt.Errorf("dolphin: %s not on PATH: %w", bin, err)
		}
		cmd := exec.CommandContext(context.Background(), path, args...)
		cmd.Env = append(os.Environ(), "DISPLAY="+display)
		cmd.SysProcAttr = supervisedAttr()
		cmd.Stdout = logf
		cmd.Stderr = logf
		// Spawn and reap on one locked thread for the child's whole lifetime
		// (Pdeathsig is per-thread; see spawnSupervised).
		pid, done, err := spawnSupervised(cmd)
		if err != nil {
			return 0, nil, fmt.Errorf("dolphin: start %s: %w", bin, err)
		}
		stop = func() error { return stopProcessGroup(pid, 15*time.Second, done) }
		return pid, stop, nil
	}
}

// procAlive probes a pid with signal 0.
func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// mkFIFO creates the named pipe (the os package has no Mkfifo).
func mkFIFO(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}

// rawReaddir returns the directory's entries in kernel readdir order minus
// "." and ".." — the enumeration 2609's Pipes::PopulateDevices indexes the
// device numbers with (ScanDirectoryTree over std::filesystem's
// directory_iterator is unsorted and excludes dot entries; Go's os.ReadDir
// sorts by name and would not be that order). Used to compute the
// "Pipe/<i>/<name>" strings before boot (ADR-0043).
func rawReaddir(dir string) ([]string, error) {
	d, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("dolphin: open %s: %w", dir, err)
	}
	defer unix.Close(d)
	// struct dirent: d_ino + d_off + d_reclen + d_type, then d_name
	// (NUL-terminated, 4-aligned); d_reclen spans entry to entry. The on-disk
	// struct has no length field — the name runs to its NUL.
	var probe unix.Dirent
	nameOff := int(unsafe.Offsetof(probe.Name))
	var out []string
	buf := make([]byte, 64*1024)
	for {
		n, gerr := unix.Getdents(d, buf)
		if n == 0 {
			break // end of directory (Getdents reports EOF as 0, nil)
		}
		off := 0
		for off < n {
			ent := (*unix.Dirent)(unsafe.Pointer(&buf[off]))
			nameEnd := off + int(ent.Reclen)
			name := ""
			for i := off + nameOff; i < nameEnd; i++ {
				if buf[i] == 0 {
					break
				}
				name += string(buf[i])
			}
			if name != "." && name != ".." {
				out = append(out, name)
			}
			off = nameEnd
		}
		if gerr == nil {
			continue
		}
		if errors.Is(gerr, unix.EINVAL) {
			break
		}
		return nil, fmt.Errorf("dolphin: getdents %s: %w", dir, gerr)
	}
	return out, nil
}
