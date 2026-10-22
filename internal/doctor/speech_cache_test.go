package doctor

import (
	"testing"
)

// The speech.cache_disk row (ADR-0159, issue #49): the free space on the data
// partition and the unreachable share the prune would take.

// The statfs seam the row reads through, so the suite never measures the real
// host's partition (docs/spec/13: the diagnostic suite runs against seams).
var realStatfsSpace = statfsSpace

func stubStatfs(freeBytes int64) (int64, int64, bool) {
	return freeBytes, int64(512) << 30, true
}

func TestSpeechCacheDiskFreeSpaceBelowReserveIsDegraded(t *testing.T) {
	orig := statfsSpace
	statfsSpace = func(string) (int64, int64, bool) { return stubStatfs(1024) }
	defer func() { statfsSpace = orig }()

	h := newFakeHost(t)
	c := h.check(h.run(), "speech.cache_disk")
	mustStatus(t, c, StatusDegraded)
	if c.Remedy == "" {
		t.Fatalf("speech.cache_disk remedy is empty: %s", c.Remedy)
	}
}

func TestSpeechCacheDiskUnknownStatfsIsOk(t *testing.T) {
	orig := statfsSpace
	statfsSpace = func(string) (int64, int64, bool) { return 0, 0, false }
	defer func() { statfsSpace = orig }()

	h := newFakeHost(t)
	mustStatus(t, h.check(h.run(), "speech.cache_disk"), StatusOK)
}

func TestSpeechCacheDiskWithinReserveAndNoEntriesIsOk(t *testing.T) {
	h := newFakeHost(t)
	c := h.check(h.run(), "speech.cache_disk")
	mustStatus(t, c, StatusOK)
	if c.Group != "Speech" {
		t.Fatalf("speech.cache_disk group = %q, want Speech", c.Group)
	}
	// The fake host's data directory holds no cache entries, so the share is not
	// reportable and the row answers from the free-space half alone.
	if c.Summary == "" || c.Duration <= 0 {
		t.Fatalf("speech.cache_disk row must carry a summary and a duration: %+v", c)
	}
}

// TestSpeechCacheDiskRowIsRegistered pins the row's presence in the fixed
// report order: every check the run lists, exactly once, with the storage rows
// it neighbours (ADR-0159 added it between storage.database and network.listen).
func TestSpeechCacheDiskRowIsRegistered(t *testing.T) {
	h := newFakeHost(t)
	rep := h.run()
	found := false
	for _, c := range rep.Checks {
		if c.ID == "speech.cache_disk" {
			found = true
			if c.Group != "Speech" || c.Summary == "" {
				t.Errorf("speech.cache_disk row shape: %+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("the doctor report has no speech.cache_disk row: %v", ids(rep))
	}
}
