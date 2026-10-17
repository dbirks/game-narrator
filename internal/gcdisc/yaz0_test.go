package gcdisc

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// yaz0Header builds the 16-byte Yaz0 header ("Yaz0", big-endian size,
// 8 reserved bytes) for a declared output size.
func yaz0Header(size uint32) []byte {
	var h [16]byte
	copy(h[:4], "Yaz0")
	binary.BigEndian.PutUint32(h[4:8], size)
	return h[:]
}

// TestYaz0LiteralRoundTrip decompresses literal-only streams of various
// lengths (reusing the yaz0Literal encoder from rarc_test.go) and
// checks the bytes round-trip.
func TestYaz0LiteralRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 7, 8, 9, 63, 64, 65, 1000} {
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i*7 + 1)
		}
		got, err := Yaz0Decompress(bytes.NewReader(yaz0Literal(data)), 0)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("n=%d: round-trip mismatch (got %d bytes, want %d)", n, len(got), n)
		}
	}
}

// TestYaz0BackReference covers back-references built by hand.
func TestYaz0BackReference(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		// Group control 0xC0: literal, literal, then a back-reference.
		// Ref: N=2 (length 4), R=(0x00<<8|0x01)=1, distance R+1=2.
		// "ab" + 4 copies from distance 2 = "ababab" (6 bytes).
		var buf bytes.Buffer
		buf.Write(yaz0Header(6))
		buf.Write([]byte{0xC0, 'a', 'b', 0x20, 0x01})
		got, err := Yaz0Decompress(&buf, 0)
		if err != nil {
			t.Fatalf("back-reference: %v", err)
		}
		if string(got) != "ababab" {
			t.Fatalf("got %q, want %q", got, "ababab")
		}
	})

	t.Run("extended length", func(t *testing.T) {
		// N=0 extends the length: L = B3+0x12 = 18. After "ab", copy 18
		// bytes at distance 2; declared size 20 stops the copy at 18.
		var buf bytes.Buffer
		buf.Write(yaz0Header(20))
		buf.Write([]byte{0xC0, 'a', 'b', 0x00, 0x01, 0x00})
		got, err := Yaz0Decompress(&buf, 0)
		if err != nil {
			t.Fatalf("extended back-reference: %v", err)
		}
		want := "abababababababababab"
		if string(got) != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// TestYaz0Bounds checks the decompression safety bounds: the
// decompression-bomb cap, the caller maxOut limit, truncation, and
// back-references that run past the current output.
func TestYaz0Bounds(t *testing.T) {
	t.Run("declared size above cap", func(t *testing.T) {
		// A declared size one byte above Yaz0MaxOut must be rejected
		// before any stream byte is consumed (decompression-bomb guard).
		data := append(yaz0Header(Yaz0MaxOut+1), 0xFF)
		_, err := Yaz0Decompress(bytes.NewReader(data), 0)
		if err == nil || !strings.Contains(err.Error(), "cap") {
			t.Fatalf("got %v, want cap error", err)
		}
	})

	t.Run("exact cap allowed", func(t *testing.T) {
		// Exactly Yaz0MaxOut is legal; a short stream must fail with a
		// truncation error, not the bomb error.
		data := append(yaz0Header(Yaz0MaxOut), 0xFF)
		_, err := Yaz0Decompress(bytes.NewReader(data), 0)
		if err == nil || strings.Contains(err.Error(), "cap") {
			t.Fatalf("got %v, want truncation error", err)
		}
	})

	t.Run("output overruns maxOut", func(t *testing.T) {
		// Declared size 8 with maxOut 4: the declared output would
		// overrun the caller's limit.
		data := yaz0Literal(make([]byte, 8))
		_, err := Yaz0Decompress(bytes.NewReader(data), 4)
		if err == nil || !strings.Contains(err.Error(), "maxOut") {
			t.Fatalf("got %v, want maxOut error", err)
		}
	})

	t.Run("truncated header", func(t *testing.T) {
		if _, err := Yaz0Decompress(bytes.NewReader(yaz0Header(10)[:10]), 0); err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("truncated stream", func(t *testing.T) {
		// Declared size 10, only five literals available.
		var buf bytes.Buffer
		buf.Write(yaz0Header(10))
		buf.Write([]byte{0xFF, 'a', 'b', 'c', 'd', 'e'})
		if _, err := Yaz0Decompress(&buf, 0); err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("truncated back-reference", func(t *testing.T) {
		// Control byte declares a back-reference but the B1 byte is
		// missing.
		data := append(yaz0Header(8), 0x00, 'a')
		if _, err := Yaz0Decompress(bytes.NewReader(data), 0); err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("back-reference distance past output", func(t *testing.T) {
		// One literal ("x"), then a ref with R=1 (distance 2) that runs
		// past the one output byte so far.
		var buf bytes.Buffer
		buf.Write(yaz0Header(8))
		buf.Write([]byte{0x80, 'x', 0x20, 0x01})
		_, err := Yaz0Decompress(&buf, 0)
		if err == nil || !strings.Contains(err.Error(), "distance") {
			t.Fatalf("got %v, want distance error", err)
		}
	})

	t.Run("stops at declared size", func(t *testing.T) {
		// Declared size 2 but four literals in the stream: the decoder
		// must stop exactly at the declared size, never overrun it.
		var buf bytes.Buffer
		buf.Write(yaz0Header(2))
		buf.Write([]byte{0xFF, 'a', 'b', 'c', 'd'})
		got, err := Yaz0Decompress(&buf, 0)
		if err != nil || string(got) != "ab" {
			t.Fatalf("got %q, %v; want %q, nil", got, err, "ab")
		}
	})
}

// FuzzYaz0 feeds arbitrary bytes to the decoder: it must never panic
// and any successful output must respect the Yaz0MaxOut cap.
func FuzzYaz0(f *testing.F) {
	f.Add(yaz0Literal([]byte("hello world")))
	f.Add(yaz0Literal(nil))
	f.Add(yaz0Header(Yaz0MaxOut + 1))
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := Yaz0Decompress(bytes.NewReader(data), 0)
		if err != nil {
			return
		}
		if int64(len(out)) > Yaz0MaxOut {
			t.Fatalf("output %d bytes exceeds cap %d", len(out), Yaz0MaxOut)
		}
	})
}
