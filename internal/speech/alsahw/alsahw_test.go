package alsahw

import (
	"context"
	"math"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
)

// ── the fake ALSA writer ───────────────────────────────────────────────────
//
// fakePCM is the test seam: it records the negotiated params (the
// hw_params contract), the write stream with timestamps (the keep-alive
// cadence), the bytes (the resample assertions) and the op sequence, and it
// can inject a xrun once to exercise the recovery. It never blocks, so the
// sink's own pacing logic (the poll cadence) is what the timing asserts
// measure.

type writeRec struct {
	ts    time.Time
	Bytes []byte
}

type fakePCM struct {
	// mu guards every field below: the sink's writer goroutine touches
	// them concurrently with the test (which reads the write stream while
	// the keep-alive is still ticking).
	mu sync.Mutex

	setupAt   time.Time
	setupSeen bool
	params    Params

	writes []writeRec

	xrunArmed  bool          // the next write fails with EPIPE once
	forceState int           // when non-zero, Status always reports this state
	pace       time.Duration // when > 0, each write takes this long (real-time device model)
	restarts   int
	state      int
	hwPtr      uint64
	closed     bool
}

func newFakePCM() *fakePCM {
	return &fakePCM{state: stateRunning}
}

func (f *fakePCM) Setup(p Params) error {
	f.mu.Lock()
	f.setupSeen = true
	f.params = p
	f.setupAt = time.Now()
	f.mu.Unlock()
	return nil
}

func (f *fakePCM) Status() (int, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forceState != 0 {
		return f.forceState, f.hwPtr, nil
	}
	return f.state, f.hwPtr, nil
}

func (f *fakePCM) Restart() error {
	f.mu.Lock()
	f.restarts++
	f.state = stateRunning
	f.mu.Unlock()
	return nil
}

func (f *fakePCM) WriteFrames(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.xrunArmed {
		f.xrunArmed = false
		f.state = stateXRun
		return 0, syscall.EPIPE
	}
	if f.pace > 0 {
		time.Sleep(f.pace)
	}
	f.writes = append(f.writes, writeRec{ts: time.Now(), Bytes: append([]byte(nil), b...)})
	f.state = stateRunning
	f.hwPtr += uint64(len(b) / 4)
	return len(b) / 4, nil
}

func (f *fakePCM) ReadyForWrite(time.Duration) error { return nil }

func (f *fakePCM) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

// snapshot returns a copy of the recorded writes (the cadence test reads
// them while the keep-alive is still ticking).
func (f *fakePCM) snapshot() []writeRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]writeRec, len(f.writes))
	copy(out, f.writes)
	return out
}

// allBytes concatenates the delivered stream.
func (f *fakePCM) allBytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, w := range f.writes {
		n += len(w.Bytes)
	}
	out := make([]byte, 0, n)
	for _, w := range f.writes {
		out = append(out, w.Bytes...)
	}
	return out
}

func newTestSink(t *testing.T, keepAlive time.Duration) (*Sink, *fakePCM) {
	t.Helper()
	fake := newFakePCM()
	s, err := New("hw:ArduinoImolaHPH,3", UNOQHubHDMI, keepAlive)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Open = func(string) (PCM, error) { return fake, nil }
	return s, fake
}

// toneWAV builds a 24 kHz mono tone WAV the way the TTS pipeline delivers
// it: 440 Hz, the seconds given, 0.25 amplitude.
func toneWAV(seconds float64) []byte {
	return speech.MakeToneWAV(440, seconds, 24000)
}

// ── the hw_params contract ─────────────────────────────────────────────────

// TestHWParamsContract is the device contract of issue #5: the sink must
// negotiate exactly the proven config (S16_LE / 48000 / 2ch / period 6240 x
// 4), setup must precede any write, and the identity is the stable
// hw:<card>,<device> — not an index.
func TestHWParamsContract(t *testing.T) {
	s, fake := newTestSink(t, 0)
	defer s.Close()
	h, err := s.Play(context.Background(), speech.PlayRequest{
		UtteranceID: "u1", Generation: 1, WAV: toneWAV(0.1), Gain: 1.0,
	})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	res := <-h.Done()
	if res.Err != nil {
		t.Fatalf("playback failed: %v", res.Err)
	}
	fake.mu.Lock()
	setupSeen, p := fake.setupSeen, fake.params
	setupAt := fake.setupAt
	fake.mu.Unlock()
	if !setupSeen {
		t.Fatal("Setup was never called: the stream was written without negotiation")
	}
	if p.Format != "S16_LE" || p.Rate != 48000 || p.Channels != 2 ||
		p.PeriodSize != 6240 || p.PeriodCount != 4 || p.BufferSize() != 24960 {
		t.Fatalf("negotiated params = %+v, want the proven S16_LE/48000/2ch/6240x4", p)
	}
	shots := fake.snapshot()
	if len(shots) == 0 {
		t.Fatal("nothing was written")
	}
	if !shots[0].ts.After(setupAt) && !shots[0].ts.Equal(setupAt) {
		t.Fatal("a write preceded the setup: the stream must be negotiated first")
	}
	// The stream is S16_LE stereo 48k: 4 bytes per frame, 96000 B/s.
	if got, want := len(fake.allBytes())%4, 0; got != want {
		t.Fatalf("delivered bytes = %d, not a whole number of 4-byte frames", len(fake.allBytes()))
	}
}

