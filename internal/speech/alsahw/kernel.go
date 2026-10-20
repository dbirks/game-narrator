package alsahw

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// kernelPCM is the production PCM surface: the kernel node
// /dev/snd/pcmC<card>D<dev>p, opened non-blocking so the writer loop can
// honour Stop within one poll interval and the keep-alive timer can own
// the idle cadence (docs/spec/07: the sink paces itself; the scheduler
// only guarantees one in-flight playback).
//
// The transfer is the kernel's RW-interleaved path (SNDRV_PCM_IOCTL_
// WRITEI_FRAMES with struct snd_xferi): the same data plane aplay and
// tinyalsa use, no mmap (the bridge's proven config is RW_INTERLEAVED —
// the MMAP stream was the silent one, issue #5).
type kernelPCM struct {
	f *os.File
}

// openKernelPCM opens the node and checks it is a real device.
func openKernelPCM(node string) (*kernelPCM, error) {
	f, err := os.OpenFile(node, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("alsahw: open %s: %w", node, err)
	}
	return &kernelPCM{f: f}, nil
}

func (k *kernelPCM) fd() uintptr { return uintptr(k.f.Fd()) }

// Setup negotiates the stream: HW_PARAMS with the exact requested values,
// verified against the kernel's applied answer; SW_PARAMS (timestamps off,
// explicit start — the aplay reference); PREPARE; START. Any applied value
// the kernel clamped away from the request is an error: the sink's whole
// reason to exist is the exact config, and a "close enough" setup is the
// silent-stream defect this design refuses.
func (k *kernelPCM) Setup(p Params) error {
	hp := k.hwParams(p)
	if err := ioctl(k.fd(), ioctlHWParams, unsafe.Pointer(&hp)); err != nil {
		return fmt.Errorf("alsahw: hw_params: %w", err)
	}
	if msg := hwParamsDrift(p, &hp); msg != "" {
		// The kernel refused part of the config. Undo the partial setup so
		// the node is not left half-configured for the next opener.
		_ = ioctl(k.fd(), ioctlHWParams, unsafe.Pointer(&uapiHWParams{}))
		return fmt.Errorf("alsahw: the device did not honour the requested hw_params: %s", msg)
	}
	sp := k.swParams(p)
	if err := ioctl(k.fd(), ioctlSWParams, unsafe.Pointer(&sp)); err != nil {
		return fmt.Errorf("alsahw: sw_params: %w", err)
	}
	if err := ioctl(k.fd(), ioctlPrepare, nil); err != nil {
		return fmt.Errorf("alsahw: prepare: %w", err)
	}
	if err := ioctl(k.fd(), ioctlStart, nil); err != nil {
		return fmt.Errorf("alsahw: start: %w", err)
	}
	return nil
}

// hwParams builds the snd_pcm_hw_params asking for the exact values: the
// access/format as masks, rate/channels/period_size/buffer_size as exact
// integer intervals. rmask/cmask name the parameters the request fixes.
func (k *kernelPCM) hwParams(p Params) uapiHWParams {
	var hp uapiHWParams
	// masks: the bit of the requested access and format (alsa-lib's
	// fill_mask convention: set the parameter's own bit).
	hp.masks[hwParamAccess][accessRWInterleaved>>5] |= 1 << (accessRWInterleaved & 31)
	hp.masks[hwParamFormat][formatS16LE>>5] |= 1 << (formatS16LE & 31)
	hp.cmask = (1 << hwParamAccess) | (1 << hwParamFormat)

	set := func(idx int, min, max uint32) {
		hp.intervals[idx-8] = uapiInterval{min: min, max: max, flags: intervalInteger}
		hp.rmask |= 1 << uint(idx-8)
	}
	set(hwParamChannels, uint32(p.Channels), uint32(p.Channels))
	set(hwParamRate, uint32(p.Rate), uint32(p.Rate))
	set(hwParamPeriodSize, p.PeriodSize, p.PeriodSize)
	set(hwParamBufferSize, p.BufferSize(), p.BufferSize())
	return hp
}

