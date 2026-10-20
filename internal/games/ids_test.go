package games

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewIDShape(t *testing.T) {
	id := NewID()
	if len(id) != IDLength {
		t.Fatalf("len(%q) = %d, want %d", id, len(id), IDLength)
	}
	if !ValidID(id) {
		t.Fatalf("NewID() = %q, which ValidID rejects", id)
	}
	// Crockford base32 excludes I, L, U and the hyphen so an id survives being
	// read aloud or typed by hand.
	for _, banned := range "ILUilu-" {
		if strings.ContainsRune(id, banned) {
			t.Errorf("NewID() = %q, contains the excluded character %q", id, banned)
		}
	}
	if _, err := time.Parse(time.RFC3339, IDTimeOrZero(id).Format(time.RFC3339)); err != nil {
		t.Fatalf("IDTime(%q): %v", id, err)
	}
}

func TestNewIDsAreDistinct(t *testing.T) {
	// 80 random bits make a collision in a few thousand ids vanishingly
	// unlikely; a regression that ignores the entropy would show up here.
	seen := map[string]bool{}
	for range 5000 {
		id := NewID()
		if seen[id] {
			t.Fatalf("NewID() repeated %q within one test run", id)
		}
		seen[id] = true
	}
}

func TestValidIDRejectsMalformed(t *testing.T) {
	valid := newID(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), [idRandomBytes]byte{})
	if !ValidID(valid) {
		t.Fatalf("canonical id %q rejected", valid)
	}

	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"too short", valid[:IDLength-1]},
		{"too long", valid + "0"},
		{"hyphen (uuid spelling)", "550e8400-e29b-41d4-a716-446655440000"},
		{"lowercase", strings.ToLower(valid)},
		{"excluded letter I", strings.Replace(valid, valid[15:16], "I", 1)},
		{"non-alphabetic character", strings.Replace(valid, valid[20:21], "!", 1)},
		{"timestamp wider than 48 bits", "8" + valid[1:]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ValidID(tc.id) {
				t.Errorf("ValidID(%q) = true, want false", tc.id)
			}
		})
	}
}

func TestNormalizeIDFoldsLookalikes(t *testing.T) {
	id := newID(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), [idRandomBytes]byte{'a', 'b', 0xff, 1, 2, 3, 4, 5, 6, 7})
	// The random suffix of that id contains V/0/1 characters; the folds that
	// Crockford defines must map back onto the canonical spelling.
	for _, sample := range []string{strings.ToUpper(id), id} {
		if got := NormalizeID(sample); got != id {
			t.Errorf("NormalizeID(%q) = %q, want %q", sample, got, id)
		}
	}
	// Force two positions in the random suffix onto characters that have
	// look-alikes, then check every alias spelling folds back to them.
	canonical := []byte(id)
	canonical[11] = '1'
	canonical[12] = 'V'
	want := string(canonical)
	if !ValidID(want) {
		t.Fatalf("modified id %q is not canonical", want)
	}
	for _, alias := range []string{"1", "I", "i", "L", "l"} {
		folded := []byte(want)
		folded[11] = byte(alias[0])
		if got := NormalizeID(string(folded)); got != want {
			t.Errorf("NormalizeID(%q) = %q, want %q", folded, got, want)
		}
	}
	for _, alias := range []string{"V", "U", "u"} {
		folded := []byte(want)
		folded[12] = byte(alias[0])
		if got := NormalizeID(string(folded)); got != want {
			t.Errorf("NormalizeID(%q) = %q, want %q", folded, got, want)
		}
	}
	if got := NormalizeID("short"); got != "" {
		t.Errorf("NormalizeID(short) = %q, want empty", got)
	}
	if got := NormalizeID("0000000000000000000000000!"); got != "" {
		t.Errorf("NormalizeID with a '!' = %q, want empty", got)
	}
}

