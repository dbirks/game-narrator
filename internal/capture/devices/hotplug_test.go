package devices

// The hotplug watcher on the fake device layer (docs/spec/13 "Capture
// removed | Mark input offline … and reconnect with backoff", docs/spec/10
// "reselect by stable ID on reboot"): a fake v4l2-ctl Run closure plus a
// fabricated udev by-id tree in a temp dir stand in for the card, and the
// test unplugs, reseats and re-plugs it. The assertions are on the
// transition events, which is the contract a supervisor (and the doctor)
// consumes: one added, one removed, one reselected with the previous node
// named, and no storm around enumeration faults.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeListState is the one card the fake world contains: its list output and
// its udev by-id symlink.
type fakeListState struct {
	mu     sync.Mutex
	list   string
	idName string // the by-id symlink name; empty = no stable name
	target string // the node path the by-id symlink points at
	idDir  string
}

// setList mutates the enumeration the next poll sees; it locks because
// Watcher.Run polls from its own goroutine while the test drives a hotplug.
func (s *fakeListState) setList(list string) {
	s.mu.Lock()
	s.list = list
	s.mu.Unlock()
}

func (s *fakeListState) run(t *testing.T) func(ctx context.Context, args ...string) ([]byte, error) {
	t.Helper()
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--list-devices" {
			s.mu.Lock()
			list := s.list
			s.mu.Unlock()
			return []byte(list), nil
		}
		if len(args) >= 3 && args[0] == "-d" && args[2] == "--list-formats-ext" {
			return []byte(mjpegYUYVFormats), nil
		}
		t.Errorf("fake v4l2-ctl saw unexpected args %v", args)
		return nil, nil
	}
}

// singleCardList renders the modern --list-devices shape for one card at
// path with a capture and a metadata node (the UVC two-node layout).
func singleCardList(path string) string {
	return "Rybozen USB Video: Rybozen USB Video (usb-vif-1):\n" +
		"\tCapture devices:\n" +
		"\t\t" + path + "\n" +
		"\tMedia controller devices:\n" +
		"\t\t/dev/media0\n"
}

func (s *fakeListState) setupSymlink(t *testing.T) {
	t.Helper()
	if s.idName == "" {
		return
	}
	if s.idDir == "" {
		s.idDir = t.TempDir()
	}
	link := filepath.Join(s.idDir, s.idName)
	_ = os.Remove(link)
	if err := os.Symlink(s.target, link); err != nil {
		t.Fatalf("fabricate by-id symlink: %v", err)
	}
}

func newFakeWatcher(t *testing.T, s *fakeListState, evs *[]Event) *Watcher {
	t.Helper()
	s.setupSymlink(t)
	return &Watcher{
		Opts: Options{
			Run:     s.run(t),
			ByIDDir: s.idDir,
		},
		OnEvent: func(ev Event) { *evs = append(*evs, ev) },
	}
}

func kinds(evs []Event) []EventKind {
	out := make([]EventKind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestWatcherAddRemoveReselect(t *testing.T) {
	const idName = "usb-0000-001c1234_567890-00"
	s := &fakeListState{
		list:   singleCardList("/dev/video0"),
		idName: idName,
		target: "/dev/video0",
	}
	var evs []Event
	w := newFakeWatcher(t, s, &evs)
	ctx := context.Background()

	// The card is plugged in at boot: one added event for the capture node;
	// the metadata and media nodes are not tracked.
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := kinds(evs); len(got) != 1 || got[0] != EventAdded {
		t.Fatalf("baseline events = %v, want [added]", got)
	}
	if evs[0].Device.Path != "/dev/video0" || evs[0].Device.StableID == "" {
		t.Fatalf("added event = %+v, want the capture node with its by-id name", evs[0].Device)
	}
	if !evs[0].Device.Usable() {
		t.Fatalf("added device has no formats: the fake layer must hand out a usable node, got %v", evs[0].Device.FormatsError)
	}

	// Unchanged re-enumeration: no event.
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("stable poll: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("events after a no-change poll = %d, want still 1 (a poll is not an event)", len(evs))
	}

	// Unplug: one removed event, naming the node and its identity.
	s.list = ""
	s.setupSymlink(t) // udev drops the symlink with the card
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("unplug poll: %v", err)
	}
	if got := kinds(evs); len(got) != 2 || got[1] != EventRemoved {
		t.Fatalf("events after unplug = %v, want [added removed]", got)
	}
	if evs[1].Device.Path != "/dev/video0" {
		t.Fatalf("removed event names %q, want the node that went away", evs[1].Device.Path)
	}

	// Re-plug: /dev/videoN is reassigned by probe order, the by-id name is the
	// same. The consumer must see "the same card back, at a new node", not a
	// stranger.
	s.list = singleCardList("/dev/video5")
	s.target = "/dev/video5"
	s.setupSymlink(t)
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("replug poll: %v", err)
	}
	if got := kinds(evs); len(got) != 3 || got[2] != EventReselected {
		t.Fatalf("events after replug = %v, want [added removed reselected]", got)
	}
	ev := evs[2]
	if ev.Device.Path != "/dev/video5" || ev.Previous.Path != "/dev/video0" {
		t.Fatalf("reselected event moved %s → %s, want /dev/video0 → /dev/video5", ev.Previous.Path, ev.Device.Path)
	}
}

