package capture

import (
	"encoding/binary"
	"time"
)

// procStart is the instant this process started, used as the zero point of the
// monotonic capture clock.
//
// time.Now() in Go carries a monotonic reading, and time.Since(aTime) subtracts
// monotonic readings, so the duration below is immune to NTP steps, DST and
// manual date changes — which is exactly the property docs/spec/01 invariant 1
// demands of a clock that the dialogue tracker measures its stability window
// against. Nanoseconds since process start also stays inside int64 for ~292
// years of uptime and is small enough to print, which an absolute epoch ns is
// not.
var procStart = time.Now()

// MonotonicNS returns nanoseconds since process start on Go's monotonic clock.
// Live sources stamp frames with this; replay sources never call it, because
// their timestamps came from the recording.
func MonotonicNS() int64 { return int64(time.Since(procStart)) }

// put32 writes v little-endian into b[:4].
func put32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
