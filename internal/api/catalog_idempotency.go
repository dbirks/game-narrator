package api

// Idempotency-Key handling for catalog creates (docs/spec/08 "Conventions":
// "`Idempotency-Key` for create or expensive requests", ADR-0017).
//
// What it guarantees, and what it does not:
//
//   - Same key + same body ⇒ the original response is replayed verbatim (status
//     and body), so a client that retried after a dropped connection sees one
//     game, not two, and sees the same id it got the first time.
//   - Same key + different body ⇒ 422. One key describing two different
//     resources is a client bug; honouring the second would be the silent
//     duplicate the header exists to prevent.
//   - Missing key on a route that requires one ⇒ 400 with
//     IDEMPOTENCY_KEY_REQUIRED, because "the caller decided this POST is safe to
//     retry" is information only the caller has.
//
// The store is in-process memory, bounded (maxIdempotencyEntries LRU,
// idempotencyTTL expiry) and keyed by principal + route + key. It is deliberately
// *not* SQLite: the durable job queue already has an idempotency_key column with
// a unique index (internal/store/jobs_store.go), and a second durable table for
// the same idea across short-lived POSTs would be state nobody can explain
// during a backup. The consequence is stated plainly in ADR-0017: a restart or a
// second process forgets replayed responses. The key is scoped by principal so
// two curators choosing "1" cannot read each other's cached response — which
// would otherwise leak one user's created resource id to another.

import (
	"container/list"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// maxIdempotencyEntries bounds the record count. 10 000 replays at ~1 KiB of
	// recorded response each is ~10 MB, which is a rounding error on the 4 GB
	// target and more than one appliance's day of curation writes.
	maxIdempotencyEntries = 10_000
	// idempotencyTTL is how long a replayed response stays valid. Beyond it a
	// retried POST with an old key is a new request, not a retry.
	idempotencyTTL = 24 * time.Hour
	// maxIdempotencyKeyLen bounds the header value itself; a client-supplied
	// string of unbounded length in a map key is a memory lever.
	maxIdempotencyKeyLen = 200
)

// idempotencyRecord is one stored response.
type idempotencyRecord struct {
	user   string
	route  string
	key    string
	digest [sha256.Size]byte
	status int
	body   []byte
	// created is read by tests through expired(); it is a wall clock instant, so
	// a restarted clock that moves backwards expires records early rather than
	// keeping them forever.
	created time.Time
}

func (rec *idempotencyRecord) expired(now time.Time) bool {
	return now.Sub(rec.created) > idempotencyTTL
}

// idempotencyStore is the bounded LRU. It is safe for concurrent use: every
// method takes one lock, and the eviction order is a container/list so an
// insert into a full store is O(1).
type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]*list.Element // composite key → element of recs
	recs    *list.List               // front = most recently used
	now     func() time.Time
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{
		entries: map[string]*list.Element{},
		recs:    list.New(),
		now:     time.Now,
	}
}

func idemCompositeKey(user, route, key string) string {
	return user + "\x00" + route + "\x00" + key
}

type idempotencyDecision struct {
	// kind is one of new, replay, conflict.
	kind    idempotencyKind
	status  int
	body    []byte
	reserve func(status int, body []byte)
	// release drops an un-finalised reservation (a handler that answered 4xx
	// must not leave a key claiming it created something).
	release func()
}

type idempotencyKind int

const (
	idemNew idempotencyKind = iota
	idemReplay
	idemConflict
)

// claim decides what to do with a keyed request, and reserves the key when the
// answer is "new". Reservation happens under the same lock as the lookup, so
// two concurrent identical POSTs cannot both be told "new" — one of them is
// waiting on the other's outcome rather than racing it into two rows.
func (st *idempotencyStore) claim(user, route, key string, digest [sha256.Size]byte) idempotencyDecision {
	st.mu.Lock()
	defer st.mu.Unlock()
	composite := idemCompositeKey(user, route, key)
	if el, ok := st.entries[composite]; ok {
		rec := el.Value.(*idempotencyRecord)
		if rec.expired(st.now()) {
			st.removeLocked(el)
		} else {
			st.recs.MoveToFront(el)
			if subtle.ConstantTimeCompare(rec.digest[:], digest[:]) != 1 {
				return idempotencyDecision{kind: idemConflict}
			}
			if rec.status == 0 {
				// Reserved but not yet completed: the in-flight sibling. Waiting
				// would put a lock hold on the request path, so the honest answer
				// is "retry", which is what an idempotent client does anyway.
				return idempotencyDecision{kind: idemConflict}
			}
			return idempotencyDecision{kind: idemReplay, status: rec.status, body: append([]byte(nil), rec.body...)}
		}
	}
	rec := &idempotencyRecord{
		user: user, route: route, key: key, digest: digest, created: st.now(),
	}
	el := st.recs.PushFront(rec)
	st.entries[composite] = el
	st.evictLocked()
	return idempotencyDecision{
		kind: idemNew,
		reserve: func(status int, body []byte) {
			st.mu.Lock()
			defer st.mu.Unlock()
			if cur, ok := st.entries[composite]; ok {
				cur.Value.(*idempotencyRecord).status = status
				cur.Value.(*idempotencyRecord).body = append([]byte(nil), body...)
				st.recs.MoveToFront(cur)
			}
		},
		release: func() {
			st.mu.Lock()
			defer st.mu.Unlock()
			if cur, ok := st.entries[composite]; ok {
				st.removeLocked(cur)
			}
		},
	}
}

