package alsahw

import (
	"fmt"
	"strconv"
	"strings"
)

// Params is the exact PCM hardware configuration the sink negotiates — the
// hw_params contract. The ANX7625 HDMI bridge on the UNO Q hub only
// reconfigures its audio at stream start and honours only the aplay
// reference (issue #5, measured on the board): S16_LE, 48000 Hz, 2 channels,
// period 6240 frames, 4 periods (24960-frame buffer, ~520 ms). A stream that
// starts with any other negotiated config runs but stays silent, so the
// sink never negotiates "the device's preference": it asks for these exact
// values and verifies the kernel returned them (the HW_PARAMS ioctl answer
// carries the applied values; a clamped value is an error with the
// measured result, not a silent drift).
type Params struct {
	// Format is the ALSA sample format; only S16_LE is supported (the
	// bridge's proven format; the TTS 32-bit sources are converted here).
	Format string
	// Rate is the sample rate in Hz.
	Rate int
	// Channels is the channel count.
	Channels int
	// PeriodSize is the frames between interrupts.
	PeriodSize uint32
	// PeriodCount is the periods per buffer (buffer = PeriodSize x
	// PeriodCount).
	PeriodCount uint32
}

// BufferSize is the negotiated buffer in frames.
func (p Params) BufferSize() uint32 { return p.PeriodSize * p.PeriodCount }

// validate rejects configs the sink cannot honour. The supported set is
// deliberately small: the bridge's proven format/rate, 2-channel output
// (the receiver is stereo) and a power-of-two-friendly period count the
// q6afe DMA accepts (the board accepted 4; counts of 1..16 are admitted,
// anything beyond is a misconfiguration, not a longer buffer).
func (p Params) validate() error {
	if p.Format != "S16_LE" {
		return fmt.Errorf("alsahw: unsupported format %q (the ANX7625 bridge honours only S16_LE, issue #5)", p.Format)
	}
	if p.Rate != 48000 {
		return fmt.Errorf("alsahw: unsupported rate %d (the bridge reconfigures audio only at stream start and was proven at 48000 Hz)", p.Rate)
	}
	if p.Channels < 1 || p.Channels > 2 {
		return fmt.Errorf("alsahw: channels %d out of range 1..2", p.Channels)
	}
	if p.PeriodSize == 0 || p.PeriodCount < 1 || p.PeriodCount > 16 {
		return fmt.Errorf("alsahw: period size %d x count %d out of range (count 1..16)", p.PeriodSize, p.PeriodCount)
	}
	return nil
}

// UNOQHubHDMI is the proven parameter set of the UNO Q "hub HDMI
// (ANX7625)" device profile: the exact hw_params aplay negotiated when the
// board measurement heard the tone on the receiver (issue #5).
var UNOQHubHDMI = Params{
	Format:      "S16_LE",
	Rate:        48000,
	Channels:    2,
	PeriodSize:  6240,
	PeriodCount: 4,
}

// Device is the stable ALSA identity the sink selects by: a card NAME plus
// a device number — never a numeric card index, which is exactly what a
// reboot renumbers (docs/spec/10; the same rule ADR-0074 applies to the
// PipeWire sink names).
type Device struct {
	Card   string
	Number int
}

// ParseDevice parses the ALSA device identity "hw:<card name>,<device>",
// e.g. "hw:ArduinoImolaHPH,3". The card is its NAME (the stable identity),
// not its index; "plughw:" and bare numeric forms are refused — a numeric
// identity is the defect this sink exists to avoid.
func ParseDevice(id string) (Device, error) {
	const prefix = "hw:"
	if !strings.HasPrefix(id, prefix) {
		return Device{}, fmt.Errorf("alsahw: device %q is not in the hw:<card>,<device> form", id)
	}
	rest := id[len(prefix):]
	i := strings.LastIndex(rest, ",")
	if i < 0 {
		return Device{}, fmt.Errorf("alsahw: device %q is missing the ,<device> part", id)
	}
	card := strings.TrimSpace(rest[:i])
	if card == "" {
		return Device{}, fmt.Errorf("alsahw: device %q has an empty card name", id)
	}
	if isDigits(card) {
		return Device{}, fmt.Errorf("alsahw: device %q: card %q is a numeric index; address the card by its stable name (aplay -l)", id, card)
	}
	n, err := strconv.Atoi(rest[i+1:])
	if err != nil || n < 0 || n > 999 {
		return Device{}, fmt.Errorf("alsahw: device %q has a non-numeric device part %q", id, rest[i+1:])
	}
	return Device{Card: card, Number: n}, nil
}

// isDigits reports whether s is a pure number — i.e. a card index, not a
// stable card name.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// String renders the stable identity (hw:<card>,<device>).
func (d Device) String() string { return "hw:" + d.Card + "," + strconv.Itoa(d.Number) }

// Node is the kernel PCM node for the playback substream.
func (d Device) Node() string {
	return "/dev/snd/pcmC" + d.Card + "D" + strconv.Itoa(d.Number) + "p"
}