// TestIDsSortByTimeAcrossMilliseconds is the ordering property the ULID choice
// buys: message lists, the job queue and event cursors page by id, which only
// works if string order is creation order.
func TestIDsSortByTimeAcrossMilliseconds(t *testing.T) {
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	entropy := [idRandomBytes]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a}

	// A *larger* random suffix on an older instant must still sort first, i.e.
	// the timestamp really is the most significant part.
	old := newID(base, [idRandomBytes]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	newer := newID(base.Add(time.Millisecond), entropy)
	if !(old < newer) {
		t.Fatalf("ids for %v and %v sorted wrong: %q !< %q", base, base.Add(time.Millisecond), old, newer)
	}

	// A spread of instants, each with random entropy, must sort lexically the
	// same way it sorts chronologically.
	var pairs []idPair
	for i := range 200 {
		at := base.Add(time.Duration(i*7%200) * time.Millisecond)
		var e [idRandomBytes]byte
		for b := range e {
			e[b] = byte((i*31 + b*17) % 256)
		}
		pairs = append(pairs, idPair{id: newID(at, e), at: at})
	}
	sortedByID := append([]idPair(nil), pairs...)
	sortPairs(sortedByID)
	sortedByTime := append([]idPair(nil), pairs...)
	sortPairsByTime(sortedByTime)
	for i := range sortedByID {
		if sortedByID[i].id != sortedByTime[i].id {
			t.Fatalf("lexical order differs from chronological order at %d: %v vs %v",
				i, sortedByID[i].id, sortedByTime[i].id)
		}
	}
}

// TestIDTimeRoundTrips checks the prefix decode the API uses to answer "when was
// this created?" from an id alone.
func TestIDTimeRoundTrips(t *testing.T) {
	for _, at := range []time.Time{
		time.UnixMilli(0).UTC().Add(time.Millisecond), // the guard that lifts 0 to 1
		time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 16, 0, 0, 0, 999_000_000, time.UTC),
		time.Date(2286, 11, 20, 17, 46, 39, 999000000, time.UTC), // near the 48-bit ceiling
	} {
		id := newID(at, [idRandomBytes]byte{})
		got, err := IDTime(id)
		if err != nil {
			t.Fatalf("IDTime(%q): %v", id, err)
		}
		want := at.Truncate(time.Millisecond)
		if want.UnixMilli() == 0 {
			want = time.UnixMilli(1).UTC() // newID's stated floor
		}
		if !got.Equal(want) {
			t.Errorf("IDTime(%q) = %v, want %v", id, got, want)
		}
	}
	if _, err := IDTime("not-a-ulid"); err == nil {
		t.Error("IDTime accepted a malformed id")
	}
}

// TestNewIDsStrictlyAscendingInTightLoop is the contract ADR-0080 puts on the
// generator: an import batch, a burst of events or a burst of job rows mints
// many ids inside a single millisecond, and every consumer that pages or lists
// by id (the message editor, exports, the speaker link UI, keyset cursors)
// relies on id order being call order. A regression that draws fresh random
// entropy on every mint shuffles a same-millisecond batch into one of N! bad
// orders, so ten thousand mints are an order of magnitude beyond the accident.
func TestNewIDsStrictlyAscendingInTightLoop(t *testing.T) {
	var prev string
	for range 10_000 {
		id := NewID()
		if !(id > prev) {
			t.Fatalf("NewID() = %q did not sort after the previous id %q", id, prev)
		}
		prev = id
	}
}

// TestNewIDsStrictlyAscendingConcurrent proves the same contract under
// contention: the counter is mutex-serialised, so each goroutine's own
// sequence is strictly ascending and no id is minted twice. A global cross-
// goroutine order is not asserted because the scheduler, not the generator,
// decides which goroutine wins the lock next.
func TestNewIDsStrictlyAscendingConcurrent(t *testing.T) {
	const (
		nGoroutines   = 16
		nPerGoroutine = 512
	)
	seqs := make([][]string, nGoroutines)
	var wg sync.WaitGroup
	for g := range nGoroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			seqs[g] = make([]string, nPerGoroutine)
			for i := range nPerGoroutine {
				seqs[g][i] = NewID()
			}
		}(g)
	}
	wg.Wait()

	seen := make(map[string]bool, nGoroutines*nPerGoroutine)
	for g := range nGoroutines {
		seq := seqs[g]
		for i, id := range seq {
			if i > 0 && !(id > seq[i-1]) {
				t.Fatalf("goroutine %d: id %d %q did not sort after %q", g, i, id, seq[i-1])
			}
			if seen[id] {
				t.Fatalf("id %q minted twice across goroutines", id)
			}
			seen[id] = true
		}
	}
}

