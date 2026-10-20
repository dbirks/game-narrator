package devices

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// EventKind is one hotplug transition the watcher observed between two
// polls.
type EventKind int

const (
	// EventAdded is a capture node the watcher has not seen before.
	EventAdded EventKind = iota
	// EventRemoved is a capture node that was present and is not enumerated
	// any more: the card (or its port) went away.
	EventRemoved
	// EventReselected is a known card back at a different node path: the
	// replug moved /dev/videoN (probe order is not stable) but the stable
	// identity says it is the same card. docs/spec/10's "reselect by stable
	// ID on reboot" is this event.
	EventReselected
)

// String names the kind for logs.
func (k EventKind) String() string {
	switch k {
	case EventAdded:
		return "added"
	case EventRemoved:
		return "removed"
	case EventReselected:
		return "reselected"
	default:
		return "unknown"
	}
}

// Event is one transition. Device is the node's current (or, for
// EventRemoved, its last-seen) state.
type Event struct {
	Kind     EventKind
	Device   Device
	Previous Device // zero for EventAdded; the state the transition moved from
}

// EventKey answers "which card is this node" across replugs. The udev
// by-id name is the only identifier that survives a replug and a reboot
// (docs/spec/10), by-path the port fallback for cards without a serial, and
// the card name plus bus info the last resort for a card that reports
// neither. A key must never be the node path: that is exactly the identity
// a replug changes.
func EventKey(d Device) string {
	switch {
	case d.StableID != "":
		return "byid:" + d.StableID
	case d.ByPath != "":
		return "bypath:" + d.ByPath
	default:
		return "name:" + d.Name + "|" + d.BusInfo
	}
}

// Watcher polls the device list on an interval and reports the capture
// nodes appearing, vanishing and reseating. It is the hotplug half of
// docs/spec/13's "Capture removed" row: the ffmpegpipe supervisor already
// re-connects the FFmpeg child for a node that dies under it (with backoff),
// and the watcher is what tells a consumer the node's *identity* came back —
// possibly at a new /dev/videoN — so a new source can be built for it.
//
// The watcher never blocks on its callback: OnEvent runs in the poll
// goroutine, so a wedged callback wedges the watch. Callers that do slow
// work fan out to their own goroutines (and bound the fan-out: a port that
// flaps must not spawn unbounded reconnect storms — the supervisor's
// backoff is the bound on the child side).
// tombstoneCap bounds the removed-identity set: a reappearing card is
// recognised as the same identity only while its removal is still recent
// enough to matter (a session that replugged its card this morning). Eight
// is far more than one appliance can unplug in an hour, and the cap is what
// keeps the set from growing with a flapping port.
const tombstoneCap = 8

type Watcher struct {
	// Opts tunes the enumeration (the zero value runs the real v4l2-ctl
	// against the real /dev; tests feed a fake Run closure — the "fake
	// device layer").
	Opts Options
	// Interval is the poll period; 0 means DefaultWatchInterval.
	Interval time.Duration
	// OnEvent receives every transition. May not block.
	OnEvent func(Event)

	mu             sync.Mutex
	known          map[string]Device // EventKey → last-seen state
	tombstones     map[string]Device // EventKey → removed state, recognised on reappear
	tombstoneOrder []string          // FIFO: the oldest removal is evicted first
	lastPollErr    error
}

// DefaultWatchInterval is the poll period the appliance uses: hotplug
// detection this slow costs nothing (enumeration is a ~20 ms exec, not a
// per-frame cost), and half a second of latency before "input offline" is
// marked is not observable by a human anyway.
const DefaultWatchInterval = 1 * time.Second