// TestRejectsNonProvenParams is the other half of the contract: a config
// the bridge was not proven at is refused at construction, not negotiated
// and then discovered silent.
func TestRejectsNonProvenParams(t *testing.T) {
	bad := []Params{
		{Format: "S24_LE", Rate: 48000, Channels: 2, PeriodSize: 6240, PeriodCount: 4},
		{Format: "S16_LE", Rate: 44100, Channels: 2, PeriodSize: 6240, PeriodCount: 4},
		{Format: "S16_LE", Rate: 48000, Channels: 2, PeriodSize: 6240, PeriodCount: 32},
	}
	for i, p := range bad {
		if _, err := New("hw:ArduinoImolaHPH,3", p, 0); err == nil {
			t.Fatalf("case %d: params %+v were accepted, want a refusal", i, p)
		}
	}
}

// TestParseDevice pins the stable-identity rule: card NAME + device, never
// an index, and the node path the kernel node is opened from.
func TestParseDevice(t *testing.T) {
	d, err := ParseDevice("hw:ArduinoImolaHPH,3")
	if err != nil {
		t.Fatalf("ParseDevice: %v", err)
	}
	if d.Card != "ArduinoImolaHPH" || d.Number != 3 {
		t.Fatalf("parsed %q", d)
	}
	if d.Node() != "/dev/snd/pcmCArduinoImolaHPHD3p" {
		t.Fatalf("node = %q", d.Node())
	}
	if d.String() != "hw:ArduinoImolaHPH,3" {
		t.Fatalf("round-trip = %q", d.String())
	}
	for _, bad := range []string{"hw:1,3", "plughw:ArduinoImolaHPH,3", "hw:ArduinoImolaHPH", "hw:,3", "default"} {
		if _, err := ParseDevice(bad); err == nil {
			t.Fatalf("%q was accepted, want a refusal", bad)
		}
	}
}

// ── the keep-alive cadence ─────────────────────────────────────────────────

// TestKeepAliveCadence: between utterances the stream must not go idle
// beyond the keep-alive window — a period of silence goes out every
// KeepAlive, which is what holds the receiver's ELD lock (the issue #5
// session.suspend-timeout-seconds=0 semantics, in-process).
func TestKeepAliveCadence(t *testing.T) {
	s, fake := newTestSink(t, 100*time.Millisecond)
	defer s.Close()

	// One short utterance to get the stream going.
	h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: toneWAV(0.1), Gain: 1.0})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	<-h.Done()

	// Measure the idle cadence for a half second.
	observeStart := time.Now()
	time.Sleep(550 * time.Millisecond)
	observeEnd := time.Now()

	idle := []writeRec{}
	for _, w := range fake.snapshot() {
		if w.ts.After(observeStart) {
			idle = append(idle, w)
		}
	}
	// A 100 ms cadence over 550 ms yields 4-7 writes; outside that window
	// the cadence is broken (timer drift or a stopped stream).
	if len(idle) < 4 || len(idle) > 10 {
		t.Fatalf("%d keep-alive writes in 550 ms at a 100 ms cadence (want ~4-7; last write %s before the end)",
			len(idle), observeEnd.Sub(idle[len(idle)-1].ts))
	}
	// The gap between consecutive writes must stay well under the window's
	// slack: a gap > 300 ms means the stream sat idle.
	for i := 1; i < len(idle); i++ {
		if gap := idle[i].ts.Sub(idle[i-1].ts); gap > 300*time.Millisecond {
			t.Fatalf("keep-alive gap %s between writes %d and %d (window 100 ms)", gap, i-1, i)
		}
	}
	// The silence is silence: every keep-alive write is all zeros.
	for i, w := range idle {
		for _, b := range w.Bytes {
			if b != 0 {
				t.Fatalf("keep-alive write %d is not silence (byte %d = %d)", i, b, b)
			}
		}
	}
}

