package games

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// Identifiers for every public row in the catalogue are 26-character ULID
// strings (docs/spec/02 "SQLite schema principles": "UUIDs or ULIDs for public
// IDs"; docs/spec/08 repeats it for the API). ULID rather than UUID v4 because
// rows in these tables are browsed in creation order — a game's messages, the
// event log, the job queue — and a sortable id lets a cursor, a log line and a
// primary key all agree without a separate sequence column.
//
// The layout is the standard one: 48 bits of big-endian millisecond timestamp,
// then 80 bits of randomness, each rendered in Crockford base32 (5 bits per
// character), giving 10 + 16 = 26 characters. No third-party module: the
// algorithm is a fixed-width base conversion, and ADR-0008's dependency rule
// means a new module is a decision, not an import.
//
// Sorting: because the timestamp is the most significant part and Crockford
// base32 is order-preserving for digits before letters (0-9 then A-Z in
// alphabetical value order), plain string comparison of two ids orders them by
// creation time across millisecond boundaries. Within one millisecond the
// random suffix decides, so ids minted in the same millisecond are *not*
// monotonic — which is fine here and stated rather than hidden: every table
// that needs a total order carries its own sequence or timestamp column (events
// has seq, jobs has created_at plus priority). Monotonic-within-a-millisecond
// would require a shared counter and a clock-monotonicity guard, which is a
// larger contract than these tables need.
const (
	// IDLength is the fixed character count of an identifier.
	IDLength = 26
	// idTimeChars renders the 48-bit millisecond timestamp.
	idTimeChars = 10
	// idRandomChars renders the 80-bit random suffix.
	idRandomChars = IDLength - idTimeChars
	idRandomBytes = 10 // 80 bits
	// idTimeBits is the timestamp field width; 48 bits covers milliseconds to
	// the year 10889, so no supported clock can overflow the prefix.
	idTimeBits = 48
)

// crockfordAlphabet is Crockford base32: no I, L, U or hyphen, so an id can be
// read aloud or typed without confusing I/1, L/1 or U/V. It is written in
// value order, which is what makes lexical order equal numeric order.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// crockfordValue maps an ASCII character back to its 5-bit value, or -1 when
// the character is not in the alphabet. I/i/L/l fold into 1 and U/u into V, as
// Crockford specifies, so a transcribed id can still be read.
var crockfordValue = func() [256]int8 {
	var table [256]int8
	for i := range table {
		table[i] = -1
	}
	for v, c := range crockfordAlphabet {
		table[c] = int8(v)
	}
	// The look-alike folds are derived from the alphabet, not hard-coded: 1 is
	// what I and L are mistaken for, V is what U is mistaken for. Writing the
	// numbers out would have been wrong the first time (V is value 27, not 21)
	// and silent, since a wrong value is still a valid character.
	one := strings.IndexByte(crockfordAlphabet, '1')
	vee := strings.IndexByte(crockfordAlphabet, 'V')
	for _, c := range "ILil" {
		table[c] = int8(one)
	}
	for _, c := range "Uu" {
		table[c] = int8(vee)
	}
	return table
}()

// NewID mints a fresh identifier for the current instant.
func NewID() string {
	var entropy [idRandomBytes]byte
	// crypto/rand.Read is documented to never return an error on the platforms
	// this project supports (see internal/api's request ids for the same
	// assumption). There is still no honest value to return if it ever did — a
	// predictable id would collide silently inside a profile — so the failure
	// is raised rather than papered over.
	if _, err := rand.Read(entropy[:]); err != nil {
		panic("games: NewID: no system randomness: " + err.Error())
	}
	return newID(time.Now().UTC(), entropy)
}

