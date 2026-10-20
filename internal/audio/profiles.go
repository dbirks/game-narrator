package audio

import (
	"strconv"
)

// Profile pins the proven hw_params of one output device whose negotiation
// the OS default gets wrong (issue #5: the UNO Q hub HDMI, ANX7625 bridge —
// PipeWire's default S24_LE + MMAP + 10 ms negotiation is silent on that
// bridge; only the aplay-equivalent config is audible into the receiver).
//
// A profiled device is surfaced in the device list under its stable ALSA
// identity (hw:<card name>,<device> — never an index, docs/spec/10) with
// the kind the profile says (hdmi for the hub HDMI), and the alsahw sink
// negotiates the profile's exact params for it. A device without a profile
// is not addressable through the alsahw sink: the point of the profile is
// the PROVEN config, and inventing one is how the receiver goes silent.
type Profile struct {
	// CardName is the stable ALSA card name (the aplay -l name); never a
	// numeric index — an index is what a reboot renumbers, and that is the
	// defect the stable identity exists to avoid.
	CardName string
	// DeviceNumber is the subdevice on that card (the hub HDMI out is 3 on
	// the UNO Q; the 0 out is the internal SoC path, a different device).
	DeviceNumber int
	// Label is the human-facing name the device row carries in the UI.
	Label string
	// Proven is the exact hw_params the measurement proved audible
	// (issue #5, 2026-10-20 board run): S16_LE, 48 kHz, 2 channels,
	// period 6240 x 4 (buffer 24960 frames = 520 ms — the receiver's ELD
	// lock survives that idle, which is why the sink keeps the stream open
	// between utterances and feeds it silence).
	Format      string
	Rate        int
	Channels    int
	PeriodSize  int
	PeriodCount int
}

// DeviceID is the stable identity the app selects the device by
// (hw:<card name>,<device>).
func (p Profile) DeviceID() string {
	return "hw:" + p.CardName + "," + strconv.Itoa(p.DeviceNumber)
}

// DefaultKeepAlive is the sink's idle-silence cadence for a profiled
// stream: short enough that the hardware pointer keeps advancing between
// the receiver's ELD refreshes (the bridge reconfigures audio at stream
// start and keeps the lock while the DMA is fed), long enough that the
// silence cost is one period per interval, not a continuous write.
// 200 ms sits well inside the 520 ms buffer the proven period config
// carries.
const DefaultKeepAlive int64 = 200 // milliseconds

// UNOQHubHDMI is the UNO Q's hub HDMI output: USB-C DisplayPort through
// the ANX7625 bridge chip into the hub's HDMI out (verified into a Pioneer
// VSX-823; its ELD is what the sink reports). The ALSA path is the
// default route — MultiMedia4 → SEC_MI2S_RX → ANX7625 → USB-C DP → hub
// HDMI — which is why the mixer route (SEC_MI2S_RX Audio Mixer
// MultiMedia4 on, DISPLAY_PORT_RX … off) must stay untouched; the Display
// Port dai-link does not exist in the qcm2290 device tree.
var UNOQHubHDMI = Profile{
	CardName:     "ArduinoImolaHPH",
	DeviceNumber: 3,
	Label:        "UNO Q hub HDMI (ANX7625)",
	Format:       "S16_LE",
	Rate:         48000,
	Channels:     2,
	PeriodSize:   6240,
	PeriodCount:  4,
}

// Profiles is the known profile set, one entry per device whose
// negotiation must be pinned to the proven config.
var Profiles = []Profile{UNOQHubHDMI}

// ProfileForDevice answers the question the sink wiring asks: does this
// device id carry a proven config, and which one?
func ProfileForDevice(id string) (Profile, bool) {
	for _, p := range Profiles {
		if p.DeviceID() == id {
			return p, true
		}
	}
	return Profile{}, false
}

// ProfileForCard answers the question the lister asks by card name (aplay
// -l carries names, not hw: identities): the first profile whose card this
// is.
func ProfileForCard(card string) (Profile, bool) {
	for _, p := range Profiles {
		if p.CardName == card {
			return p, true
		}
	}
	return Profile{}, false
}
