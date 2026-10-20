// Package alsahw is the ALSA-hardware AudioSink (ADR-0111, issue #5): it
// opens a named ALSA PCM node by stable identity (hw:<card name>,<device> —
// never a numeric index), negotiates the exact hw_params the ANX7625 HDMI
// bridge on the UNO Q hub proved it needs (S16_LE / 48000 / 2ch / period
// 6240 x 4, the aplay reference of the board measurement), resamples the
// TTS WAV in-process to that format, and keeps the stream open between
// utterances with silence so the receiver's ELD lock survives (the
// session.suspend-timeout-seconds=0 semantics of the WirePlumber rule,
// in-process).
//
// The data plane is the kernel's RW-interleaved transfer
// (SNDRV_PCM_IOCTL_WRITEI_FRAMES with struct snd_xferi) — the same path
// aplay and tinyalsa take; there is no alsa-lib and no subprocess, so the
// sink has no user-namespace and no per-utterance process of its own. The
// ABI structs below are copied from include/uapi/sound/asound.h (64-bit
// userspace layout); the compile-time size asserts pin them, and the
// hwsmoke gate re-verifies the negotiated parameters against
// /proc/asound before any audibility claim (the board measurement, issue
// #5, is the acceptance fixture).
package alsahw

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// ── ALSA UAPI constants (include/uapi/sound/asound.h) ─────────────────────

// Access types (snd_pcm_access_t).
const (
	accessMmapInterleaved = 0
	accessRWInterleaved   = 3
)

// Sample formats (snd_pcm_format_t).
const (
	formatS16LE = 2
)

// Stream states (snd_pcm_state_t).
const (
	stateOpen         = 0
	stateSetup        = 1
	statePrepared     = 2
	stateRunning      = 3
	stateXRun         = 4
	stateDraining     = 5
	statePaused       = 6
	stateSuspended    = 7
	stateDisconnected = 8
)

// hw_param indices (snd_pcm_hw_param_t).
const (
	hwParamAccess     = 0
	hwParamFormat     = 1
	hwParamChannels   = 10
	hwParamRate       = 11
	hwParamPeriodSize = 13
	hwParamBufferSize = 17
)

// snd_interval bitfield words: openmin:1 openmax:1 integer:1 empty:1.
const (
	intervalOpenMin = 1 << 0
	intervalOpenMax = 1 << 1
	intervalInteger = 1 << 2
	intervalEmpty   = 1 << 3
)

// sw_params protocol version: SNDRV_PROTOCOL_VERSION(2, 0, 12).
const swProto2_0_12 = (2 << 16) | (0 << 8) | 12

// sw_params timestamp type: SNDRV_PCM_TSTAMP_TYPE_MONOTONIC.
const swTstampMonotonic = 1

// ioctl request numbers, computed with the 64-bit _IOC encoding
// (dir<<30 | size<<16 | type<<8 | nr) of include/uapi/sound/asound.h.
// The struct sizes are the 64-bit userspace layouts (time_t == 8 bytes,
// no y2038 pad), which aplay on the same 64-bit target uses.
var (
	// _IOR('A', 0x01, struct snd_pcm_info)
	ioctlInfo = ioc(2, 'A', 0x01, 284)
	// _IOWR('A', 0x11, struct snd_pcm_hw_params)
	ioctlHWParams = ioc(3, 'A', 0x11, 1504)
	// _IOWR('A', 0x13, struct snd_pcm_sw_params)
	ioctlSWParams = ioc(3, 'A', 0x13, 136)
	// _IOR('A', 0x20, struct snd_pcm_status)
	ioctlStatus = ioc(2, 'A', 0x20, 140)
	// _IO('A', 0x40 / 0x42 / 0x43)
	ioctlPrepare = ioc(0, 'A', 0x40, 0)
	ioctlStart   = ioc(0, 'A', 0x42, 0)
	ioctlDrop    = ioc(0, 'A', 0x43, 0)
	// _IOW('A', 0x50, struct snd_xferi)
	ioctlWriteiFrames = ioc(1, 'A', 0x50, 24)
)

func ioc(dir, typ byte, nr, size uint) uintptr {
	return uintptr(uint32(dir)<<30 | uint32(size)<<16 | uint32(typ)<<8 | uint32(nr))
}

// ── the UAPI structs, 64-bit userspace layout ─────────────────────────────

// snd_mask: 256 parameter bits.
type uapiMask [32]uint32

// snd_interval: min/max plus the openmin/openmax/integer/empty flags.
type uapiInterval struct {
	min   uint32
	max   uint32
	flags uint32
}

// snd_pcm_hw_params.
type uapiHWParams struct {
	flags     uint32
	masks     [4]uapiMask
	mres      [5]uapiMask
	intervals [12]uapiInterval
	ires      [9]uapiInterval
	rmask     uint32
	cmask     uint32
	info      uint32
	msbits    uint32
	rateNum   uint32
	rateDen   uint32
	fifoSize  uint64
	sync      [16]byte
	reserved  [48]byte
}

// snd_pcm_sw_params.
type uapiSWParams struct {
	tstampMode     int32
	periodStep     uint32
	sleepMin       uint32
	availMin       uint64
	xferAlign      uint64
	startThreshold uint64
	stopThreshold  uint64
	silenceThresh  uint64
	silenceSize    uint64
	boundary       uint64
	proto          uint32
	tstampType     uint32
	reserved       [56]byte
}

// uapiTimespec models struct timespec (tv_sec, tv_nsec): 16 bytes with the
// 8-byte alignment the C struct has (a plain [16]byte would sit unaligned
// and drift the whole status struct — the compile-time size assert catches
// exactly this).
type uapiTimespec [2]uint64

// snd_pcm_status (the 64-bit userspace form): state plus the 4 bytes the C
// compiler pads to align trigger_tstamp to struct timespec, the timestamps,
// the pointers, and reserved[52-2*sizeof(struct timespec)].
type uapiPCMStatus struct {
	state         int32
	triggerTstamp uapiTimespec
	tstamp        uapiTimespec
	applPtr       uint64
	hwPtr         uint64
	delay         int64
	avail         uint64
	availMax      uint64
	overrange     uint64
	suspended     int32
	tstampData    uint32
	audioTstamp   uapiTimespec
	driverTstamp  uapiTimespec
	accuracy      uint32
	reserved      [20]byte
}

// snd_xferi: the RW-interleaved data transfer (tinyalsa's pcm_rw_transfer
// uses the same struct for exactly this purpose).
type uapiXferi struct {
	result int64
	buf    unsafe.Pointer
	frames uint64
}

// Compile-time layout pins: the ABI is only as good as these asserts plus
// the hwsmoke /proc/asound re-verification. If a kernel change or a
// re-target (32-bit, y2038) drifts the layout, the build fails here.
var (
	_ = [1]byte([unsafe.Sizeof(uapiHWParams{}) - 1503]byte{})
	_ = [1]byte([unsafe.Sizeof(uapiSWParams{}) - 135]byte{})
	_ = [1]byte([unsafe.Sizeof(uapiPCMStatus{}) - 151]byte{})
	_ = [1]byte([unsafe.Sizeof(uapiXferi{}) - 23]byte{})
)

// ioctl runs one kernel ioctl against the PCM node fd.
func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