// TestKeepAliveDisabled: KeepAlive 0 writes no idle silence (the stream
// stays open but frozen — the documented trade-off).
func TestKeepAliveDisabled(t *testing.T) {
	s, fake := newTestSink(t, 0)
	defer s.Close()
	h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: toneWAV(0.05), Gain: 1.0})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	<-h.Done()
	nAfterDone := len(fake.snapshot())
	time.Sleep(250 * time.Millisecond)
	if n := len(fake.snapshot()); n > nAfterDone {
		t.Fatalf("KeepAlive 0 still wrote %d idle frames after the utterance ended", n-nAfterDone)
	}
}

// ── the resample ───────────────────────────────────────────────────────────

// goertzel returns the magnitude of the freq bin of a 48 kHz signal —
// O(n) per frequency, no FFT library in the module.
func goertzel(x []float64, freq, rate float64) float64 {
	n := float64(len(x))
	k := 0.5 + freq*n/rate
	w := 2 * math.Pi * k / n
	c := 2 * math.Cos(w)
	s0, s1, s2 := 0.0, 0.0, 0.0
	for _, v := range x {
		s0 = v + c*s1 - s2
		s2, s1 = s1, s0
	}
	return math.Sqrt(s1*s1 + s2*s2 - c*s1*s2)
}

// TestResampleTone is the resample correctness assertion: a 440 Hz tone
// rendered at 24 kHz mono (the TTS shape) comes out of the sink as 48 kHz
// stereo whose 440 Hz bin carries the energy and the off-frequency bins do
// not, and whose RMS sits at the rendered amplitude (0.25/√2).
func TestResampleTone(t *testing.T) {
	s, fake := newTestSink(t, 0)
	defer s.Close()
	h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: toneWAV(0.5), Gain: 1.0})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	res := <-h.Done()
	if res.Err != nil {
		t.Fatalf("playback: %v", res.Err)
	}
	raw := fake.allBytes()
	if len(raw) == 0 {
		t.Fatal("nothing was delivered")
	}
	// 24 kHz mono x 0.5 s becomes 48 kHz stereo x 0.5 s: ~24000 frames.
	frames := len(raw) / 4
	if frames < 20000 || frames > 28000 {
		t.Fatalf("delivered %d frames for a 0.5 s tone at 48 kHz (want ~24000): the rate conversion is wrong", frames)
	}
	// Mono→stereo: the two channels are identical.
	var l, r []float64
	for i := 0; i+3 < len(raw); i += 4 {
		ls := int16(uint16(raw[i]) | uint16(raw[i+1])<<8)
		rs := int16(uint16(raw[i+2]) | uint16(raw[i+3])<<8)
		l = append(l, float64(ls)/32768.0)
		r = append(r, float64(rs)/32768.0)
	}
	if len(l) == 0 {
		t.Fatal("no frames decoded")
	}
	maxDiff := 0.0
	for i := range l {
		if d := math.Abs(l[i] - r[i]); d > maxDiff {
			maxDiff = d
		}
	}
	if maxDiff > 0.01 {
		t.Fatalf("mono→stereo: channel difference %f (want ≤ 0.01)", maxDiff)
	}
	// The tone: the 440 Hz bin dominates; the neighbours do not.
	m440 := goertzel(l, 440, 48000)
	m220 := goertzel(l, 220, 48000)
	m880 := goertzel(l, 880, 48000)
	if m440 < 0.3 {
		t.Fatalf("440 Hz bin magnitude %f (want ≥ 0.3 for a 0.25-amplitude tone)", m440)
	}
	if m220 > 0.05*m440 || m880 > 0.05*m440 {
		t.Fatalf("off-frequency leakage: 220 Hz %f, 880 Hz %f vs 440 Hz %f", m220, m880, m440)
	}
	// RMS of the (single-sided) tone ≈ amplitude/√2 ≈ 0.177, rendered at
	// 0.25 by MakeToneWAV.
	var sum float64
	for _, v := range l {
		sum += v * v
	}
	rms := math.Sqrt(sum / float64(len(l)))
	if rms < 0.12 || rms > 0.24 {
		t.Fatalf("RMS %f outside [0.12, 0.24] for a 0.25-amplitude tone", rms)
	}
	_ = res
}

