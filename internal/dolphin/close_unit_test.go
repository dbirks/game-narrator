//go:build unix

package dolphin

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The CLOSE step's timing logic (issue #12): the boot guard, the
// advance→settle phase transition, the three-sample settle window at the
// 2 s cadence, and the streak reset. Pure (time, band) inputs — no
// Dolphin, runs in the unit gate.

// observeOne feeds one sample and returns the (phase, done) pair.
func observeOne(t *testing.T, det *CloseDetector, now time.Time, band float64) (ClosePhase, bool) {
	t.Helper()
	p, done := det.Observe(now, band)
	if done && p != ClosePhaseSettle {
		t.Fatalf("done reported in phase %v (want settle)", p)
	}
	return p, done
}

func TestCloseDetectorBootGuardBlocksEarlyFullFrames(t *testing.T) {
	var det CloseDetector
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// The restore's opening: the saved scene's bright bands (the measured
	// reason for the guard) — full-frame samples well before the guard must
	// NOT move to the settle phase (presses must keep advancing the
	// dialogue).
	for i := 0; i < 14; i++ { // 0..26 s at the 2 s cadence
		p, done := det.Observe(t0.Add(time.Duration(i)*2*time.Second), 0.27)
		if done {
			t.Fatalf("detector fired inside the boot guard at t=%ds", i*2)
		}
		if p != ClosePhaseAdvance {
			t.Fatalf("phase=%v inside the boot guard (want advance — presses must continue)", p)
		}
	}
	// Just inside the guard boundary: still 2 s short — advance only.
	p, done := det.Observe(t0.Add(30*time.Second-1*time.Second), 0.27)
	if done || p != ClosePhaseAdvance {
		t.Fatalf("t=29s: phase=%v done=%v (want advance, not done)", p, done)
	}
}

func TestCloseDetectorFiresOnThreeSamplesAtCadence(t *testing.T) {
	var det CloseDetector
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// Dialogue frames (band below the floor) through the guard.
	for i := 0; i < 20; i++ { // 0..38 s
		p, done := det.Observe(t0.Add(time.Duration(i)*2*time.Second), 0.0)
		if done || p != ClosePhaseAdvance {
			t.Fatalf("dialogue frame at t=%ds: phase=%v done=%v (want advance)", i*2, p, done)
		}
	}
	// The gameplay scene arrives at t=40 s: the first full-frame sample
	// after the guard transitions to settle (streak 1).
	p, done := det.Observe(t0.Add(40*time.Second), 0.26)
	if done {
		t.Fatal("fired on the first full-frame sample (want the settle window)")
	}
	if p != ClosePhaseSettle {
		t.Fatalf("phase=%v after the first full-frame sample (want settle — presses stop)", p)
	}
	// Two more 2 s-cadence settle samples complete the window.
	p, done = det.Observe(t0.Add(42*time.Second), 0.27)
	if done {
		t.Fatal("fired on the second full-frame sample (want a three-sample window)")
	}
	if p != ClosePhaseSettle {
		t.Fatalf("phase=%v (want settle)", p)
	}
	if p, done = det.Observe(t0.Add(44*time.Second), 0.25); !done || p != ClosePhaseSettle {
		t.Fatalf("no fire on the third full-frame sample (phase=%v done=%v)", p, done)
	}
}

func TestCloseDetectorResetsOnDialogueFrame(t *testing.T) {
	var det CloseDetector
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	// A full-frame glimpse, then the dialogue returns (a fade-back or a
	// passing bright transition): the settle streak resets and the detector
	// returns to advance (presses resume).
	for i := 0; i < 16; i++ {
		det.Observe(t0.Add(time.Duration(i)*2*time.Second), 0.0)
	}
	p, done := det.Observe(t0.Add(32*time.Second), 0.26)
	if done || p != ClosePhaseSettle {
		t.Fatalf("first full-frame glimpse: phase=%v done=%v (want settle)", p, done)
	}
	det.Observe(t0.Add(34*time.Second), 0.26)
	// The dialogue frame voids the settle streak and resumes advance.
	p, done = det.Observe(t0.Add(36*time.Second), 0.0)
	if done || p != ClosePhaseAdvance {
		t.Fatalf("dialogue frame during settle: phase=%v done=%v (want advance, not done)", p, done)
	}
	// The reset streak re-accumulates from scratch.
	det.Observe(t0.Add(38*time.Second), 0.26)
	det.Observe(t0.Add(40*time.Second), 0.26)
	p, done = det.Observe(t0.Add(42*time.Second), 0.26)
	if !done {
		t.Fatal("the reset streak never re-accumulated to the settle window")
	}
}