// Poll runs one enumeration and reports the transitions since the last
// Poll (or since construction). It is exported because the doctor's
// capture check is a one-shot poller and the tests drive it directly.
func (w *Watcher) Poll(ctx context.Context) error {
	if w.known == nil {
		w.known = map[string]Device{}
	}
	devs, err := ListWith(ctx, w.Opts)
	if err != nil {
		// Enumeration failed mid-watch (v4l2-ctl wedged, /dev permissions
		// revoked): report the fault, keep the last-seen set, and let the
		// next poll retry. A failed poll must not look like "everything
		// unplugged" — that is the false EventRemoved storm a flapping
		// watcher would produce.
		return fmt.Errorf("devices: poll: %w", err)
	}
	seen := map[string]bool{}
	for _, d := range devs {
		if !isCaptureNode(d.Kind) {
			continue
		}
		key := EventKey(d)
		seen[key] = true
		prev, ok := w.known[key]
		tomb, fresh := w.tombstones[key]
		switch {
		case ok:
			w.known[key] = d
			if prev.Path != d.Path {
				w.emit(Event{Kind: EventReselected, Device: d, Previous: prev})
			} // same path: refresh Formats/Errors without an event
		case fresh:
			// The identity is known from a recent removal: the card came back
			// (possibly at a new node), not a stranger. docs/spec/10's
			// "reselect by stable ID" is this branch.
			delete(w.tombstones, key)
			w.dropTombstoneKey(key)
			w.known[key] = d
			w.emit(Event{Kind: EventReselected, Device: d, Previous: tomb})
		default:
			w.known[key] = d
			w.emit(Event{Kind: EventAdded, Device: d})
		}
	}
	for key, prev := range w.known {
		if !seen[key] {
			delete(w.known, key)
			w.tombstone(key, prev)
			w.emit(Event{Kind: EventRemoved, Device: prev, Previous: prev})
		}
	}
	return nil
}

// tombstone records a removal for the reappearance recognition, evicting the
// oldest entry at the cap so a flapping port cannot grow the set.
func (w *Watcher) tombstone(key string, d Device) {
	if w.tombstones == nil {
		w.tombstones = map[string]Device{}
	}
	if _, fresh := w.tombstones[key]; fresh {
		w.tombstones[key] = d
		return
	}
	w.tombstones[key] = d
	w.tombstoneOrder = append(w.tombstoneOrder, key)
	for len(w.tombstoneOrder) > tombstoneCap {
		w.dropTombstoneKey(w.tombstoneOrder[0])
		w.tombstoneOrder = w.tombstoneOrder[1:]
	}
}

func (w *Watcher) dropTombstoneKey(key string) {
	delete(w.tombstones, key)
	for i, k := range w.tombstoneOrder {
		if k == key {
			w.tombstoneOrder = append(w.tombstoneOrder[:i], w.tombstoneOrder[i+1:]...)
			return
		}
	}
}

// Run polls until ctx is done. It returns ctx.Err(): the watcher's
// contract is to keep trying (an enumeration fault is retried on the next
// tick, and a process-lifetime supervisor has no other signal to act on).
func (w *Watcher) Run(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultWatchInterval
	}
	if err := w.Poll(ctx); err != nil {
		// The first poll is the baseline; its faults are retried on the next
		// tick, not fatal: the watcher's job is to outlive enumeration
		// faults, which is exactly when its transitions matter most.
		w.setPollErr(err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.setPollErr(w.Poll(ctx))
		}
	}
}

// LastPollErr reports the most recent Poll's fault (nil when the last poll
// enumerated cleanly), so a supervisor can tell "the watcher is up but
// v4l2-ctl is failing" from "quiet because there are no transitions".
func (w *Watcher) LastPollErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastPollErr
}

func (w *Watcher) setPollErr(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastPollErr = err
}

func (w *Watcher) emit(ev Event) {
	if w.OnEvent != nil {
		w.OnEvent(ev)
	}
}

// isCaptureNode keeps the watcher on the capture nodes only: a UVC card's
// metadata and media-controller nodes are present for the lifetime of the
// card and would otherwise generate an added/removed pair around every
// capture-node event, doubling the noise the consumer has to filter. An
// empty kind (a flat old-format --list-devices line) is kept, matching
// ListWith's own filter.
func isCaptureNode(kind string) bool {
	return kind == "" || strings.Contains(strings.ToLower(kind), "capture")
}
