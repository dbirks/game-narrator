package api

// The in-process Idempotency-Key store's own guarantees (docs/spec/08
// "Conventions", ADR-0017 requirement 2). The HTTP-level behaviour of keyed
// creates is asserted in the flow tests; this file is about the store: that it is
// *bounded* (the appliance has 4 GB and runs for months), that records *expire*,
// that a claim is atomic, and that a key is scoped to the principal that used it —
// because a cached 201 names a resource id, and handing that to another user is a
// leak rather than a convenience.

import (
	"crypto/sha256"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
)

func TestIdempotencyStoreReplaysConfirmsAndScopes(t *testing.T) {
	st := newIdempotencyStore()
	digest := sha256.Sum256([]byte("body"))
	other := sha256.Sum256([]byte("different body"))

	first := st.claim("u1", "POST /api/v1/games", "k", digest)
	if first.kind != idemNew {
		t.Fatalf("a fresh key = %v, want new", first.kind)
	}
	// A key claimed but not yet completed is an in-flight sibling, not a replay:
	// the honest answer is "this is being done right now", which an idempotent
	// client handles by retrying.
	if inFlight := st.claim("u1", "POST /api/v1/games", "k", digest); inFlight.kind != idemConflict {
		t.Error("an un-finalised reservation was reported as a replay")
	}
	first.reserve(http.StatusCreated, []byte(`{"id":"01GAME"}`))

	replay := st.claim("u1", "POST /api/v1/games", "k", digest)
	if replay.kind != idemReplay || replay.status != http.StatusCreated ||
		string(replay.body) != `{"id":"01GAME"}` {
		t.Fatalf("replay = %+v, want the stored 201 verbatim", replay)
	}
	if clash := st.claim("u1", "POST /api/v1/games", "k", other); clash.kind != idemConflict {
		t.Error("the same key with a different body was not reported as a conflict")
	}

	// Same key string, another principal: a different request. Reading u1's cached
	// response would disclose the id u1 just created.
	stranger := st.claim("u2", "POST /api/v1/games", "k", digest)
	if stranger.kind != idemNew {
		t.Error("one principal's key was visible to another principal")
	}
	stranger.release()

	// Same key, another route: also a different request.
	elsewhere := st.claim("u1", "POST /api/v1/games/01/editions", "k", digest)
	if elsewhere.kind != idemNew {
		t.Error("a key crossed routes")
	}
	elsewhere.release()

	// A released reservation leaves no trace, so a corrected retry with the same
	// key after a 4xx can still succeed.
	failed := st.claim("u1", "POST /api/v1/games", "retry-me", digest)
	failed.release()
	again := st.claim("u1", "POST /api/v1/games", "retry-me", digest)
	if again.kind != idemNew {
		t.Error("a released reservation still occupied the key")
	}
	again.release()
}

func TestIdempotencyStoreExpiresAndEvictsWithinItsBounds(t *testing.T) {
	st := newIdempotencyStore()
	now := time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	digest := sha256.Sum256([]byte("body"))

	// TTL: 24 h is the documented window. A retried POST with a day-old key is a
	// new request, and keeping responses forever would be an unbounded promise.
	if maxIdempotencyEntries != 10_000 || idempotencyTTL != 24*time.Hour {
		t.Fatalf("the documented bound/TTL changed: %d / %s", maxIdempotencyEntries, idempotencyTTL)
	}
	early := st.claim("u1", "POST /api/v1/games", "ttl", digest)
	early.reserve(http.StatusCreated, []byte(`{}`))

	now = now.Add(idempotencyTTL - time.Minute)
	if got := st.claim("u1", "POST /api/v1/games", "ttl", digest); got.kind != idemReplay {
		t.Errorf("inside the TTL the key read as %v, want replay", got.kind)
	}
	now = now.Add(2 * time.Minute)
	expired := st.claim("u1", "POST /api/v1/games", "ttl", digest)
	if expired.kind != idemNew {
		t.Errorf("past the TTL the key read as %v, want a fresh claim", expired.kind)
	}
	expired.reserve(http.StatusCreated, []byte(`{"id":"x"}`))
	if live := st.liveTTLCount(now); live != 1 {
		t.Errorf("liveTTLCount = %d, want the expired record replaced by one live one", live)
	}

	// Bound: fed more than maxIdempotencyEntries distinct keys, the store keeps the
	// most recent and forgets the oldest rather than growing with traffic.
	for i := 0; i < maxIdempotencyEntries+500; i++ {
		decision := st.claim("u1", "POST /api/v1/games", "k"+strconv.Itoa(i), digest)
		decision.reserve(http.StatusCreated, []byte(`{}`))
	}
	if size := st.size(); size != maxIdempotencyEntries {
		t.Fatalf("the store retained %d records, want the bound of %d", size, maxIdempotencyEntries)
	}
	// The oldest are the ones gone, and the newest still replay.
	if got := st.claim("u1", "POST /api/v1/games", "k0", digest); got.kind != idemNew {
		t.Error("the oldest record survived eviction while newer ones were retained")
	} else {
		got.release()
	}
	newest := "k" + strconv.Itoa(maxIdempotencyEntries+499)
	if got := st.claim("u1", "POST /api/v1/games", newest, digest); got.kind != idemReplay {
		t.Errorf("the newest key read as %v, want replay", got.kind)
	}
}