// newID renders the two ULID fields. Splitting the clock and the entropy out
// makes the ordering property testable without waiting for milliseconds to
// pass.
func newID(at time.Time, entropy [idRandomBytes]byte) string {
	ms := uint64(at.UnixMilli())
	if ms == 0 {
		// A zero timestamp would sort before every real id and read as
		// "the epoch"; there is no such thing as an id for year 1970 in a
		// profile store, and time.Before(1971) is the only way a caller can
		// ask for it. Refuse loudly at the one place that knows.
		ms = 1
	}

	var out [IDLength]byte
	encodeFixed(out[:idTimeChars], ms, idTimeBits)
	encodeFixedBytes(out[idTimeChars:], entropy[:], idRandomBytes*8)
	return string(out[:])
}

// encodeFixed writes the low `bits` of v as base32 characters, most significant
// first, into dst (whose length must equal bits/5 rounded up).
func encodeFixed(dst []byte, v uint64, bits int) {
	for i := range dst {
		shift := uint(len(dst)-1-i) * 5
		var field byte
		if shift < 64 && shift < uint(bits) {
			field = byte((v >> shift) & 0x1f)
		}
		dst[i] = crockfordAlphabet[field]
	}
}

// encodeFixedBytes writes a big-endian byte array as base32 characters.
func encodeFixedBytes(dst []byte, src []byte, bits int) {
	for i := range dst {
		// Bit offset of this character's 5-bit field, counted from the most
		// significant bit of src.
		offset := i * 5
		var field byte
		for bit := 0; bit < 5; bit++ {
			pos := offset + bit
			if pos >= bits {
				break
			}
			byteIdx := pos / 8
			bitIdx := 7 - (pos % 8)
			field <<= 1
			if src[byteIdx]&(1<<uint(bitIdx)) != 0 {
				field |= 1
			}
		}
		dst[i] = crockfordAlphabet[field]
	}
}

// canonicalChar marks the 32 characters that may appear in an identifier as
// written. ValidID uses this, not crockfordValue: an id carrying I, L or U is a
// transcription of a real id, and NormalizeID exists to read that spelling, but
// a stored row must hold the canonical form or two strings would name the same
// record while the primary key disagrees with the UNIQUE index.
var canonicalChar = func() [256]bool {
	var table [256]bool
	for i := range crockfordAlphabet {
		table[crockfordAlphabet[i]] = true
	}
	return table
}()

// ValidID reports whether s is a well-formed identifier in canonical spelling.
func ValidID(s string) bool {
	if len(s) != IDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !canonicalChar[s[i]] {
			return false
		}
	}
	// The first character carries the top 2 bits of a 48-bit field: 50 bits are
	// encoded, so values above 7 in position 0 would mean a timestamp wider than
	// 48 bits, i.e. not something newID can produce.
	return crockfordValue[s[0]] <= 7
}

// NormalizeID folds the Crockford look-alikes a human may have retyped (I, L →
// 1, U → V) and uppercases. It returns "" when the result is not a valid id, so
// callers can use it directly in a guard.
func NormalizeID(s string) string {
	if len(s) != IDLength {
		return ""
	}
	out := make([]byte, IDLength)
	for i := 0; i < len(s); i++ {
		v := crockfordValue[s[i]]
		if v < 0 {
			return ""
		}
		out[i] = crockfordAlphabet[v]
	}
	return string(out)
}

// IDTime recovers the creation instant encoded in an id's prefix. It is what
// lets a log line or an event cursor be read without a database lookup.
func IDTime(id string) (time.Time, error) {
	if !ValidID(id) {
		return time.Time{}, fmt.Errorf("games: %q is not a ULID", id)
	}
	var ms uint64
	for i := 0; i < idTimeChars; i++ {
		ms = ms<<5 | uint64(crockfordValue[id[i]])
	}
	return time.UnixMilli(int64(ms)).UTC(), nil
}

// IDTimeOrZero is IDTime for log rendering, where an unparseable id should
// print as no time rather than fail the line that mentions it.
func IDTimeOrZero(id string) time.Time {
	at, err := IDTime(id)
	if err != nil {
		return time.Time{}
	}
	return at
}
