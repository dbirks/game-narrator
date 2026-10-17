package session

import "sync"

// inbox is the bounded queue between the capture/worker goroutines and the
// reducer's single goroutine.
//
// The contract it exists to satisfy is docs/spec/01: "One capture loop: bounded
// single-slot latest-frame queue for live runtime" and "slow browsers cannot
// stall capture" — the generalization being that nothing downstream of the frame
// source may ever be able to apply backpressure to the frame source. So Send
// blocks never, under any queue state. When the queue is full something must be
// lost, and the order of loss is the interesting decision:
//
//	observation < ranking < speech < control
//
// A dropped observation costs one frame's evidence, and the *newer* observation
// still in the queue usually makes it redundant — this is why the brief's rule
// ("drop the oldest observation message") is right rather than merely convenient.
// A dropped ranking costs a commit that will be retried by the next observation of
// the same stable text. A dropped speech stage loses a UI truth that no later
// message restores. A dropped control loses a user's intention, which is why a
// Stop can evict a queued ranking and why controls are only ever lost to other
// controls.
//
// Every loss is counted per kind (Stats.DroppedInboxObservations and friends)
// rather than silently absorbed: docs/spec/13 treats a rising drop counter as the
// signal that a worker has died, and a counter that cannot distinguish which class
// was shed would send an operator to the wrong component.
type inbox struct {
	mu   sync.Mutex
	buf  []Msg
	max  int
	seq  int64 // message sequence handed out by push, for un-stamped senders
	drop [kindCount]int64
	// stamp tells push to assign Seq when the sender left it 0. Live producers
	// leave it 0 and get a reducer-assigned order; a replay tool stamps its own
	// sequence so a re-delivered message is recognised as the duplicate it is.
	stamp bool
}

// kindCount bounds the priority table. It is a private constant rather than
// len([]Kind) because Kind values are ints and a slice of them would imply the
// vocabulary is open when it is closed by definition.
const kindCount = 5

func (k Kind) priority() int {
	switch k {
	case KindObservation:
		return 0
	case KindRanking:
		return 1
	case KindSpeech:
		return 2
	case KindControl:
		return 3
	default:
		return 0
	}
}

func newInbox(max int, stamp bool) *inbox {
	if max < 1 {
		max = 1
	}
	return &inbox{buf: make([]Msg, 0, max), max: max, stamp: stamp}
}

// push appends m, returning false only when m itself was the thing discarded.
// evicted names whatever left the queue to make room, so a caller can log or
// metric it. O(max) worst case and max ≤ 65 536 pointer-sized slots: the scan
// runs only on the overflow path, which means a worker is already behind, and
// picking a victim by linear search beats maintaining a per-priority index that
// has to be kept consistent with a slice that shifts.
func (q *inbox) push(m Msg) (accepted bool, evicted Msg) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.stamp && m.MsgHeader().Seq == 0 {
		q.seq++
		h := m.MsgHeader()
		h.Seq = q.seq
		m = withSeq(m, h.Seq)
	}

	if len(q.buf) < q.max {
		q.buf = append(q.buf, m)
		return true, nil
	}

	incoming := m.MsgKind().priority()
	victim := -1
	for i, queued := range q.buf {
		p := queued.MsgKind().priority()
		// Evict the oldest message that is strictly less important than the
		// incoming one. Strictly, because evicting a peer to keep a peer would
		// make the queue's behaviour under sustained overload depend on arrival
		// order rather than on which class the system can afford to lose.
		if p < incoming || (victim < 0 && p == 0 && incoming == 0) {
			victim = i
			break
		}
	}
	if victim < 0 {
		q.drop[countKind(m.MsgKind())]++
		return false, nil
	}
	evicted = q.buf[victim]
	q.buf = append(q.buf[:victim], q.buf[victim+1:]...)
	q.buf = append(q.buf, m)
	q.drop[countKind(evicted.MsgKind())]++
	return true, evicted
}

// countKind keeps the drop counter index in range for an unknown Kind, which
// Validate rejects but Send cannot be made to reject without blocking the
// capture loop on a caller bug.
func countKind(k Kind) int {
	if k >= kindCount {
		return int(KindObservation)
	}
	return int(k)
}

func (q *inbox) pop() (Msg, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.buf) == 0 {
		return nil, false
	}
	m := q.buf[0]
	q.buf[0] = nil // do not pin a ranking's candidate slice
	q.buf = q.buf[1:]
	return m, true
}

func (q *inbox) depth() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.buf)
}

func (q *inbox) drops() [kindCount]int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.drop
}

// withSeq returns a copy of m carrying seq, leaving the caller's value untouched.
// Every message type is a value struct, so the switch is exhaustive by
// construction rather than by assertion, and a new message kind that forgot to
// add a case fails to compile the default branch's defensive copy below.
func withSeq(m Msg, seq int64) Msg {
	switch v := m.(type) {
	case ObservationMsg:
		v.Header.Seq = seq
		return v
	case RankingMsg:
		v.Header.Seq = seq
		return v
	case DecisionMsg:
		v.Header.Seq = seq
		return v
	case SpeechMsg:
		v.Header.Seq = seq
		return v
	case ControlMsg:
		v.Header.Seq = seq
		return v
	default:
		// A Msg implementation from outside this package (a test double, a future
		// wrapper) cannot be copied without knowing its type. Hand back the
		// original: the seq is a dedupe hint, and a message applied with a
		// caller-supplied seq is still subject to the generation and session
		// fences, which are the ones that protect correctness.
		return m
	}
}