// TestConcurrentIdenticalCreatesClaimTheKeyOnce is why the lookup and the
// reservation happen under one lock: if two POSTs that arrive together can both be
// told "new", the header has bought nothing and the second row is already written.
func TestConcurrentIdenticalCreatesClaimTheKeyOnce(t *testing.T) {
	st := newIdempotencyStore()
	digest := sha256.Sum256([]byte("body"))

	const racers = 16
	var wg sync.WaitGroup
	owner := make(chan bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := st.claim("u1", "POST /api/v1/games", "race", digest)
			if d.kind == idemNew {
				d.reserve(http.StatusCreated, []byte(`{"id":"01ONLY"}`))
				owner <- true
				return
			}
			owner <- false
		}()
	}
	wg.Wait()
	close(owner)

	var new_, refused int
	for wasNew := range owner {
		if wasNew {
			new_++
		} else {
			refused++
		}
	}
	if new_ != 1 {
		t.Errorf("%d of %d concurrent identical requests were told the key was new, want exactly 1", new_, racers)
	}
	if refused != racers-1 {
		t.Errorf("%d racers were neither owner nor refused: the claim is not atomic", refused)
	}
	// Every racer was refused or owned the key: the in-flight sibling answers
	// "retry" (a conflict), which is what an idempotent client does anyway, rather
	// than blocking a request path on a lock held through a database write.
	if got := st.claim("u1", "POST /api/v1/games", "race", digest); got.kind != idemReplay {
		t.Errorf("after the owner finished, the key read as %v, want replay", got.kind)
	}
}

// TestIdempotencyKeysAreScopedPerPrincipalAndRouteOnTheWire drives the same rules
// through HTTP: two curators choosing "one" do not read each other's response, and
// one key string does not cover two different routes.
func TestIdempotencyKeysAreScopedPerPrincipalAndRouteOnTheWire(t *testing.T) {
	e := newCatalogEnv(t)

	mine := e.requireStatus(e.post(auth.RoleCurator, "/api/v1/games",
		`{"slug":"scoped-one","title":"Scoped One"}`, "Idempotency-Key", "one"),
		http.StatusCreated, "the curator's create under key \"one\"")
	theirsRec := e.post(auth.RoleAdmin, "/api/v1/games",
		`{"slug":"scoped-two","title":"Scoped Two"}`, "Idempotency-Key", "one")
	theirs := e.requireStatus(theirsRec, http.StatusCreated, "another principal's create under the same key")
	if mine["id"] == theirs["id"] {
		t.Fatal("two principals under one key got one resource: the second create was swallowed as a replay")
	}
	if theirsRec.Header().Get("Idempotency-Replayed") == "true" {
		t.Error("one principal's key replayed another principal's response")
	}

	// The same key on another route is another request.
	editionRec := e.post(auth.RoleCurator,
		"/api/v1/games/"+mine["id"].(string)+"/editions",
		`{"region":"us","language":"en","revision":"rev1"}`, "Idempotency-Key", "one")
	edition := e.requireStatus(editionRec, http.StatusCreated, "an edition create reusing the key on its own route")
	if edition["id"] == mine["id"] {
		t.Error("a key crossed routes and replayed the game's response")
	}

	// And the same key + same body on the same route by the same principal is the
	// documented replay: the same bytes, the same Location, visibly marked.
	original := e.post(auth.RoleCurator, "/api/v1/games",
		`{"slug":"scoped-three","title":"Scoped Three","description":"d"}`, "Idempotency-Key", "three")
	e.requireStatus(original, http.StatusCreated, "the create to be replayed")
	replay := e.post(auth.RoleCurator, "/api/v1/games",
		`{"slug":"scoped-three","title":"Scoped Three","description":"d"}`, "Idempotency-Key", "three")
	if replay.Code != http.StatusCreated {
		t.Fatalf("the replay = %d, want the original 201 (body %s)", replay.Code, replay.Body.String())
	}
	if replay.Body.String() != original.Body.String() {
		t.Errorf("the replay is not the original body:\n %s\n %s", replay.Body.String(), original.Body.String())
	}
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("the replay did not identify itself")
	}
	if replay.Header().Get("Location") != original.Header().Get("Location") {
		t.Errorf("Location differs: %q vs %q", replay.Header().Get("Location"), original.Header().Get("Location"))
	}

	// A key over the documented length ceiling is refused before it can become a
	// map key of unbounded size.
	longKey := ""
	for len(longKey) <= maxIdempotencyKeyLen {
		longKey += "0123456789"
	}
	requireProblem(t, e.post(auth.RoleCurator, "/api/v1/games",
		`{"slug":"long-key","title":"Long Key"}`, "Idempotency-Key", longKey),
		http.StatusBadRequest, codeInvalidParameter)

	// A keyed create that failed validation releases its key, so the corrected
	// retry is not stuck behind the refusal.
	if refused := e.post(auth.RoleCurator, "/api/v1/games", `{"slug":"","title":"No Slug"}`,
		"Idempotency-Key", "four"); refused.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an invalid create = %d, want 422 (body %s)", refused.Code, refused.Body.String())
	}
	if fixed := e.post(auth.RoleCurator, "/api/v1/games", `{"slug":"scoped-four","title":"Fixed"}`,
		"Idempotency-Key", "four"); fixed.Code != http.StatusCreated {
		t.Errorf("the corrected retry under the same key = %d, want 201 (body %s)", fixed.Code, fixed.Body.String())
	}
}