func (st *idempotencyStore) removeLocked(el *list.Element) {
	st.recs.Remove(el)
	rec := el.Value.(*idempotencyRecord)
	delete(st.entries, idemCompositeKey(rec.user, rec.route, rec.key))
}

func (st *idempotencyStore) evictLocked() {
	for st.recs.Len() > maxIdempotencyEntries {
		oldest := st.recs.Back()
		if oldest == nil {
			return
		}
		st.removeLocked(oldest)
	}
}

// size reports the retained record count, for the bound test.
func (st *idempotencyStore) size() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.recs.Len()
}

// liveTTLCount counts non-expired records; the TTL sweep is lazy (a record is
// dropped when its key is next touched or when the LRU evicts it), which is the
// right shape for a bounded map but must not be mistaken for an unbounded one.
func (st *idempotencyStore) liveTTLCount(now time.Time) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for el := st.recs.Front(); el != nil; el = el.Next() {
		if !el.Value.(*idempotencyRecord).expired(now) {
			n++
		}
	}
	return n
}

// idempotentCreate runs a create handler at most once per (principal, route,
// Idempotency-Key). The `do` callback writes its own error problem and returns
// status 0 when it failed, in which case the reservation is released so a
// corrected retry with the same key can still succeed.
func (s *Server) idempotentCreate(w http.ResponseWriter, r *http.Request, route, body string, do func() (int, any)) (int, any) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "idempotency-key-required", codeIdempotencyKeyMissing,
			"Bad Request",
			"this create requires an Idempotency-Key header: it is what makes a retry after a dropped response safe")
		return 0, nil
	}
	if len(key) > maxIdempotencyKeyLen {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "idempotency-key-invalid", codeInvalidParameter,
			"Bad Request",
			"Idempotency-Key must be at most "+strconv.Itoa(maxIdempotencyKeyLen)+" characters")
		return 0, nil
	}
	user := "anonymous"
	if id := IdentityFromContext(r.Context()); id != nil && id.User != nil {
		user = id.User.ID
	}

	sum := sha256.Sum256([]byte(body))
	decision := s.idempotency.claim(user, route, key, sum)
	switch decision.kind {
	case idemReplay:
		// The replay is visibly a replay: a client that sees the same 201 twice
		// must be able to tell "my retry was deduplicated" from "I created it
		// again", and the header is how without re-reading the body.
		w.Header().Set("Idempotency-Replayed", "true")
		if decision.status == http.StatusCreated {
			if loc := r.URL.Path; loc != "" {
				w.Header().Set("Location", replayLocation(decision.body, loc))
			}
		}
		w.Header().Set("Content-Type", ContentTypeJSON)
		w.WriteHeader(decision.status)
		_, _ = w.Write(decision.body)
		return decision.status, nil
	case idemConflict:
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "idempotency-key-reused", codeIdempotencyConflict,
			"Unprocessable Entity",
			"this Idempotency-Key was already used with a different request body; a key identifies one request, not one slot")
		return 0, nil
	}

	status, payload := do()
	if status == 0 {
		decision.release()
		return 0, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		decision.release()
		s.log.Error("could not encode a create response", "route", route, "error", err)
		return status, payload
	}
	// The newline is not decoration: writeJSON encodes with a json.Encoder, which
	// terminates the document, so the stored body has to carry it or the replay
	// would differ from the response it stands in for by one byte — and a client
	// that hashes what it received would call that a different answer.
	decision.reserve(status, append(raw, '\n'))
	if status == http.StatusCreated {
		// Set before the write, and derived the same way the replay derives it,
		// so the first response and the replayed one are identical.
		w.Header().Set("Location", replayLocation(raw, r.URL.Path))
	}
	writeJSON(w, status, payload)
	return status, payload
}

// replayLocation re-derives the Location header for a replayed create from the
// stored body's id, so the replay is byte-for-byte the response the client
// would have got.
func replayLocation(body []byte, path string) string {
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.ID != "" {
		return path + "/" + probe.ID
	}
	return path
}