func TestCloseDetectorIgnoresSubCadenceBurst(t *testing.T) {
	var det CloseDetector
	t0 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 16; i++ {
		det.Observe(t0.Add(time.Duration(i)*2*time.Second), 0.0)
	}
	// A burst of fast full-frame samples (a screenshot hiccup's retry loop)
	// must not pack the window: only 2 s-spaced samples count.
	p, done := det.Observe(t0.Add(32*time.Second), 0.26)
	if done {
		t.Fatal("first burst sample fired the window")
	}
	if p != ClosePhaseSettle {
		t.Fatalf("phase=%v after the first full-frame sample (want settle)", p)
	}
	if _, done = det.Observe(t0.Add(32*time.Second+500*time.Millisecond), 0.26); done {
		t.Fatal("sub-cadence sample fired the window")
	}
	if _, done = det.Observe(t0.Add(32*time.Second+1500*time.Millisecond), 0.26); done {
		t.Fatal("sub-cadence sample fired the window")
	}
	// The next cadence sample after the burst extends the streak by one.
	det.Observe(t0.Add(34*time.Second), 0.26)
	det.Observe(t0.Add(36*time.Second), 0.26)
	if _, done = det.Observe(t0.Add(38*time.Second), 0.26); !done {
		t.Fatal("the cadence-paced settle streak never fired")
	}
}

// --- the state header oracle -------------------------------------------------

// writeStateHeaderFile builds a synthetic 2609-layout state file (header +
// a dummy payload) for the header oracle's unit tests.
func writeStateHeaderFile(t *testing.T, gameID string, cookie uint32, version string, payload int) string {
	t.Helper()
	var b []byte
	b = append(b, []byte(gameID)...)
	b = append(b, 0, 0) // reserved1
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], 0) // lzo_size
	b = append(b, u32[:]...)
	b = append(b, u32[:]...) // reserved2
	var ts [8]byte
	binary.LittleEndian.PutUint64(ts[:], math.Float64bits(1.7e9))
	b = append(b, ts[:]...)
	binary.LittleEndian.PutUint32(u32[:], cookie)
	b = append(b, u32[:]...)
	binary.LittleEndian.PutUint32(u32[:], uint32(len(version)))
	b = append(b, u32[:]...)
	b = append(b, []byte(version)...)
	var ext [16]byte
	binary.LittleEndian.PutUint16(ext[0:2], 1) // header_version
	binary.LittleEndian.PutUint16(ext[2:4], 1) // compression: LZ4
	binary.LittleEndian.PutUint32(ext[4:8], 0) // payload_offset
	binary.LittleEndian.PutUint64(ext[8:16], uint64(payload))
	b = append(b, ext[:]...)
	b = append(b, make([]byte, 16)...) // a payload byte (the file must exceed the header)
	p := filepath.Join(t.TempDir(), "GZLE01.s01")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	return p
}

func TestParseStateHeaderAcceptsTheMeasuredShape(t *testing.T) {
	p := writeStateHeaderFile(t, "GZLE01", stateVersionCookie, "Dolphin 2609", 93447681)
	info, err := ParseStateHeader(p, "GZLE01")
	if err != nil {
		t.Fatalf("ParseStateHeader: %v", err)
	}
	if info.GameID != "GZLE01" || info.VersionCookie != stateVersionCookie || info.VersionString != "Dolphin 2609" {
		t.Fatalf("parsed header = %+v, want the GZLE01/Dolphin 2609 shape", info)
	}
	if info.Compression != 1 || info.UncompressedSize != 93447681 {
		t.Fatalf("payload facts = compression %d size %d, want LZ4/93447681", info.Compression, info.UncompressedSize)
	}
}

func TestParseStateHeaderRejectsTheMismatchSuspects(t *testing.T) {
	// Issue #12's suspect list, each half checked by the oracle: a state
	// for the WRONG GAME and a state from the WRONG Dolphin state version
	// are exactly the two inputs 2609's ValidateHeaders would reject with
	// the silent fresh-boot fallback.
	if _, err := ParseStateHeader(writeStateHeaderFile(t, "RWDE01", stateVersionCookie, "Dolphin 2609", 100000), "GZLE01"); err == nil {
		t.Fatal("a different game's state header must be rejected")
	}
	if _, err := ParseStateHeader(writeStateHeaderFile(t, "GZLE01", stateVersionCookie+1, "Dolphin 2609", 100000), "GZLE01"); err == nil {
		t.Fatal("a different state version's state header must be rejected")
	}
	if _, err := ParseStateHeader(writeStateHeaderFile(t, "GZLE01", stateVersionCookie, "Dolphin 2609", 100000), ""); err == nil {
		t.Fatal("an empty want-game must not be accepted")
	}
}

func TestParseStateHeaderNamesTruncation(t *testing.T) {
	p := writeStateHeaderFile(t, "GZLE01", stateVersionCookie, "Dolphin 2609", 100000)
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, raw[:20], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseStateHeader(p, "GZLE01"); err == nil {
		t.Fatal("a truncated state file must be rejected (the silent-fallback input of issue #12)")
	}
}