// TestResampleStereo32 is the other documented source shape: 32-bit stereo
// at a non-2x rate (22.05 kHz is outside the TTS contract, but the
// converter must handle the admitted input formats honestly).
func TestResampleStereo32(t *testing.T) {
	// Build a 32-bit stereo 32 kHz sine WAV by hand.
	var pcm []byte
	const n = 3200 // 100 ms
	for i := 0; i < n; i++ {
		v := int32(math.Sin(2*math.Pi*440*float64(i)/32000) * 0.25 * 2147483647)
		pcm = append(pcm, byte(uint32(v)&0xff), byte(uint32(v)>>8&0xff), byte(uint32(v)>>16&0xff), byte(uint32(v)>>24&0xff))
		pcm = append(pcm, pcm[len(pcm)-4], pcm[len(pcm)-3], pcm[len(pcm)-2], pcm[len(pcm)-1])
	}
	wav := speech.EncodeWAV(pcm, 32000, 2, 32)
	s, fake := newTestSink(t, 0)
	defer s.Close()
	h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: wav, Gain: 1.0})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	<-h.Done()
	raw := fake.allBytes()
	frames := len(raw) / 4
	if frames < 4700 || frames > 5300 {
		t.Fatalf("delivered %d frames for 100 ms at 48 kHz (want ~4800)", frames)
	}
	var l []float64
	for i := 0; i+3 < len(raw); i += 4 {
		ls := int16(uint16(raw[i]) | uint16(raw[i+1])<<8)
		l = append(l, float64(ls)/32768.0)
	}
	m := goertzel(l, 440, 48000)
	if m < 0.1 {
		t.Fatalf("440 Hz bin %f after 32-bit stereo conversion (want ≥ 0.1 for a 0.25-FS tone)", m)
	}
}

// ── gain, stop, xrun, close ────────────────────────────────────────────────

// TestGainIsApplied: req.Gain scales the delivered samples (the runtime's
// wrappers set Gain to 1.0 after they scale, so the device sink applying
// the field is safe and is the honest answer for a direct consumer).
func TestGainIsApplied(t *testing.T) {
	mean := func(t *testing.T, g float64) float64 {
		t.Helper()
		s, fake := newTestSink(t, 0)
		defer s.Close()
		h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: toneWAV(0.2), Gain: g})
		if err != nil {
			t.Fatalf("Play: %v", err)
		}
		<-h.Done()
		raw := fake.allBytes()
		var sum float64
		cnt := 0
		for i := 0; i+1 < len(raw); i += 2 {
			v := int16(uint16(raw[i]) | uint16(raw[i+1])<<8)
			sum += math.Abs(float64(v))
			cnt++
		}
		if cnt == 0 {
			t.Fatal("no samples delivered")
		}
		return sum / float64(cnt) / 32768.0
	}
	m1 := mean(t, 1.0)
	mHalf := mean(t, 0.5)
	if mHalf > 0.75*m1 || mHalf < 0.25*m1 {
		t.Fatalf("gain 0.5 mean |sample| %f vs unity %f (want ~half)", mHalf, m1)
	}
}

// TestStopCutsShort: Stop(0) mid-utterance ends the playback early with
// fewer delivered bytes than the full utterance, and reports it.
func TestStopCutsShort(t *testing.T) {
	s, fake := newTestSink(t, 0)
	// Pace the device in real time: a 1 s utterance is 7-8 period chunks,
	// 40 ms each — Stop at 100 ms lands mid-utterance, the way it does on
	// hardware.
	fake.pace = 40 * time.Millisecond
	defer s.Close()
	h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: toneWAV(1.0), Gain: 1.0})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	h.Stop(0)
	res := <-h.Done()
	if !res.Interrupted {
		t.Fatal("stop was not reported as an interruption")
	}
	if res.Err != nil {
		t.Fatalf("stop produced an error: %v", res.Err)
	}
	// 1 s at 48 kHz stereo = 384000 bytes; a stop at ~100 ms must deliver
	// far less (the device buffer holds at most ~520 ms beyond that).
	if n := len(fake.allBytes()); n > 200000 {
		t.Fatalf("stop delivered %d bytes of a 384000-byte utterance (want ≤ ~200000)", n)
	}
}

// TestXrunRecovery: a write the kernel rejects with EPIPE (the xrun) is
// recovered (drop/prepare/start) and the utterance still completes; the
// underrun is counted.
func TestXrunRecovery(t *testing.T) {
	s, fake := newTestSink(t, 0)
	fake.xrunArmed = true
	defer s.Close()
	h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: toneWAV(0.2), Gain: 1.0})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	res := <-h.Done()
	if res.Err != nil {
		t.Fatalf("playback failed after an xrun: %v", res.Err)
	}
	fake.mu.Lock()
	restarts := fake.restarts
	fake.mu.Unlock()
	if restarts != 1 {
		t.Fatalf("restarts = %d, want 1 (the xrun recovery)", restarts)
	}
	if n := s.Underruns(); n != 1 {
		t.Fatalf("underruns = %d, want 1", n)
	}
	if res.Interrupted {
		t.Fatal("a recovered xrun must not interrupt the utterance")
	}
}

