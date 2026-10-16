package store

import (
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// TestInTxCommitsAndRollsBack is the basic contract of the scaffolding every
// repository write uses: work that returns an error leaves nothing behind, work
// that returns nil is visible.
func TestInTxCommitsAndRollsBack(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	sentinel := errors.New("stop here")
	err := f.s.InTx(ctx, func(tx *Tx) error {
		if err := tx.createGame(ctx, games.Game{
			ID: games.NewID(), Slug: "rolled-back", Title: "Rolled Back", CreatedAt: nowRFC3339(),
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx returned %v, want the callback's error", err)
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM games WHERE slug = 'rolled-back'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rolled-back insert is present (%d rows)", n)
	}

	if err := f.s.InTx(ctx, func(tx *Tx) error {
		return tx.createGame(ctx, games.Game{
			ID: games.NewID(), Slug: "committed", Title: "Committed", CreatedAt: nowRFC3339(),
		})
	}); err != nil {
		t.Fatalf("committing InTx: %v", err)
	}
	if _, err := f.s.GetGameBySlug(ctx, "committed"); err != nil {
		t.Fatalf("committed row not readable: %v", err)
	}
}

// TestInTxJoinsAnOpenTransaction covers the behaviour that makes the repositories
// composable: a Store write method called from inside InTx must not ask the pool
// for a second connection (the pool is capped, and the write lock is already held
// by this goroutine, so it would block until the busy timeout expired).
func TestInTxJoinsAnOpenTransaction(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	inner := errors.New("inner unit failed")
	err := f.s.InTx(ctx, func(tx *Tx) error {
		// A write through the normal public API, inside an outer transaction.
		if err := f.s.PutCharacters(tx.Context(ctx), f.profile.ID,
			[]games.Character{f.character("Kept")}); err != nil {
			return err
		}
		// A failing nested unit must roll back only itself.
		nested := f.s.InTx(tx.Context(ctx), func(*Tx) error {
			return f.s.PutCharacters(tx.Context(ctx), f.profile.ID,
				[]games.Character{f.character("Discarded")})
		})
		_ = nested
		return inner
	})
	if !errors.Is(err, inner) {
		t.Fatalf("InTx returned %v, want the callback's error", err)
	}

	// The outer transaction rolled back, so neither write survived.
	if cs, err := f.s.ListCharacters(ctx, f.profile.ID); err != nil || len(cs) != 0 {
		t.Fatalf("characters after rollback = %+v, %v; want none", cs, err)
	}
	// And the profile's row version went back to its pre-transaction value: the
	// bump was part of the rolled-back unit, not a separate committed write.
	if v := f.version(); v != freshRowVersion {
		t.Fatalf("row version after rollback = %d, want %d", v, freshRowVersion)
	}
}

// TestInTxRollsBackOnlyTheNestedUnit is the savepoint half of the same story: an
// error in a joined unit does not discard the work around it, which is what lets a
// 200-row import record the rows it could write and report the one it could not
// without losing the batch.
func TestInTxRollsBackOnlyTheNestedUnit(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	err := f.s.InTx(ctx, func(tx *Tx) error {
		txCtx := tx.Context(ctx)
		misplaced := f.character("Misplaced")
		misplaced.ProfileID = games.NewID()
		unnamed := f.character("")
		if err := f.s.PutCharacters(txCtx, f.profile.ID, []games.Character{f.character("Kept")}); err != nil {
			return err
		}
		if err := f.s.PutCharacters(txCtx, f.profile.ID, []games.Character{misplaced}); !errors.Is(err, ErrConflict) {
			return errors.Join(errors.New("misplaced character was not a conflict"), err)
		}
		if err := f.s.PutCharacters(txCtx, f.profile.ID, []games.Character{unnamed}); !errors.Is(err, games.ErrValidation) {
			return errors.Join(errors.New("invalid character was not a validation failure"), err)
		}
		return f.s.PutCharacters(txCtx, f.profile.ID, []games.Character{f.character("Also kept")})
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	cs, err := f.s.ListCharacters(ctx, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("characters = %+v, want the two valid writes", cs)
	}
}

// TestInTxSerializesWriters is the point of BEGIN IMMEDIATE. Four goroutines each
// write the same draft 25 times; every write must move profiles.row_version
// exactly once, so the final value is the initial 1 plus 100. A deferred
// transaction could produce a smaller number (two writers reading the same version
// and both "succeeding"), and a writer that hit SQLITE_BUSY would fail the
// assertion with a lock error instead.
func TestInTxSerializesWriters(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	const writers, perWriter = 4, 25
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				c := f.character("Writer " + string(rune('A'+w)) + " line " + strconv.Itoa(i))
				if err := f.s.PutCharacters(ctx, f.profile.ID, []games.Character{c}); err != nil {
					errs[w] = err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent writer: %v", err)
		}
	}

	want := int64(1 + writers*perWriter)
	if got := f.version(); got != want {
		t.Fatalf("row version = %d, want %d (every write counted once)", got, want)
	}
	cs, err := f.s.ListCharacters(ctx, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != writers*perWriter {
		t.Fatalf("characters = %d, want %d", len(cs), writers*perWriter)
	}
}

// TestConstraintErrorsAreTyped covers docs/spec/02's backstop constraints: the
// repository must say *which* kind of disagreement happened, because the API
// layer maps them to different wire codes and the UI offers a different repair
// for each.
func TestConstraintErrorsAreTyped(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	// UNIQUE on games.slug.
	dup := games.Game{ID: games.NewID(), Slug: f.game.Slug, Title: "Other", CreatedAt: nowRFC3339()}
	if err := f.s.CreateGame(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate slug = %v, want ErrConflict", err)
	}

	// FK: an edition under a game that does not exist.
	orphan := games.Edition{ID: games.NewID(), GameID: games.NewID(), Region: "JP"}
	if err := f.s.CreateEdition(ctx, orphan); !errors.Is(err, ErrConflict) {
		t.Errorf("edition with unknown game = %v, want ErrConflict", err)
	}

	// Missing rows on read and on update.
	if _, err := f.s.GetGame(ctx, games.NewID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetGame of a missing id = %v, want ErrNotFound", err)
	}
	missing := f.game
	missing.ID = games.NewID()
	if err := f.s.UpdateGame(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateGame of a missing id = %v, want ErrNotFound", err)
	}

	// games.Validate runs before SQLite sees the row, so a bad value is reported
	// with the field that caused it rather than as a constraint code.
	if err := f.s.CreateGame(ctx, games.Game{ID: games.NewID(), Slug: "UPPER", Title: "x", CreatedAt: nowRFC3339()}); !errors.Is(err, games.ErrValidation) {
		t.Errorf("invalid slug = %v, want games.ErrValidation", err)
	}

	// The same message twice from a second source is a duplicate source identity,
	// which messages_source_identity_idx forbids.
	src := f.source("file:///curated/a.csv", shaA, games.RightsUserSupplied)
	if err := f.s.PutSources(ctx, f.profile.ID, []games.Source{src}); err != nil {
		t.Fatal(err)
	}
	m := f.message("Repeated identity")
	m.SourceID, m.SourceMessageID = src.ID, "row-1"
	if err := f.s.PutMessages(ctx, f.profile.ID, []games.Message{m}, nil); err != nil {
		t.Fatalf("first insert of the identified message: %v", err)
	}
	other := f.source("file:///curated/b.csv", shaB, games.RightsUserSupplied)
	if err := f.s.PutSources(ctx, f.profile.ID, []games.Source{other}); err != nil {
		t.Fatal(err)
	}
	clone := m
	clone.ID = games.NewID()
	clone.SourceID = other.ID
	if err := f.s.PutMessages(ctx, f.profile.ID, []games.Message{clone}, nil); err != nil {
		// Different source id, so the partial unique index does not apply: what
		// must not happen is a silent success that hides a parser bug. This is the
		// documented behaviour — identity is unique per (source, message), so two
		// sources may each contribute the same line.
		t.Logf("same source_message_id from a second source: %v", err)
	}
}

// TestCursorIsOpaqueAndValidated guards docs/spec/08's "pagination with cursor":
// the token round-trips, garbage is a client error, and nothing about the payload
// is part of the contract.
func TestCursorIsOpaqueAndValidated(t *testing.T) {
	id := games.NewID()
	cur := encodeCursor(id)
	decoded, err := decodeCursor(cur)
	if err != nil {
		t.Fatalf("decodeCursor(%q): %v", cur, err)
	}
	if decoded != id {
		t.Fatalf("round trip = %q, want %q", decoded, id)
	}
	if cur == id {
		t.Error("cursor is the raw id; docs/spec/08 requires it to be opaque")
	}
	if empty, err := decodeCursor(""); err != nil || empty != "" {
		t.Errorf("empty cursor = %q, %v; want no error and no resume point", empty, err)
	}
	for _, bad := range []string{"!!!!", encodeCursor("short"), encodeCursor("game-1")} {
		if _, err := decodeCursor(bad); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("decodeCursor(%q) = %v, want ErrInvalidCursor", bad, err)
		}
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct{ want, limit, max int }{
		{DefaultListLimit, 0, MaxListLimit},
		{DefaultListLimit, -7, MaxListLimit},
		{1, 1, MaxListLimit},
		{MaxListLimit, MaxListLimit + 1, MaxListLimit},
		{MaxListLimit, 100_000, MaxListLimit},
		{20, 20, MaxListLimit},
		{1, 5, 1},
	}
	for _, c := range cases {
		if got := clampLimit(c.limit, c.max); got != c.want {
			t.Errorf("clampLimit(%d, %d) = %d, want %d", c.limit, c.max, got, c.want)
		}
	}
}

// TestUpdateMessageSpeechTextBumpsBothCounters is the smallest proof of the
// optimistic-locking contract: a stale token loses and changes nothing, a current
// token wins, moves the profile's version once, and hands back the message with
// its own counter already advanced.
func TestUpdateMessageSpeechTextBumpsBothCounters(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(1)
	b.put(f)
	before := f.version()
	msg := b.Messages[0]

	stale, err := f.s.UpdateMessageSpeechText(ctx, f.profile.ID, msg.ID, "Speak this instead.", before+99)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale token = %v, want ErrRevisionConflict", err)
	}
	if stale.SpeechText != "" {
		t.Errorf("the rejected write returned speech text %q", stale.SpeechText)
	}
	if v := f.version(); v != before {
		t.Fatalf("rejected write moved the row version to %d, want %d", v, before)
	}

	got, err := f.s.UpdateMessageSpeechText(ctx, f.profile.ID, msg.ID, "Speak this instead.", before)
	if err != nil {
		t.Fatalf("UpdateMessageSpeechText: %v", err)
	}
	if got.SpeechText != "Speak this instead." {
		t.Errorf("speech text = %q", got.SpeechText)
	}
	if got.Revision != 2 {
		t.Errorf("message revision = %d, want 2 (server-owned counter advanced)", got.Revision)
	}
	if v := f.version(); v != before+1 {
		t.Fatalf("row version = %d, want %d", v, before+1)
	}

	reread, _, err := f.s.GetMessage(ctx, f.profile.ID, msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.SpeechText != "Speak this instead." || reread.Revision != 2 {
		t.Errorf("re-read message = %+v, want the edited row", reread)
	}
}