// withMonotonicState pins the generator's high-water mark and running entropy
// (tests in this package run sequentially, so a plain lock is a store, not a
// guard against a live minter) and restores them afterwards, so the package's
// other tests still mint from the real clock.
func withMonotonicState(t *testing.T, ms uint64, entropy [idRandomBytes]byte) {
	t.Helper()
	monoMu.Lock()
	savedMS, savedEntropy := monoLastMS, monoEntropy
	monoLastMS = ms
	monoEntropy = entropy
	monoMu.Unlock()
	t.Cleanup(func() {
		monoMu.Lock()
		monoLastMS, monoEntropy = savedMS, savedEntropy
		monoMu.Unlock()
	})
}

// TestNewIDAdvancesEntropyWithinAMillisecond is the deterministic form of the
// tight-loop test: with the high-water mark pinned to the future (so the wall
// clock cannot overtake it), two mints must land in the same millisecond and
// the second must be the first's entropy plus one, big-endian.
func TestNewIDAdvancesEntropyWithinAMillisecond(t *testing.T) {
	mark := uint64(time.Now().UTC().UnixMilli()) + 1000
	e := [idRandomBytes]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0x41}
	withMonotonicState(t, mark, e)

	first := NewID()
	second := NewID()

	ePlus1 := [idRandomBytes]byte(e)
	ePlus1[9]++
	ePlus2 := [idRandomBytes]byte(ePlus1)
	ePlus2[9]++
	if first != newIDMS(mark, ePlus1) {
		t.Fatalf("first mint = %q, want the pinned entropy advanced by one (%q)", first, newIDMS(mark, ePlus1))
	}
	if second != newIDMS(mark, ePlus2) {
		t.Fatalf("second mint = %q, want the entropy advanced again (%q)", second, newIDMS(mark, ePlus2))
	}
	if !(second > first) {
		t.Fatalf("same-millisecond mints out of order: %q !< %q", first, second)
	}
	if at, err := IDTime(second); err != nil || at.UnixMilli() != int64(mark) {
		t.Fatalf("IDTime(second) = %v, %v; want the pinned millisecond %d", at, err, mark)
	}
}

// TestNewIDHoldsTheHighWaterMark is the clock-guard half of ADR-0080: when the
// wall clock steps backwards (NTP slew, VM resume) the prefix must not move
// backwards, or an id minted after the step would sort before ids already
// handed out and re-open the very flake this fix closes.
func TestNewIDHoldsTheHighWaterMark(t *testing.T) {
	before := NewID()
	mark := uint64(time.Now().UTC().UnixMilli()) + 5_000 // simulate the mark being ahead of the clock
	e := [idRandomBytes]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0x10}
	withMonotonicState(t, mark, e)

	after := NewID()
	if !(after > before) {
		t.Fatalf("id minted after a backwards clock step sorts before an earlier one: %q !< %q", after, before)
	}
	if at, err := IDTime(after); err != nil || at.UnixMilli() != int64(mark) {
		t.Fatalf("IDTime(after) = %v, %v; want the high-water mark %d, not the stepped-back clock", at, err, mark)
	}
}

// TestNewIDRefusesExhaustedEntropy covers the one state in which monotonicity
// would have to be broken: all 2^80 entropies of one millisecond already
// minted. The generator must panic rather than wrap to a suffix that sorts
// before ids it has already handed out.
func TestNewIDRefusesExhaustedEntropy(t *testing.T) {
	mark := uint64(time.Now().UTC().UnixMilli()) + 1000
	withMonotonicState(t, mark, [idRandomBytes]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("NewID() with exhausted monotonic entropy did not panic")
		}
	}()
	NewID()
}

// idPair ties an id to the instant it was minted for the ordering assertions.
type idPair struct {
	id string
	at time.Time
}

// insertion sorts keep this file free of a sort dependency for two tiny cases.
func sortPairs(p []idPair) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].id < p[j-1].id; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}

func sortPairsByTime(p []idPair) {
	for i := 1; i < len(p); i++ {
		for j := i; j > 0 && p[j].at.Before(p[j-1].at); j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}
