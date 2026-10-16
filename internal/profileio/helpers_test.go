package profileio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/store"
)

// Test fixtures are the generic adapter's own synthetic scripts (see
// internal/games/adapter/generic/testdata) plus text generated here from an
// invented vocabulary. No real game text appears anywhere in this package, and
// nothing writes outside t.TempDir() (AGENTS.md rule 5).

// env is one appliance: a database and an asset store in a temp directory, with
// the adapters this build links.
type env struct {
	t      *testing.T
	ctx    context.Context
	dir    string
	store  *store.Store
	assets *assets.Local
	deps   Deps
}

func newEnv(t *testing.T, timeout ...time.Duration) *env {
	t.Helper()
	e := &env{t: t, ctx: testCtx(t, timeout...), dir: t.TempDir()}
	e.open()
	return e
}

// open (re)opens the database and asset store under e.dir. Restart tests call it
// again after closeAll: docs/spec/02's "restart tests demonstrate persistent
// games/voices/cache" is about what survives a process boundary, and a fresh
// handle is the cheapest honest way to cross one.
func (e *env) open() {
	e.t.Helper()
	db, err := store.Open(e.ctx, filepath.Join(e.dir, store.FileName))
	if err != nil {
		e.t.Fatalf("store.Open: %v", err)
	}
	if _, err := db.Migrate(e.ctx); err != nil {
		e.t.Fatalf("Migrate: %v", err)
	}
	as, err := assets.NewLocal(filepath.Join(e.dir, "assets"))
	if err != nil {
		e.t.Fatalf("assets.NewLocal: %v", err)
	}
	reg := adapter.NewRegistry()
	if err := generic.Register(reg); err != nil {
		e.t.Fatalf("generic.Register: %v", err)
	}
	e.store, e.assets = db, as
	e.deps = Deps{Store: db, Assets: as, Adapters: reg}
}

// newIn returns a second, independent appliance under its own temp dir — the
// "re-import into a fresh DB" of the portability acceptance criterion.
func newIn(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: testCtx(t), dir: t.TempDir()}
	e.open()
	return e
}

func (e *env) closeAll() {
	e.t.Helper()
	if err := e.store.Close(); err != nil {
		e.t.Fatalf("store.Close: %v", err)
	}
}

// game, edition and draft build the catalogue rows a profile needs, with titles
// invented for this test.
func (e *env) game(slug, title string) games.Game {
	e.t.Helper()
	g := games.Game{
		ID: games.NewID(), Slug: slug, Title: title, Platform: "gamecube-like",
		UserTags: []byte(`["synthetic"]`), CreatedAt: nowStamp(),
	}
	if err := g.Validate(); err != nil {
		e.t.Fatalf("fixture game invalid: %v", err)
	}
	if err := e.store.CreateGame(e.ctx, g); err != nil {
		e.t.Fatalf("CreateGame: %v", err)
	}
	return g
}

func (e *env) edition(g games.Game, region, language string) games.Edition {
	e.t.Helper()
	aspect := 1.3333
	row := games.Edition{
		ID: games.NewID(), GameID: g.ID, Region: region, Language: language,
		Revision: "demo", Locale: language + "_" + region, DisplayAspect: &aspect,
	}
	if err := row.Validate(); err != nil {
		e.t.Fatalf("fixture edition invalid: %v", err)
	}
	if err := e.store.CreateEdition(e.ctx, row); err != nil {
		e.t.Fatalf("CreateEdition: %v", err)
	}
	return row
}

func (e *env) draft(editionID string) games.Profile {
	e.t.Helper()
	p, err := e.store.CreateDraftProfile(e.ctx, editionID)
	if err != nil {
		e.t.Fatalf("CreateDraftProfile: %v", err)
	}
	return p
}

func testCtx(t *testing.T, timeout ...time.Duration) context.Context {
	t.Helper()
	limit := 120 * time.Second
	if len(timeout) > 0 {
		limit = timeout[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	t.Cleanup(cancel)
	return ctx
}

// fixturePath locates a generic adapter fixture. The two-game portability test
// reads the importer's own golden files rather than a copy of them, so the
// profiles under test are the ones `make test-replay` also covers; reading a
// neighbouring package's testdata is not modifying it.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "games", "adapter", "generic", "testdata", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fixture %s: %v", path, err)
	}
	return path
}

func openFixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open(fixturePath(t, name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// sha256Bytes is the digest a test compares against an import's reported one.
func sha256Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

//
// ── the synthetic 10 000-message corpus (docs/spec/16 M1) ───────────────────
//

// inventedVocabulary is a word list made up for this test. Nothing in it is
// anyone's game script; it exists so the corpus is clearly synthetic and so the
// search queries below can be exact substrings of text nobody else wrote.
var inventedVocabulary = []string{
	"brack", "veljin", "quorath", "snell", "marivane", "toth", "glimmer", "ashfen",
	"pluvek", "dorn", "yarrow", "skelling", "cinderfall", "marrow", "vesk", "thraik",
	"ondrel", "kell", "parrow", "fluvane", "sable", "merrow", "gath", "nimmel",
	"torchfen", "elvane", "skarry", "bruggle", "wath", "perivane", "duskwell", "harrow",
	"jellin", "cragmoor", "veskine", "tarrow", "mizzle", "brannock", "lothe", "quillfen",
	"shadowmere", "fenwick", "galloway", "stroven", "kettle", "barrowmere", "delfane",
	"woad", "sirn", "yelkin",
}

// syntheticMessage builds one deterministic line of invented dialogue. The
// generator is a linear congruential shuffle over the vocabulary, seeded by the
// row number, so the corpus is identical on every machine and in every run
// (docs/spec/12: a fixture has a reproducible identity, not a plausible one).
// Word counts vary from 3 to 40 so the index holds both a page of a sign and a
// long speech, and the lengths are not all the same modulo any small number.
func syntheticMessage(seed uint64) string {
	var (
		b       strings.Builder
		wordLen = int(3 + seed%38)
	)
	state := seed*6364136223846793005 + 1442695040888963407
	for i := 0; i < wordLen; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		b.WriteString(inventedVocabulary[state%uint64(len(inventedVocabulary))])
	}
	out := b.String()
	return strings.ToUpper(out[:1]) + out[1:] + "."
}

// syntheticCorpus returns n messages and their pages, addressed to profileID and
// sourceID. Source message ids are `syn-<n>`, which is the identity a re-import
// would reconcile against.
func syntheticCorpus(profileID, sourceID string, n int) ([]games.Message, []games.MessagePage) {
	now := nowStamp()
	msgs := make([]games.Message, 0, n)
	pages := make([]games.MessagePage, 0, n/4)
	kinds := []games.TextKind{games.TextDialogue, games.TextSign, games.TextItem, games.TextMenu, games.TextTutorial, games.TextSystem, games.TextOther}
	for i := 0; i < n; i++ {
		text := syntheticMessage(uint64(i) + 1)
		id := games.NewID()
		msgs = append(msgs, games.Message{
			ID: id, ProfileID: profileID, SourceID: sourceID,
			SourceMessageID: fmt.Sprintf("syn-%06d", i),
			OriginalText:    text, NormalizedText: strings.ToLower(text),
			SpeechText: text, Language: "en", TextKind: kinds[i%len(kinds)],
			AttributionStatus: games.AttributionUnknown,
			Revision:          1, CreatedAt: now, UpdatedAt: now,
		})
		if i%4 == 0 {
			pages = append(pages, games.MessagePage{
				MessageID: id, Ordinal: 1, PageText: text,
				SourcePageID: fmt.Sprintf("syn-%06d-p1", i),
			})
		}
	}
	return msgs, pages
}

// Message ids come from games.NewID: the corpus's *content* is deterministic
// (every line is a function of its row number), which is the property a fixture
// needs, while ids only need to be unique and valid.
//
// ── comparison helpers ──────────────────────────────────────────────────────
//

// profileContentFor reads a revision back out of the store and exports it, for
// tests that compare "what the database holds" against "what the file said".
func profileContentFor(t *testing.T, e *env, profileID string) Manifest {
	t.Helper()
	m, err := Export(e.ctx, e.store, profileID, ExportOptions{})
	if err != nil {
		t.Fatalf("Export(%s): %v", profileID, err)
	}
	return m
}