// TestStreamLeftIsFatal: a disconnected stream is a terminal failure, not
// a retry loop.
func TestStreamLeftIsFatal(t *testing.T) {
	s, fake := newTestSink(t, 0)
	fake.xrunArmed = true
	fake.forceState = stateDisconnected // Status reports disconnected after the EPIPE
	defer s.Close()
	h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: toneWAV(0.2), Gain: 1.0})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	res := <-h.Done()
	if res.Err == nil {
		t.Fatal("a disconnected stream must fail the playback")
	}
	if !res.Interrupted {
		// A device death mid-utterance is a terminal loss, not an operator stop.
		t.Logf("note: device-death result reports interrupted=%v", res.Interrupted)
	}
}

// TestCloseEndsInFlight: Close() during an utterance ends the handle
// (terminal result, bounded) and releases the device.
func TestCloseEndsInFlight(t *testing.T) {
	s, fake := newTestSink(t, 0)
	h, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u1", Generation: 1, WAV: toneWAV(1.0), Gain: 1.0})
	if err != nil {
		t.Fatalf("Play: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-h.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end the in-flight handle in bounded time")
	}
	<-done
	fake.mu.Lock()
	closed := fake.closed
	fake.mu.Unlock()
	if !closed {
		t.Fatal("Close did not release the device")
	}
	// The device is gone: a new Play after Close is refused.
	if _, err := s.Play(context.Background(), speech.PlayRequest{UtteranceID: "u2", Generation: 2, WAV: toneWAV(0.1)}); err == nil {
		t.Fatal("Play after Close must be refused")
	}
}

// TestKernelHWParamsEncoding pins the ABI encoding the kernel sees: the
// mask bits for access/format and the exact integer intervals for
// rate/channels/period/buffer, plus the drift report for a clamped answer.
func TestKernelHWParamsEncoding(t *testing.T) {
	k := &kernelPCM{}
	hp := k.hwParams(UNOQHubHDMI)
	// access RW_INTERLEAVED (3) and format S16_LE (2) mask bits.
	if got := hp.masks[hwParamAccess][0] & (1 << 3); got == 0 {
		t.Fatal("access mask does not carry RW_INTERLEAVED")
	}
	if got := hp.masks[hwParamFormat][0] & (1 << 2); got == 0 {
		t.Fatal("format mask does not carry S16_LE")
	}
	iv := func(idx int) uapiInterval { return hp.intervals[idx-8] }
	if got := iv(hwParamRate); got.min != 48000 || got.max != 48000 || got.flags&intervalInteger == 0 {
		t.Fatalf("rate interval %+v (want exact 48000)", got)
	}
	if got := iv(hwParamChannels); got.min != 2 || got.max != 2 {
		t.Fatalf("channels interval %+v (want exact 2)", got)
	}
	if got := iv(hwParamPeriodSize); got.min != 6240 || got.max != 6240 {
		t.Fatalf("period interval %+v (want exact 6240)", got)
	}
	if got := iv(hwParamBufferSize); got.min != 24960 || got.max != 24960 {
		t.Fatalf("buffer interval %+v (want exact 24960)", got)
	}
	if hp.cmask != (1<<hwParamAccess)|(1<<hwParamFormat) {
		t.Fatalf("cmask = %d", hp.cmask)
	}
	wantRmask := uint32((1 << (hwParamChannels - 8)) | (1 << (hwParamRate - 8)) |
		(1 << (hwParamPeriodSize - 8)) | (1 << (hwParamBufferSize - 8)))
	if hp.rmask != wantRmask {
		t.Fatalf("rmask = %d, want %d", hp.rmask, wantRmask)
	}
	// A kernel that honoured the request reports no drift; one that
	// clamped the period reports the moved value.
	if msg := hwParamsDrift(UNOQHubHDMI, &hp); msg != "" {
		t.Fatalf("no-drift case reported %q", msg)
	}
	clamped := hp
	clamped.intervals[hwParamPeriodSize-8].max = 6144
	if msg := hwParamsDrift(UNOQHubHDMI, &clamped); msg == "" || !strings.Contains(msg, "period_size=6144") {
		t.Fatalf("clamped drift = %q (want it to name period_size=6144)", msg)
	}
}