func TestWatcherEnumerationFaultIsNotAUnplug(t *testing.T) {
	s := &fakeListState{list: singleCardList("/dev/video2"), target: "/dev/video2"}
	var evs []Event
	run := s.run(t)
	faulty := Options{Run: run, ByIDDir: s.idDir}
	// A flapping tool: the second enumeration (the list call) fails; the
	// per-node format calls are the per-node degradation ListWith already
	// absorbs, so the fault has to hit the list to be a poll fault.
	var listCalls int
	faulty.Run = func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "--list-devices" {
			listCalls++
			if listCalls == 2 {
				// A plain error on purpose: ListWith admits
				// context.DeadlineExceeded (its own timeout case) through,
				// and the fault this test wants is the tool wedging.
				return nil, errors.New("v4l2-ctl wedged")
			}
		}
		return run(ctx, args...)
	}
	w := &Watcher{Opts: faulty, OnEvent: func(ev Event) { evs = append(evs, ev) }}
	ctx := context.Background()

	if err := w.Poll(ctx); err != nil {
		t.Fatalf("baseline poll: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != EventAdded {
		t.Fatalf("baseline events = %v, want [added]", kinds(evs))
	}
	if err := w.Poll(ctx); err == nil {
		t.Fatal("the flapping poll reported success, want the enumeration fault")
	}
	// The fault must not read as an unplug: the known set is kept, and the
	// recovery poll reports no transitions at all.
	if n := len(evs); n != 1 {
		t.Fatalf("events after a failed poll = %d, want 1 (a failed poll is not an unplug)", n)
	}
	if err := w.Poll(ctx); err != nil {
		t.Fatalf("recovery poll: %v", err)
	}
	if n := len(evs); n != 1 {
		t.Fatalf("events after recovery = %d, want 1 (the card never left)", n)
	}
}

func TestWatcherRunReactsToHotplug(t *testing.T) {
	s := &fakeListState{target: "/dev/video0"}
	var mu sync.Mutex
	var evs []Event
	w := &Watcher{
		Opts:     Options{Run: s.run(t), ByIDDir: s.idDir},
		Interval: 20 * time.Millisecond,
		OnEvent:  func(ev Event) { mu.Lock(); evs = append(evs, ev); mu.Unlock() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	snapshot := func() []Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]Event(nil), evs...)
	}

	// The card plugs in while the watcher runs.
	time.Sleep(50 * time.Millisecond)
	s.setList(singleCardList("/dev/video0"))
	deadline := time.Now().Add(5 * time.Second)
	for len(snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := snapshot(); len(got) == 0 || got[0].Kind != EventAdded {
		t.Fatalf("watcher saw no added event for a live plug-in: events = %v", kinds(got))
	}

	// The card goes away; the watcher reports it without being told to stop.
	s.setList("")
	deadline = time.Now().Add(5 * time.Second)
	for len(snapshot()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := snapshot(); len(got) < 2 || got[1].Kind != EventRemoved {
		t.Fatalf("watcher saw no removed event for a live unplug: events = %v", kinds(got))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