// hwParamsDrift reports which of the requested values the kernel's answer
// moved away from the request ("" when the kernel honoured them all).
func hwParamsDrift(p Params, hp *uapiHWParams) string {
	type drift struct {
		want, got uint32
		name      string
	}
	check := []drift{
		{uint32(p.Channels), hp.intervals[hwParamChannels-8].max, "channels"},
		{uint32(p.Rate), hp.intervals[hwParamRate-8].max, "rate"},
		{p.PeriodSize, hp.intervals[hwParamPeriodSize-8].max, "period_size"},
		{p.BufferSize(), hp.intervals[hwParamBufferSize-8].max, "buffer_size"},
	}
	var msgs []string
	for _, d := range check {
		if d.want != d.got {
			msgs = append(msgs, fmt.Sprintf("%s=%d requested %d", d.name, d.got, d.want))
		}
	}
	// access/format: the applied mask must still carry our bit.
	if hp.masks[hwParamAccess][accessRWInterleaved>>5]&(1<<(accessRWInterleaved&31)) == 0 {
		msgs = append(msgs, "access=RW_INTERLEAVED not applied")
	}
	if hp.masks[hwParamFormat][formatS16LE>>5]&(1<<(formatS16LE&31)) == 0 {
		msgs = append(msgs, "format=S16_LE not applied")
	}
	if len(msgs) == 0 {
		return ""
	}
	return "applied: " + join(msgs)
}

// swParams builds the snd_pcm_sw_params: tstamp_mode NONE (the aplay
// reference), explicit start (start_threshold 0: the sink issues START
// itself and never auto-starts), no silence prefill.
func (k *kernelPCM) swParams(p Params) uapiSWParams {
	var sp uapiSWParams
	sp.tstampMode = 0 // SNDRV_PCM_TSTAMP_NONE
	sp.availMin = 1
	sp.startThreshold = 0
	sp.stopThreshold = 0
	sp.silenceThresh = 0
	sp.silenceSize = 0
	sp.boundary = uint64(p.BufferSize())
	sp.proto = swProto2_0_12
	sp.tstampType = swTstampMonotonic
	return sp
}

// Status reports the stream state and the hardware pointer.
func (k *kernelPCM) Status() (int, uint64, error) {
	var st uapiPCMStatus
	if err := ioctl(k.fd(), ioctlStatus, unsafe.Pointer(&st)); err != nil {
		return 0, 0, err
	}
	return int(st.state), st.hwPtr, nil
}

// Restart is the xrun recovery: DROP + PREPARE + START (alsa-lib's
// snd_pcm_recover sequence).
func (k *kernelPCM) Restart() error {
	if err := ioctl(k.fd(), ioctlDrop, nil); err != nil {
		return fmt.Errorf("alsahw: drop: %w", err)
	}
	if err := ioctl(k.fd(), ioctlPrepare, nil); err != nil {
		return fmt.Errorf("alsahw: prepare after xrun: %w", err)
	}
	if err := ioctl(k.fd(), ioctlStart, nil); err != nil {
		return fmt.Errorf("alsahw: start after xrun: %w", err)
	}
	return nil
}

// WriteFrames transfers S16_LE interleaved frames via the kernel's writei
// transfer. On a healthy stream the fd's non-blocking state means a full
// buffer answers EAGAIN (the caller polled first, so this is rare); an
// xrun answers EPIPE. The caller owns the Status/Restart recovery.
func (k *kernelPCM) WriteFrames(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	// 4 bytes per frame (S16 interleaved, any channel count the product
	// admits: 1 or 2 — the frame byte count is Channels*2, computed by the
	// caller's params; the kernel needs frames, not bytes).
	frames := uint64(len(b) / 4)
	var x uapiXferi
	x.buf = unsafe.Pointer(&b[0])
	x.frames = frames
	err := ioctl(k.fd(), ioctlWriteiFrames, unsafe.Pointer(&x))
	if err != nil {
		return 0, err
	}
	if x.result < 0 {
		// The kernel reports the error in result (tinyalsa does the same
		// check): the ioctl itself succeeded.
		return 0, syscall.Errno(-x.result)
	}
	return int(x.result), nil
}

// ReadyForWrite polls the node for POLLOUT (room in the DMA buffer). A
// timeout answers nil — the caller re-checks its stop signals and retries;
// a timeout is not an error, the stream simply has not drained.
func (k *kernelPCM) ReadyForWrite(timeout time.Duration) error {
	polls := []unix.PollFd{{Fd: int32(k.f.Fd()), Events: unix.POLLOUT}}
	n, err := unix.Poll(polls, int(timeout.Microseconds()))
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if polls[0].Revents&unix.POLLERR != 0 {
		return fmt.Errorf("alsahw: poll error on the PCM node")
	}
	return nil
}

// Close releases the node.
func (k *kernelPCM) Close() error { return k.f.Close() }

// join renders the drift message.
func join(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	s := parts[0]
	for _, p := range parts[1:] {
		s += "; " + p
	}
	return s
}
