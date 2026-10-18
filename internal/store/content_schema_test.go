package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/speech"
)

// Tests for 0002_content.sql and 0003_jobs_events.sql: docs/spec/02 "Entity
// catalog" + "SQLite schema principles" as the contract, docs/spec/12
// "Contract" mode as the gate (blank-DB migration, every CHECK and FK
// asserted, FTS5 trigram behaviour, cascade behaviour).

// contentFixture holds the parent rows the content tables need, in the order
// 0001 → 0002 requires them.
type contentFixture struct {
	profileID string
	sourceID  string
	charID    string
	sceneID   string
}

// seedContent creates game/edition/profile/source plus one character and one
// scene, leaving them empty of messages so each test controls its own rows.
func seedContent(t *testing.T, db *sql.DB) contentFixture {
	t.Helper()
	insertGame(t, db, "game-content", "synthetic-content")
	insertEdition(t, db, "ed-content", "game-content")
	insertProfile(t, db, "prof-content", "ed-content", 1, "draft", nil)
	insertSource(t, db, "src-content", "prof-content", "user_file", shaA)
	mustExec(t, db,
		`INSERT INTO characters (id, profile_id, name, status, created_at)
		 VALUES ('char-hero', 'prof-content', 'Hero', 'confirmed', ?)`, nowRFC3339())
	mustExec(t, db,
		`INSERT INTO scenes (id, profile_id, name, created_at)
		 VALUES ('scene-deck', 'prof-content', 'Deck', ?)`, nowRFC3339())
	return contentFixture{
		profileID: "prof-content",
		sourceID:  "src-content",
		charID:    "char-hero",
		sceneID:   "scene-deck",
	}
}

// insertSecondProfile adds a second profile revision (own edition and source)
// so queries can be shown to be profile-scoped.
func insertSecondProfile(t *testing.T, db *sql.DB) {
	t.Helper()
	insertEditionIn(t, db, "ed-two", "game-content", "EU")
	insertProfile(t, db, "prof-two", "ed-two", 1, "draft", nil)
	insertSource(t, db, "src-two", "prof-two", "user_file", shaB)
}

// q renders a Go string as a SQL string literal. The bad-row tables embed
// values as raw SQL (rather than binding parameters) so that one helper can
// express NULL, numbers and text uniformly.
func q(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// row builds "INSERT INTO table (cols) VALUES (vals)" from a baseline plus
// single-column overrides. Every "this row must be rejected" case below differs
// from a row known to be accepted in exactly one place — otherwise the case
// could pass because of an unrelated typo in the fixture.
type row struct {
	table string
	cols  []string
	vals  map[string]string
}

func (r row) with(pairs ...string) row {
	vals := make(map[string]string, len(r.vals)+len(pairs)/2)
	for k, v := range r.vals {
		vals[k] = v
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		vals[pairs[i]] = pairs[i+1]
	}
	return row{table: r.table, cols: r.cols, vals: vals}
}

func (r row) insert() string {
	cols := make([]string, 0, len(r.cols))
	vals := make([]string, 0, len(r.cols))
	for _, c := range r.cols {
		v, ok := r.vals[c]
		if !ok {
			panic("row " + r.table + ": no value for column " + c)
		}
		cols = append(cols, c)
		vals = append(vals, v)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", r.table,
		strings.Join(cols, ", "), strings.Join(vals, ", "))
}

func messageBaseline(id string) row {
	return row{
		table: "messages",
		cols: []string{
			"id", "profile_id", "source_id", "source_message_id", "original_text",
			"normalized_text", "speech_text", "language", "text_kind", "scene_id", "speaker_id",
			"attribution_status", "attribution_confidence", "markup_json", "attribution_evidence",
			"flags", "revision", "created_at", "updated_at",
		},
		vals: map[string]string{
			"id": q(id), "profile_id": q("prof-content"), "source_id": q("src-content"),
			"source_message_id": q("sm-" + id), "original_text": q("Hello there"),
			"normalized_text": q("hello there"), "speech_text": q("Hello there!"),
			"language": q("en"), "text_kind": q("dialogue"), "scene_id": q("scene-deck"),
			"speaker_id": q("char-hero"), "attribution_status": q("confirmed"),
			"attribution_confidence": "0.9", "markup_json": q("[]"),
			"attribution_evidence": q("[]"), "flags": q("{}"), "revision": "1",
			"created_at": q(nowRFC3339()), "updated_at": q(nowRFC3339()),
		},
	}
}

func characterBaseline(id string) row {
	return row{
		table: "characters",
		cols: []string{
			"id", "profile_id", "name", "aliases", "visual_descriptors", "personality_summary",
			"role", "verified_source_refs", "portrait_asset_id", "status", "created_at",
		},
		vals: map[string]string{
			"id": q(id), "profile_id": q("prof-content"), "name": q("Hero"),
			"aliases": q(`["Link"]`), "visual_descriptors": q("[]"),
			"personality_summary": q("brave"), "role": q("player_one"),
			"verified_source_refs": q(`["src-content"]`), "portrait_asset_id": q(""),
			"status": q("confirmed"), "created_at": q(nowRFC3339()),
		},
	}
}

func sceneBaseline(id string) row {
	return row{
		table: "scenes",
		cols: []string{
			"id", "profile_id", "name", "aliases", "region_fingerprint",
			"visual_anchors", "progress_json", "created_at",
		},
		vals: map[string]string{
			"id": q(id), "profile_id": q("prof-content"), "name": q("Deck"),
			"aliases": q("[]"), "region_fingerprint": q("fp-1"), "visual_anchors": q("[]"),
			"progress_json": q("{}"), "created_at": q(nowRFC3339()),
		},
	}
}

func regionBaseline(id string) row {
	return row{
		table: "regions",
		cols: []string{
			"id", "profile_id", "scene_id", "label", "region_type", "detect_method",
			"rect_x", "rect_y", "rect_w", "rect_h", "min_aspect", "max_aspect",
			"min_width_px", "min_height_px", "ocr_settings", "mask_asset_ids", "reliability",
			"revision", "updated_at",
		},
		vals: map[string]string{
			"id": q(id), "profile_id": q("prof-content"), "scene_id": q("scene-deck"),
			"label": q("main textbox"), "region_type": q("textbox"), "detect_method": q("manual"),
			"rect_x": "0.1", "rect_y": "0.7", "rect_w": "0.8", "rect_h": "0.2",
			"min_aspect": "NULL", "max_aspect": "NULL", "min_width_px": "NULL", "min_height_px": "NULL",
			"ocr_settings": q("{}"), "mask_asset_ids": q("[]"), "reliability": "0.95",
			"revision": "1", "updated_at": q(nowRFC3339()),
		},
	}
}

func pageBaseline(messageID string, ordinal string) row {
	return row{
		table: "message_pages",
		cols: []string{
			"message_id", "ordinal", "source_page_id", "page_text",
			"visible_prefixes", "display_controls", "timing_json",
		},
		vals: map[string]string{
			"message_id": q(messageID), "ordinal": ordinal, "source_page_id": q("pg-" + ordinal),
			"page_text": q("Hello there"), "visible_prefixes": q(`["hello"]`),
			"display_controls": q(`["A"]`), "timing_json": q(`{"ms_per_char":40}`),
		},
	}
}

func edgeBaseline(from, to string) row {
	return row{
		table: "message_edges",
		cols: []string{
			"from_id", "to_id", "edge_type", "condition_expr", "source_id",
			"evidence", "confidence", "created_at",
		},
		vals: map[string]string{
			"from_id": q(from), "to_id": q(to), "edge_type": q("next"),
			"condition_expr": q("flags.has_herosuit"), "source_id": q("src-content"),
			"evidence": q("[]"), "confidence": "1", "created_at": q(nowRFC3339()),
		},
	}
}

func assertionBaseline(id string) row {
	return row{
		table: "speaker_assertions",
		cols: []string{
			"id", "profile_id", "message_id", "character_id", "method",
			"asserted_at", "evidence", "posterior", "reviewer",
		},
		vals: map[string]string{
			"id": q(id), "profile_id": q("prof-content"), "message_id": q("msg-a"),
			"character_id": q("char-hero"), "method": q("on_screen_label"),
			"asserted_at": q(nowRFC3339()), "evidence": q("[]"), "posterior": "0.8",
			"reviewer": q(""),
		},
	}
}

func correctionBaseline(id string) row {
	return row{
		table: "corrections",
		cols: []string{
			"id", "profile_id", "kind", "target_type", "target_id", "message_id",
			"before_value", "after_value", "reason", "evidence", "actor", "status",
			"reviewer", "created_at", "reviewed_at",
		},
		vals: map[string]string{
			"id": q(id), "profile_id": q("prof-content"), "kind": q("speaker"),
			"target_type": q("message"), "target_id": q("msg-a"), "message_id": q("msg-a"),
			"before_value": q("Old Name"), "after_value": q("Hero"), "reason": q("wrong label"),
			"evidence": q("[]"), "actor": q("curator"), "status": q("proposed"),
			"reviewer": q(""), "created_at": q(nowRFC3339()), "reviewed_at": "NULL",
		},
	}
}

func jobBaseline(id string) row {
	return row{
		table: "jobs",
		cols: []string{
			"id", "kind", "state", "priority", "payload_json", "result_json", "parent_id",
			"resume_token", "attempts", "max_attempts", "run_after", "trace_id",
			"created_at", "updated_at", "idempotency_key",
		},
		vals: map[string]string{
			"id": q(id), "kind": q("import.parse"), "state": q("queued"), "priority": "0",
			"payload_json": q(`{"profile_id":"prof-content"}`), "result_json": "NULL",
			"parent_id": "NULL", "resume_token": q("stage-2"), "attempts": "1", "max_attempts": "3",
			"run_after": "NULL", "trace_id": q("trace-1"), "created_at": q(nowRFC3339()),
			"updated_at": q(nowRFC3339()), "idempotency_key": q("key-" + id),
		},
	}
}

func attemptBaseline(jobID, attempt string) row {
	return row{
		table: "job_attempts",
		cols:  []string{"job_id", "attempt", "started_at", "finished_at", "outcome", "error"},
		vals: map[string]string{
			"job_id": q(jobID), "attempt": attempt, "started_at": q("2026-01-01T00:00:00Z"),
			"finished_at": q("2026-01-01T00:00:30Z"), "outcome": q("failed"), "error": q("boom"),
		},
	}
}

func eventBaseline(id string) row {
	return row{
		table: "events",
		cols: []string{
			"event_id", "session_id", "name", "trace_id", "causation_id",
			"occurred_at", "schema_version", "data_json",
		},
		vals: map[string]string{
			"event_id": q(id), "session_id": q("sess-1"), "name": q("frame.observed"),
			"trace_id": q("trace-1"), "causation_id": "NULL", "occurred_at": q(nowRFC3339()),
			"schema_version": "1", "data_json": q(`{"frame":1}`),
		},
	}
}

// seedEdgeParents adds the rows the baselines above point at: four messages
// (msg-p, msg-e1, msg-e2, msg-a) and two jobs (job-a, job-base).
func seedEdgeParents(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, id := range []string{"msg-p", "msg-e1", "msg-e2", "msg-a"} {
		mustExec(t, db, messageBaseline(id).insert())
	}
	for _, id := range []string{"job-a", "job-base"} {
		mustExec(t, db, jobBaseline(id).insert())
	}
}

//
// ── Migration shape ─────────────────────────────────────────────────────────
//

// TestMigrationAppliesContentFileToBlankDatabase is the docs/spec/12 Contract
// gate: blank file → the schema version this build ships, every promised object
// present, and a second run that changes nothing. It also proves SQLite *accepts*
// the FTS5 sync triggers, which is the half of the story splitSQL cannot tell on its
// own.
func TestMigrationAppliesContentFileToBlankDatabase(t *testing.T) {
	s := openIn(t)
	ctx := testCtx(t)

	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if want := MigrationNames(); strings.Join(applied, ",") != strings.Join(want, ",") {
		t.Fatalf("applied %v, want %v", applied, want)
	}
	if v, err := s.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, LatestSchemaVersion())
	}

	// Checked by name: a silently misnamed or missing index/trigger would
	// otherwise look like a successful migration.
	wantObjects := map[string]string{
		"messages":                        "table",
		"message_pages":                   "table",
		"message_edges":                   "table",
		"characters":                      "table",
		"speaker_assertions":              "table",
		"scenes":                          "table",
		"regions":                         "table",
		"corrections":                     "table",
		"messages_fts":                    "table",
		"jobs":                            "table",
		"job_attempts":                    "table",
		"events":                          "table",
		"messages_fts_after_insert":       "trigger",
		"messages_fts_after_update":       "trigger",
		"messages_fts_after_delete":       "trigger",
		"messages_profile_scene_kind_idx": "index",
		"messages_profile_normalized_idx": "index",
		"messages_profile_speaker_idx":    "index",
		"messages_source_identity_idx":    "index",
		"message_edges_from_id_idx":       "index",
		"events_session_seq_idx":          "index",
		"events_trace_seq_idx":            "index",
		"jobs_claim_idx":                  "index",
		"jobs_parent_id_idx":              "index",
		"corrections_profile_status_idx":  "index",
		"speaker_assertions_message_idx":  "index",
	}
	for name, typ := range wantObjects {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE name = ? AND type = ?`, name, typ).Scan(&n); err != nil {
			t.Fatalf("look up %s: %v", name, err)
		}
		if n == 0 {
			t.Errorf("%s %q missing after migrate", typ, name)
		}
	}

	// message_pages(message_id, ordinal) is promised UNIQUE. SQLite implements
	// that as an unnamed auto-index, so it is read through PRAGMA rather than
	// guessed at by name (TestUniquenessConstraints asserts the behaviour).
	var uniquePages int
	rows, err := s.db.QueryContext(ctx, `SELECT "unique", origin FROM pragma_index_list('message_pages')`)
	if err != nil {
		t.Fatalf("index_list(message_pages): %v", err)
	}
	for rows.Next() {
		var isUnique, origin string
		if err := rows.Scan(&isUnique, &origin); err != nil {
			t.Fatal(err)
		}
		if isUnique == "1" {
			uniquePages++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if uniquePages == 0 {
		t.Error("message_pages has no UNIQUE index on (message_id, ordinal)")
	}

	// FTS5 addresses rows by rowid, so `messages` must keep its implicit one:
	// WITHOUT ROWID would not merely be slower, it would make the triggers
	// write to an index that cannot be read back.
	var withoutRowid int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name = 'messages' AND sql LIKE '%WITHOUT ROWID%'`).
		Scan(&withoutRowid); err != nil {
		t.Fatal(err)
	}
	if withoutRowid != 0 {
		t.Error("messages declared WITHOUT ROWID; FTS5 content_rowid='rowid' needs the implicit rowid")
	}

	if applied, err := s.Migrate(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("second Migrate applied %v, %v; want nothing", applied, err)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatalf("IntegrityCheck after migrate: %v", err)
	}
}

// TestNewTablesSurviveBackupRoundTrip: the queue and the event log have to live
// in the same file Backup copies, or a restore silently loses them.
func TestNewTablesSurviveBackupRoundTrip(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	mustExec(t, s.db, jobBaseline("snap-job").insert())
	mustExec(t, s.db, eventBaseline("snap-event").insert())

	// The speech-cache index row (migration 0011) goes through the index's own
	// write path: it is the row a restart LoadAlls, so the backup/restore
	// round trip (AGENTS.md rule 10) must carry it like the jobs/events rows.
	if err := NewSpeechCacheIndex(s).Put(speechTestKey(0), speech.Entry{
		SHA256: "aa" + tail62('a'), Size: 4096, Duration: 1_000_000,
		SampleRate: 16000, QA: speech.QAPassed,
	}); err != nil {
		t.Fatalf("seed speech_cache: %v", err)
	}

	dest := DBPath(t.TempDir())
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	backup := openPath(t, dest)
	if v, err := backup.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("backup SchemaVersion = %d, %v; want %d", v, err, LatestSchemaVersion())
	}
	if n := countRows(t, backup.db, "jobs"); n != 1 {
		t.Fatalf("backup jobs = %d, want 1", n)
	}
	if n := countRows(t, backup.db, "events"); n != 1 {
		t.Fatalf("backup events = %d, want 1", n)
	}
	if n := countRows(t, backup.db, "speech_cache"); n != 1 {
		t.Fatalf("backup speech_cache = %d, want 1", n)
	}
	// Restoring the backup reproduces a usable index, not just a row count:
	// a fresh index over the backup LoadAlls the seeded entry.
	loaded, err := NewSpeechCacheIndex(backup).LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll on the backup: %v", err)
	}
	if e, ok := loaded[speechTestKey(0)]; !ok || e.QA != speech.QAPassed {
		t.Fatalf("backup LoadAll entry = %+v ok=%v, want the QA-passed row", e, ok)
	}
}

//
// ── splitSQL and trigger bodies ─────────────────────────────────────────────
//

// TestSplitSQLUnderstandsTriggerBodies is the proof ADR-0008 asked for: each
// FTS5 trigger in 0002_content.sql survives splitting as ONE statement with its
// internal semicolons intact. Before the splitter learned about trigger bodies
// these statements were cut in half and the migration failed with
// "SQL logic error: incomplete input".
func TestSplitSQLUnderstandsTriggerBodies(t *testing.T) {
	migs, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var content string
	for _, m := range migs {
		if m.Version == 2 {
			content = m.SQL
		}
	}
	if content == "" {
		t.Fatal("no version 2 migration embedded")
	}

	statements, err := splitSQL(content)
	if err != nil {
		t.Fatalf("splitSQL(0002_content.sql): %v", err)
	}

	var triggers []string
	total := 0
	for _, stmt := range statements {
		code := strings.ToUpper(stripLeadComments(stmt))
		switch {
		case strings.HasPrefix(code, "CREATE TRIGGER"):
			triggers = append(triggers, stmt)
		case strings.HasPrefix(code, "CREATE VIRTUAL TABLE"):
			// The FTS5 declaration is one statement, not one per option.
		default:
			continue
		}
	}
	if len(triggers) != 3 {
		t.Fatalf("CREATE TRIGGER statements = %d, want the 3 FTS5 sync triggers: %q",
			len(triggers), triggers)
	}
	for _, trg := range triggers {
		code := strings.TrimSpace(stripSQLComments(trg))
		upper := strings.ToUpper(code)
		// splitSQL strips the terminator that ended the statement, so a whole
		// trigger body reads "CREATE TRIGGER … BEGIN stmt; stmt; END".
		if !strings.HasSuffix(upper, "END") {
			t.Errorf("trigger is cut in half: %s", tail(code, 80))
		}
		if !strings.Contains(upper, "BEGIN") {
			t.Errorf("trigger lost its BEGIN keyword: %s", tail(code, 80))
		}
		// Every semicolon in the body must still be there: exactly one per body
		// statement. Counting them in code only is what makes this meaningful —
		// the comments around the triggers discuss semicolons themselves.
		stmts := strings.Count(upper, "INSERT INTO MESSAGES_FTS")
		if n := strings.Count(code, ";"); n != stmts {
			t.Errorf("trigger has %d body statement(s) but %d semicolon(s), want them equal: %s",
				stmts, n, tail(code, 160))
		}
		total += stmts
	}
	// 1 for AFTER INSERT, 1 for AFTER DELETE, 2 for AFTER UPDATE (delete-old
	// then insert-new). The two-statement body is the case a naive splitter
	// destroys, so it is asserted explicitly.
	if total != 4 {
		t.Fatalf("FTS5 sync statements across the triggers = %d, want 4", total)
	}

	var virtual int
	for _, stmt := range statements {
		if strings.HasPrefix(strings.ToUpper(stripLeadComments(stmt)), "CREATE VIRTUAL TABLE") {
			virtual++
		}
	}
	if virtual != 1 {
		t.Fatalf("CREATE VIRTUAL TABLE statements = %d, want 1", virtual)
	}
}

// TestSplitSQLRejectsAnUnterminatedTriggerBody proves the remaining limitation
// is loud rather than silent: a trigger whose END is missing swallows the rest
// of the file into one statement SQLite refuses, so the migration transaction
// rolls back instead of half-applying.
func TestSplitSQLRejectsAnUnterminatedTriggerBody(t *testing.T) {
	script := `
CREATE TABLE messages (id TEXT PRIMARY KEY, normalized_text TEXT);
CREATE TRIGGER bad AFTER INSERT ON messages BEGIN
	INSERT INTO messages (id) VALUES ('x');
	INSERT INTO messages (id) VALUES ('y');
;
CREATE TABLE never_arrives (id TEXT);
`
	statements, err := splitSQL(script)
	if err != nil {
		t.Fatalf("splitSQL: %v", err)
	}
	if len(statements) != 2 {
		t.Fatalf("statements = %d, want the table plus one swallow: %q", len(statements), statements)
	}
	if !strings.Contains(statements[1], "never_arrives") {
		t.Fatalf("the missing END did not swallow the following statement: %q", statements[1])
	}

	s := openIn(t)
	if _, err := s.db.Exec(statements[1]); err == nil {
		t.Fatal("an unterminated trigger body executed; it must be a syntax error")
	}
}

// tail returns the last n bytes of s, for failure messages about long DDL.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// stripSQLComments removes -- and /* */ comments so assertions can count
// statement tokens without tripping over prose in the migration files. It is
// deliberately simpler than splitSQL's own scanner: it does not track string
// literals, and is only used on DDL whose literals contain no comment markers.
func stripSQLComments(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j
				continue
			}
			return out.String()
		case strings.HasPrefix(s[i:], "/*"):
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				return out.String()
			}
			i += 2 + j + 2
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String()
}

//
// ── Baselines ───────────────────────────────────────────────────────────────
//

// TestBaselineContentRowsAreAccepted guards every rejection case below: if a
// baseline stopped being valid, each "rejected" expectation underneath it would
// pass for the wrong reason.
func TestBaselineContentRowsAreAccepted(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	seedContent(t, db)

	mustExec(t, db, messageBaseline("msg-base").insert())
	mustExec(t, db, messageBaseline("msg-base-2").
		with("source_id", "NULL", "source_message_id", "NULL", "speaker_id", "NULL",
			"scene_id", "NULL", "attribution_status", q("unknown")).insert())
	mustExec(t, db, characterBaseline("char-base").insert())
	mustExec(t, db, sceneBaseline("scene-base").insert())
	mustExec(t, db, regionBaseline("region-base").
		with("min_aspect", "1.3", "max_aspect", "1.7778", "min_width_px", "640").insert())
	mustExec(t, db, pageBaseline("msg-base", "1").insert())
	mustExec(t, db, edgeBaseline("msg-base", "msg-base-2").insert())
	mustExec(t, db, assertionBaseline("assert-base").with("message_id", q("msg-base")).insert())
	mustExec(t, db, correctionBaseline("corr-base").with("message_id", q("msg-base")).insert())
	mustExec(t, db, jobBaseline("job-base").insert())
	mustExec(t, db, jobBaseline("job-child").with("parent_id", q("job-base"), "idempotency_key", "NULL").insert())
	mustExec(t, db, attemptBaseline("job-base", "1").insert())
	mustExec(t, db, eventBaseline("event-base").insert())

	// The baseline row set must also be FTS-visible, or the search tests below
	// would be measuring an index that nothing ever entered.
	if n := countRows(t, db, "messages_fts"); n != 2 {
		t.Fatalf("messages_fts = %d, want one entry per message (2)", n)
	}
}

//
// ── CHECK constraints ───────────────────────────────────────────────────────
//

// TestContentCheckConstraintsRejectBadRows asserts the schema enforces what
// docs/spec/02 promises, so a bug in a parser or an agent tool cannot persist
// an impossible row. Each case varies one column of an accepted baseline.
func TestContentCheckConstraintsRejectBadRows(t *testing.T) {
	cases := []struct {
		name string
		bad  row
	}{
		// messages
		{"message text_kind enum", messageBaseline("m1").with("text_kind", q("cutscene"))},
		{"message text_kind empty", messageBaseline("m1b").with("text_kind", q(""))},
		{"message attribution_status enum", messageBaseline("m2").with("attribution_status", q("guessed"))},
		{"message confidence too high", messageBaseline("m3").with("attribution_confidence", "1.5")},
		{"message confidence too low", messageBaseline("m4").with("attribution_confidence", "-0.1")},
		{"message revision zero", messageBaseline("m5").with("revision", "0")},
		{"message markup_json not json", messageBaseline("m6").with("markup_json", q("not json"))},
		{"message flags not json", messageBaseline("m7").with("flags", q(`{"a":`))},
		{"message attribution_evidence not json", messageBaseline("m8").with("attribution_evidence", q("[1,2"))},
		{"message created_at not a date", messageBaseline("m9").with("created_at", q("yesterday"))},
		{"message updated_at not a date", messageBaseline("m10").with("updated_at", q("2026/01/01"))},
		{"message original_text null", messageBaseline("m11").with("original_text", "NULL")},
		{"message normalized_text null", messageBaseline("m12").with("normalized_text", "NULL")},

		// message_pages
		{"page ordinal zero", pageBaseline("msg-p", "0")},
		{"page ordinal negative", pageBaseline("msg-p", "-1")},
		{"page timing_json not json", pageBaseline("msg-p", "1").with("timing_json", q("nope"))},
		{"page visible_prefixes not json", pageBaseline("msg-p", "2").with("visible_prefixes", q("hello"))},
		{"page display_controls not json", pageBaseline("msg-p", "3").with("display_controls", q("A"))},
		{"page page_text null", pageBaseline("msg-p", "1").with("page_text", "NULL")},

		// message_edges
		{"edge edge_type enum", edgeBaseline("msg-e1", "msg-e2").with("edge_type", q("probably"))},
		{"edge confidence above one", edgeBaseline("msg-e1", "msg-e2").with("confidence", "2")},
		{"edge confidence negative", edgeBaseline("msg-e1", "msg-e2").with("confidence", "-0.5")},
		{"edge evidence not json", edgeBaseline("msg-e1", "msg-e2").with("evidence", q("{oops"))},

		// characters
		{"character status enum", characterBaseline("c1").with("status", q("merged"))},
		{"character aliases not json", characterBaseline("c2").with("aliases", q("Link, Hero"))},
		{"character short portrait hash", characterBaseline("c3").with("portrait_asset_id", q("abc123"))},
		{"character visual_descriptors not json", characterBaseline("c4").with("visual_descriptors", q("blue tunic"))},
		{"character verified_source_refs not json", characterBaseline("c5").with("verified_source_refs", q("src-content"))},

		// scenes
		{"scene aliases not json", sceneBaseline("s1").with("aliases", q("Deck, Ship"))},
		// 0.4 on its own *is* valid JSON, so the case has to be a
		// malformed object to exercise json_valid rather than the type.
		{"scene progress_json not json", sceneBaseline("s2").with("progress_json", q("{'low':0.4"))},
		{"scene visual_anchors not json", sceneBaseline("s3").with("visual_anchors", q("anchor"))},

		// regions — geometry is edited by hand in the calibration UI, so the
		// box itself is constrained, not just its shape.
		{"region type enum", regionBaseline("r1").with("region_type", q("speech_bubble"))},
		{"region detect_method enum", regionBaseline("r2").with("detect_method", q("guessed"))},
		{"region rect_x above one", regionBaseline("r3").with("rect_x", "1.2")},
		{"region rect_x negative", regionBaseline("r4").with("rect_x", "-0.01")},
		{"region rect_w above one", regionBaseline("r5").with("rect_w", "1.01")},
		{"region rect_h negative", regionBaseline("r6").with("rect_h", "-0.2")},
		{"region rect_y above one", regionBaseline("r6b").with("rect_y", "1.5")},
		{"region box runs off the right edge", regionBaseline("r7").with("rect_x", "0.5", "rect_w", "0.6")},
		{"region box runs off the bottom edge", regionBaseline("r8").with("rect_y", "0.9", "rect_h", "0.2")},
		{"region aspect band inverted", regionBaseline("r9").with("min_aspect", "1.8", "max_aspect", "1.3")},
		{"region aspect non-positive", regionBaseline("r10").with("min_aspect", "0")},
		{"region reliability above one", regionBaseline("r11").with("reliability", "1.1")},
		{"region revision zero", regionBaseline("r12").with("revision", "0")},
		{"region ocr_settings not json", regionBaseline("r13").with("ocr_settings", q("threshold"))},
		{"region mask_asset_ids not json", regionBaseline("r14").with("mask_asset_ids", q("mask.png"))},

		// speaker_assertions
		{"assertion method enum", assertionBaseline("a1").with("method", q("vibes"))},
		{"assertion posterior above one", assertionBaseline("a2").with("posterior", "1.2")},
		{"assertion evidence not json", assertionBaseline("a3").with("evidence", q("screenshot-1"))},
		{"assertion asserted_at not a date", assertionBaseline("a4").with("asserted_at", q("last tuesday"))},

		// corrections
		{"correction kind enum", correctionBaseline("k1").with("kind", q("vibe"))},
		{"correction target_type enum", correctionBaseline("k2").with("target_type", q("universe"))},
		{"correction status enum", correctionBaseline("k3").with("status", q("applied"))},
		{"correction actor null", correctionBaseline("k4").with("actor", "NULL")},
		{"correction reviewed_at not a date", correctionBaseline("k5").with("reviewed_at", q("soon"))},
		{"correction evidence not json", correctionBaseline("k6").with("evidence", q("frame-9"))},

		// jobs
		{"job state enum", jobBaseline("j1").with("state", q("pending"))},
		{"job priority above ceiling", jobBaseline("j2").with("priority", "101")},
		{"job priority negative", jobBaseline("j3").with("priority", "-1")},
		{"job max_attempts zero", jobBaseline("j4").with("max_attempts", "0")},
		{"job attempts above max", jobBaseline("j5").with("attempts", "4")},
		{"job attempts negative", jobBaseline("j5b").with("attempts", "-1")},
		{"job payload not json", jobBaseline("j6").with("payload_json", q("prof-content"))},
		{"job result not json", jobBaseline("j7").with("result_json", q("{"))},
		{"job run_after not a date", jobBaseline("j8").with("run_after", q("tomorrow"))},
		{"job kind null", jobBaseline("j9").with("kind", "NULL")},

		// job_attempts
		{"attempt number zero", attemptBaseline("job-a", "0")},
		{"attempt outcome enum", attemptBaseline("job-a", "1").with("outcome", q("exploded"))},
		{"attempt finished before started", attemptBaseline("job-a", "1").with("finished_at", q("2025-12-31T23:59:59Z"))},
		// Same two instants, different UTC offsets: what matters is the
		// chronology, and a plain text comparison of the two columns would have
		// accepted this one because "09" < "12".
		{"attempt finished before started across offsets", attemptBaseline("job-a", "2").
			with("started_at", q("2026-01-01T12:00:00+02:00"), "finished_at", q("2026-01-01T09:00:00Z"))},

		// events
		{"event schema_version zero", eventBaseline("e1").with("schema_version", "0")},
		{"event data_json not json", eventBaseline("e2").with("data_json", q("frame-1"))},
		{"event name null", eventBaseline("e3").with("name", "NULL")},
		{"event occurred_at not a date", eventBaseline("e4").with("occurred_at", q("earlier"))},
		{"event event_id null", eventBaseline("e5").with("event_id", "NULL")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := migratedStore(t)
			db := s.db
			seedContent(t, db)
			seedEdgeParents(t, db)

			_, err := db.Exec(tc.bad.insert())
			if err == nil {
				t.Fatalf("row accepted, want a constraint failure.\n%s", tc.bad.insert())
			}
			if !strings.Contains(err.Error(), "CHECK constraint failed") &&
				!strings.Contains(err.Error(), "NOT NULL constraint failed") {
				t.Fatalf("error = %v, want a CHECK or NOT NULL violation", err)
			}
		})
	}
}

//
// ── Foreign keys ────────────────────────────────────────────────────────────
//

func TestForeignKeyConstraintsRejectBadParents(t *testing.T) {
	cases := []struct {
		name string
		bad  row
	}{
		{"message profile", messageBaseline("f1").with("profile_id", q("prof-nope"))},
		{"message source", messageBaseline("f2").with("source_id", q("src-nope"))},
		{"message speaker", messageBaseline("f3").with("speaker_id", q("char-nope"))},
		{"message scene", messageBaseline("f4").with("scene_id", q("scene-nope"))},
		{"page message", pageBaseline("msg-nope", "1")},
		{"edge from_id", edgeBaseline("msg-nope", "msg-e2")},
		{"edge to_id", edgeBaseline("msg-e1", "msg-nope")},
		{"edge source", edgeBaseline("msg-e1", "msg-e2").with("source_id", q("src-nope"))},
		{"character profile", characterBaseline("fc1").with("profile_id", q("prof-nope"))},
		{"scene profile", sceneBaseline("fs1").with("profile_id", q("prof-nope"))},
		{"region profile", regionBaseline("fr1").with("profile_id", q("prof-nope"))},
		{"region scene", regionBaseline("fr2").with("scene_id", q("scene-nope"))},
		{"assertion profile", assertionBaseline("fa1").with("profile_id", q("prof-nope"))},
		{"assertion message", assertionBaseline("fa2").with("message_id", q("msg-nope"))},
		{"assertion character", assertionBaseline("fa3").with("character_id", q("char-nope"))},
		{"correction profile", correctionBaseline("fk1").with("profile_id", q("prof-nope"))},
		{"correction message", correctionBaseline("fk2").with("message_id", q("msg-nope"))},
		{"job parent", jobBaseline("fj1").with("parent_id", q("job-nope"))},
		{"attempt job", attemptBaseline("job-nope", "1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := migratedStore(t)
			db := s.db
			seedContent(t, db)
			seedEdgeParents(t, db)

			if _, err := db.Exec(tc.bad.insert()); err == nil {
				t.Fatalf("row accepted, want a foreign key failure.\n%s", tc.bad.insert())
			} else if !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
				t.Fatalf("error = %v, want FOREIGN KEY constraint failed", err)
			}
		})
	}
}

// TestUniquenessConstraints covers the reconciliation guards: re-running an
// import must collide with what it already wrote instead of duplicating it
// (docs/spec/02 "Transaction workflows": "no duplicate message IDs").
func TestUniquenessConstraints(t *testing.T) {
	cases := []struct {
		name  string
		first row
		again row
	}{
		{
			"messages source identity",
			messageBaseline("u1"),
			// Different id, same (profile_id, source_id, source_message_id).
			messageBaseline("u2").with("source_message_id", q("sm-u1")),
		},
		{
			"message_pages ordinal",
			pageBaseline("msg-p", "1"),
			pageBaseline("msg-p", "1").with("page_text", q("different text")),
		},
		{
			"message_edges triple",
			edgeBaseline("msg-e1", "msg-e2"),
			edgeBaseline("msg-e1", "msg-e2").with("confidence", "0.2"),
		},
		{
			"speaker_assertion per method",
			assertionBaseline("ua1").with("message_id", q("msg-a")),
			assertionBaseline("ua2").with("message_id", q("msg-a")),
		},
		{
			"job idempotency key",
			jobBaseline("uj1"),
			jobBaseline("uj2").with("idempotency_key", q("key-uj1")),
		},
		{
			"event id",
			eventBaseline("ue1"),
			eventBaseline("ue1").with("name", q("text.observed")),
		},
		{
			"job_attempts composite key",
			attemptBaseline("job-a", "1"),
			attemptBaseline("job-a", "1").with("error", q("again")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := migratedStore(t)
			db := s.db
			seedContent(t, db)
			seedEdgeParents(t, db)

			mustExec(t, db, tc.first.insert())
			if _, err := db.Exec(tc.again.insert()); err == nil {
				t.Fatalf("duplicate accepted.\nfirst: %s\nduplicate: %s", tc.first.insert(), tc.again.insert())
			} else if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
				t.Fatalf("error = %v, want UNIQUE constraint failed", err)
			}
		})
	}
}

// TestNullSourceIdentityDoesNotCollide: the partial unique index exists so that
// hand-typed and OCR-derived messages, which share NULL source_message_id, are
// not treated as duplicates of each other.
func TestNullSourceIdentityDoesNotCollide(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	seedContent(t, db)
	seedEdgeParents(t, db)

	for _, id := range []string{"hand-1", "hand-2", "hand-3"} {
		mustExec(t, db, messageBaseline(id).
			with("source_id", "NULL", "source_message_id", "NULL", "speaker_id", "NULL").insert())
	}
	if n := countRows(t, db, "messages"); n != 7 { // 4 seeded + 3 typed
		t.Fatalf("messages = %d, want 7", n)
	}
}

// TestNullIdempotencyKeysDoNotCollide: internal jobs have no client key, and
// SQLite keeps NULLs distinct inside UNIQUE.
func TestNullIdempotencyKeysDoNotCollide(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	for _, id := range []string{"k1", "k2", "k3"} {
		mustExec(t, db, jobBaseline(id).with("idempotency_key", "NULL").insert())
	}
	if n := countRows(t, db, "jobs"); n != 3 {
		t.Fatalf("jobs = %d, want 3 unsupervised rows", n)
	}
}

//
// ── ON DELETE SET NULL vs CASCADE ───────────────────────────────────────────
//

// TestDeletingAnAttributionTargetKeepsTheDialogue is the distinction docs/spec/02
// draws between content and provenance: revoking a source, scene or character
// takes away the claim about a line, never the line itself.
func TestDeletingAnAttributionTargetKeepsTheDialogue(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	fx := seedContent(t, db)
	seedEdgeParents(t, db)

	mustExec(t, db, messageBaseline("keep-me").insert())

	mustExec(t, db, `DELETE FROM characters WHERE id = ?`, fx.charID)
	mustExec(t, db, `DELETE FROM scenes WHERE id = ?`, fx.sceneID)
	mustExec(t, db, `DELETE FROM sources WHERE id = ?`, fx.sourceID)

	var speaker, scene, source sql.NullString
	if err := db.QueryRow(`SELECT speaker_id, scene_id, source_id FROM messages WHERE id = 'keep-me'`).
		Scan(&speaker, &scene, &source); err != nil {
		t.Fatal(err)
	}
	if speaker.Valid || scene.Valid || source.Valid {
		t.Fatalf("speaker=%v scene=%v source=%v, want all three NULL", speaker, scene, source)
	}
	// The words themselves survive, and so does their edit history.
	var original string
	if err := db.QueryRow(`SELECT original_text || '|' || normalized_text FROM messages WHERE id = 'keep-me'`).
		Scan(&original); err != nil {
		t.Fatal(err)
	}
	if original != "Hello there|hello there" {
		t.Fatalf("message text = %q, want it untouched", original)
	}
}

// TestAssertionRowDiesWithItsClaim: a speaker_assertion row *is* the pair
// (message, character), so it cascades from both.
func TestAssertionRowDiesWithItsClaim(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	seedContent(t, db)
	seedEdgeParents(t, db)

	mustExec(t, db, characterBaseline("char-villain").with("name", q("Villain")).insert())
	mustExec(t, db, assertionBaseline("a-msg-side").insert())
	mustExec(t, db, assertionBaseline("a-char-side").with("message_id", q("msg-e1")).insert())
	if n := countRows(t, db, "speaker_assertions"); n != 2 {
		t.Fatalf("speaker_assertions = %d, want 2", n)
	}

	// Deleting the message removes every candidate recorded for it.
	mustExec(t, db, `DELETE FROM messages WHERE id = 'msg-a'`)
	if n := countRows(t, db, "speaker_assertions"); n != 1 {
		t.Fatalf("speaker_assertions = %d after deleting the message, want only the other one", n)
	}

	// Deleting the character removes the claims that named it, not the
	// dialogue those claims were about (which is still in messages).
	mustExec(t, db, `DELETE FROM characters WHERE id = 'char-hero'`)
	if n := countRows(t, db, "speaker_assertions"); n != 0 {
		t.Fatalf("speaker_assertions = %d after deleting the character, want 0", n)
	}
	if n := countRows(t, db, "messages"); n == 0 {
		t.Fatal("deleting a character deleted the messages too; speaker_id must SET NULL")
	}
}

// TestDeletingProfileCascadesContentAndFTS: content is part of one profile
// revision, so retiring the revision retires the graph, the labels and — the
// part a derived index gets wrong silently — the FTS entries.
func TestDeletingProfileCascadesContentAndFTS(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	db := s.db
	seedContent(t, db)

	for _, id := range []string{"m-1", "m-2", "m-3"} {
		mustExec(t, db, messageBaseline(id).insert())
	}
	mustExec(t, db, pageBaseline("m-1", "1").insert())
	mustExec(t, db, edgeBaseline("m-1", "m-2").insert())
	mustExec(t, db, assertionBaseline("a-1").with("message_id", q("m-1")).insert())
	mustExec(t, db, correctionBaseline("c-1").with("message_id", q("m-1")).insert())
	mustExec(t, db, regionBaseline("reg-1").insert())

	if n := countRows(t, db, "messages_fts"); n != 3 {
		t.Fatalf("messages_fts = %d, want one entry per message", n)
	}

	mustExec(t, db, `DELETE FROM profiles WHERE id = ?`, "prof-content")

	for _, table := range []string{
		"messages", "message_pages", "message_edges", "characters", "scenes",
		"regions", "speaker_assertions", "corrections", "sources",
	} {
		if n := countRows(t, db, table); n != 0 {
			t.Errorf("%s = %d rows after deleting the profile, want 0", table, n)
		}
	}
	if n := countRows(t, db, "messages_fts"); n != 0 {
		t.Errorf("messages_fts = %d entries after the cascade, want 0", n)
	}
	// 0001 makes profiles RESTRICT against editions precisely so a title can
	// not be dropped out from under published work. This test deletes the
	// profile itself, so the edition must still be standing — if it had gone
	// too, the cascade would have silently rewritten 0001's policy.
	if n := countRows(t, db, "editions"); n != 1 {
		t.Errorf("editions = %d, want 1: only the profile was deleted", n)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatalf("IntegrityCheck after the cascade: %v", err)
	}
}

func TestDeletingParentJobCascadesChainAndAttempts(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	mustExec(t, db, jobBaseline("root").insert())
	mustExec(t, db, jobBaseline("stage-1").with("parent_id", q("root"), "idempotency_key", "NULL").insert())
	mustExec(t, db, jobBaseline("stage-2").with("parent_id", q("stage-1"), "idempotency_key", "NULL").insert())
	mustExec(t, db, attemptBaseline("stage-2", "1").insert())

	mustExec(t, db, `DELETE FROM jobs WHERE id = 'root'`)
	if n := countRows(t, db, "jobs"); n != 0 {
		t.Errorf("jobs = %d after deleting the root, want 0: the chain is one unit of work", n)
	}
	if n := countRows(t, db, "job_attempts"); n != 0 {
		t.Errorf("job_attempts = %d, want 0", n)
	}

	// The event log is deliberately unlinked from jobs and sessions, so pruning
	// a job must never take audit history with it (docs/spec/08 "Event stream
	// reliability": the log is replayed after the fact).
	mustExec(t, db, jobBaseline("solo").insert())
	mustExec(t, db, eventBaseline("audit-1").with("causation_id", q("solo")).insert())
	mustExec(t, db, `DELETE FROM jobs WHERE id = 'solo'`)
	if n := countRows(t, db, "events"); n != 1 {
		t.Errorf("events = %d, want the log untouched by job deletes", n)
	}
}

//
// ── FTS5 trigram index ──────────────────────────────────────────────────────
//

// ftsMessageIDs returns the messages a whole-index MATCH finds, proving the
// index rowids still address the right rows.
func ftsMessageIDs(t *testing.T, db *sql.DB, match string) []string {
	t.Helper()
	return ftsIDs(t, db, "messages_fts MATCH "+q(match), "")
}

// ftsColumnIDs restricts the MATCH to one indexed column.
func ftsColumnIDs(t *testing.T, db *sql.DB, column, match string) []string {
	t.Helper()
	return ftsIDs(t, db, "messages_fts."+column+" MATCH "+q(match), "")
}

// ftsProfileIDs answers "which messages of this profile contain …", the query
// Milestone 2's candidate ranker runs on every observation.
func ftsProfileIDs(t *testing.T, db *sql.DB, match, profileID string) []string {
	t.Helper()
	return ftsIDs(t, db, "messages_fts MATCH "+q(match), " AND m.profile_id = "+q(profileID))
}

// ftsIDs runs a candidate query shaped like the ranker's. Note the absence of
// a table alias: SQLite binds `MATCH` to the *named* FTS5 table, so
// `FROM messages_fts f … WHERE f MATCH ?` (or WHERE messages_fts MATCH ? with
// the alias in place) silently stops being a full-text predicate and returns
// every joined row. This test caught that by asserting exact id sets.
func ftsIDs(t *testing.T, db *sql.DB, where, extra string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT m.id FROM messages_fts JOIN messages m ON m.rowid = messages_fts.rowid
		WHERE ` + where + extra + ` ORDER BY m.id`)
	if err != nil {
		t.Fatalf("fts query %q%s: %v", where, extra, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestFTSTrigramSearchStaysInSyncWithMessages is the acceptance test for the
// external-content pattern: a three-character substring finds text the user
// never typed whole, and UPDATE/DELETE keep the index honest. A stale entry
// here means the narrator speaks a line that was edited away.
func TestFTSTrigramSearchStaysInSyncWithMessages(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	seedContent(t, db)

	// Every text column is set explicitly: the baseline's speech_text contains
	// "Hello", and leaving it in f-2/f-3 would make the 'ello' assertion below
	// pass or fail for the wrong reason.
	mustExec(t, db, messageBaseline("f-1").
		with("original_text", q("Hello there!"), "normalized_text", q("hello there"),
			"speech_text", q("Hello there!")).insert())
	mustExec(t, db, messageBaseline("f-2").
		with("original_text", q("Open the door."), "normalized_text", q("open the door"),
			"speech_text", q("Open the door.")).insert())
	mustExec(t, db, messageBaseline("f-3").
		with("original_text", q("A sign"), "normalized_text", q("a sign on the wall"),
			"speech_text", q("A sign on the wall.")).insert())

	// 'ello' is not a word: with the default unicode61 tokenizer this query
	// would find nothing, which is exactly why docs/spec/02 asks for trigrams.
	if ids := ftsMessageIDs(t, db, "ello"); len(ids) != 1 || ids[0] != "f-1" {
		t.Fatalf("MATCH 'ello' -> %v, want [f-1]", ids)
	}
	if ids := ftsMessageIDs(t, db, "sign"); len(ids) != 1 || ids[0] != "f-3" {
		t.Fatalf("MATCH 'sign' -> %v, want [f-3]", ids)
	}

	// Both columns are indexed: speech_text carries text the source never had
	// ("the A button"), so an editor can find an utterance by what it sounds like.
	mustExec(t, db, messageBaseline("f-4").
		with("normalized_text", q("press a button"), "speech_text", q("Press the A button.")).insert())
	if ids := ftsMessageIDs(t, db, "\"the A button\""); len(ids) != 1 || ids[0] != "f-4" {
		t.Fatalf("MATCH '\"the A button\"' -> %v, want [f-4]", ids)
	}
	// And a column-restricted query must not leak a hit from the other column.
	if ids := ftsColumnIDs(t, db, "normalized_text", "\"the a button\""); len(ids) != 0 {
		t.Fatalf("normalized_text MATCH 'the a button' -> %v, want no hits", ids)
	}
	if ids := ftsColumnIDs(t, db, "normalized_text", "\"press a\""); len(ids) != 1 || ids[0] != "f-4" {
		t.Fatalf("normalized_text MATCH 'press a' -> %v, want [f-4]", ids)
	}

	// UPDATE: the old text must stop matching.
	mustExec(t, db, `UPDATE messages SET normalized_text = 'farewell friend', speech_text = 'Farewell, friend.'
		WHERE id = 'f-1'`)
	if ids := ftsMessageIDs(t, db, "ello"); len(ids) != 0 {
		t.Fatalf("after UPDATE, MATCH 'ello' -> %v, want no stale hits", ids)
	}
	if ids := ftsMessageIDs(t, db, "farewell"); len(ids) != 1 || ids[0] != "f-1" {
		t.Fatalf("after UPDATE, MATCH 'farewell' -> %v, want [f-1]", ids)
	}

	// An UPDATE that touches no indexed column must leave the entry usable:
	// the delete-then-insert pattern is only correct if the values it hands to
	// 'delete' are the ones actually in the index.
	mustExec(t, db, `UPDATE messages SET attribution_confidence = 0.11 WHERE id = 'f-2'`)
	if ids := ftsMessageIDs(t, db, "\"open the\""); len(ids) != 1 || ids[0] != "f-2" {
		t.Fatalf("after an unrelated UPDATE, MATCH 'open the' -> %v, want [f-2]", ids)
	}
	// Overlapping tokens are where a wrong trigger order shows up: the new text
	// shares nearly all its trigrams with the old, so inserting before deleting
	// would let the delete strip entries the insert had just written.
	mustExec(t, db, `UPDATE messages SET normalized_text = 'open the door loudly' WHERE id = 'f-2'`)
	if ids := ftsMessageIDs(t, db, "\"open the door loudly\""); len(ids) != 1 || ids[0] != "f-2" {
		t.Fatalf("after an overlapping UPDATE, MATCH -> %v, want [f-2]", ids)
	}

	// DELETE.
	mustExec(t, db, `DELETE FROM messages WHERE id = 'f-2'`)
	if ids := ftsMessageIDs(t, db, "\"open the\""); len(ids) != 0 {
		t.Fatalf("after DELETE, MATCH 'open the' -> %v, want no hits", ids)
	}
	if n := countRows(t, db, "messages_fts"); n != 3 {
		t.Fatalf("messages_fts = %d, want one entry per remaining message (3)", n)
	}
}

// TestFTSSearchIsProfileScoped: FTS5 has no notion of a profile, so scoping is
// a join — and a session pinned to one revision must never hear another's
// lines.
func TestFTSSearchIsProfileScoped(t *testing.T) {
	s := migratedStore(t)
	db := s.db
	seedContent(t, db)
	insertSecondProfile(t, db)

	mustExec(t, db, messageBaseline("scope-a").with("normalized_text", q("the unique phrase here")).insert())
	mustExec(t, db, messageBaseline("scope-b").
		with("profile_id", q("prof-two"), "source_id", q("src-two"),
			"scene_id", "NULL", "speaker_id", "NULL", "normalized_text", q("the unique phrase here")).insert())

	if ids := ftsProfileIDs(t, db, "\"unique phrase\"", "prof-content"); len(ids) != 1 || ids[0] != "scope-a" {
		t.Fatalf("profile-scoped search -> %v, want [scope-a]", ids)
	}
	if ids := ftsProfileIDs(t, db, "\"unique phrase\"", "prof-two"); len(ids) != 1 || ids[0] != "scope-b" {
		t.Fatalf("profile-scoped search -> %v, want [scope-b]", ids)
	}
	// Uncorrected, the same query finds both — the index really is global and
	// the join is what enforces the scope.
	if ids := ftsMessageIDs(t, db, "\"unique phrase\""); len(ids) != 2 {
		t.Fatalf("unscoped search -> %v, want both rows", ids)
	}
}

//
// ── jobs and events ─────────────────────────────────────────────────────────
//

// TestJobQueueIsClaimableInPriorityOrder exercises the durable queue and the
// composite index that makes the claim query cheap (docs/spec/02 "SQLite schema
// principles": job indexes; docs/spec/08: /jobs/:id/cancel|retry survive a
// restart).
func TestJobQueueIsClaimableInPriorityOrder(t *testing.T) {
	s := migratedStore(t)
	db := s.db

	mustExec(t, db, jobBaseline("ordinary").insert())
	mustExec(t, db, jobBaseline("urgent").with("priority", "90").insert())
	mustExec(t, db, jobBaseline("deferred").with("run_after", q("2099-01-01T00:00:00Z")).insert())
	mustExec(t, db, jobBaseline("done").with("state", q("succeeded"),
		"result_json", q(`{"messages":10}`)).insert())
	mustExec(t, db, jobBaseline("parked").with("state", q("requires_input")).insert())

	// The worker's claim query, as the queue will run it.
	rows, err := db.Query(`SELECT id FROM jobs
		-- Eligible means due: run_after is the earliest start, so a job whose
		-- run_after is in the future waits. (Asserting the ordering is the
		-- point of this test, so the clause is spelled out rather than hidden
		-- behind a helper that could get it backwards.)
		WHERE state = 'queued'
		  AND (run_after IS NULL OR run_after <= strftime('%Y-%m-%dT%H:%M:%SZ','now'))
		ORDER BY priority DESC, created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var claimable []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		claimable = append(claimable, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(claimable, ","); got != "urgent,ordinary" {
		t.Fatalf("claim order = %q, want urgent,ordinary (deferred waits, parked and done are not claimable)", got)
	}

	// A retry records an attempt and consumes budget; the last one that fits is
	// attempts == max_attempts, and the CHECK already proves one more is
	// impossible (TestContentCheckConstraintsRejectBadRows/job attempts above max).
	mustExec(t, db, `UPDATE jobs SET state = 'running', attempts = 2, updated_at = ? WHERE id = 'ordinary'`,
		nowRFC3339())
	mustExec(t, db, attemptBaseline("ordinary", "2").with("outcome", q("timed_out")).insert())
	var attempts, maxAttempts int
	if err := db.QueryRow(`SELECT attempts, max_attempts FROM jobs WHERE id = 'ordinary'`).
		Scan(&attempts, &maxAttempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || maxAttempts != 3 {
		t.Fatalf("attempts/max = %d/%d, want 2/3", attempts, maxAttempts)
	}
	if _, err := db.Exec(`UPDATE jobs SET attempts = 4 WHERE id = 'ordinary'`); err == nil {
		t.Fatal("UPDATE bypassed the attempts <= max_attempts CHECK")
	}
	// Crash recovery per docs/spec/02: an interrupted `running` job is
	// resumable, which is what resume_token exists for.
	var resume sql.NullString
	if err := db.QueryRow(`SELECT resume_token FROM jobs WHERE id = 'ordinary'`).Scan(&resume); err != nil {
		t.Fatal(err)
	}
	if !resume.Valid || resume.String == "" {
		t.Fatal("job has no resume_token, so a crash cannot be resumed rather than redone")
	}
}

// TestEventLogSequenceIsMonotonicAndReplayable covers what SSE Last-Event-ID
// replay needs (docs/spec/08 "Event stream reliability"): a global order, an
// id the client can hand back, and per-session slicing.
func TestEventLogSequenceIsMonotonicAndReplayable(t *testing.T) {
	s := migratedStore(t)
	db := s.db

	names := []string{"frame.observed", "text.observed", "message.candidates", "decision.resolved", "speech.started"}
	for i, name := range names {
		cause := "NULL"
		if i > 0 {
			cause = q(fmt.Sprintf("ev-%d", i-1))
		}
		mustExec(t, db, eventBaseline(fmt.Sprintf("ev-%d", i)).
			with("name", q(name), "causation_id", cause).insert())
	}
	// A global event belongs to no session.
	mustExec(t, db, eventBaseline("ev-global").
		with("session_id", "NULL", "name", q("config.changed")).insert())

	var seqs []int64
	rows, err := db.Query(`SELECT seq FROM events WHERE session_id = 'sess-1' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if len(seqs) != len(names) {
		t.Fatalf("session replay returned %d events, want %d", len(seqs), len(names))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seq not increasing: %v", seqs)
		}
	}

	// Replay from a cursor: the client saw ev-2, so it asks for what came after.
	var replayed []string
	rows, err = db.Query(`SELECT event_id FROM events WHERE session_id = 'sess-1' AND seq >
		(SELECT seq FROM events WHERE event_id = 'ev-2') ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		replayed = append(replayed, id)
	}
	rows.Close()
	if got := strings.Join(replayed, ","); got != "ev-3,ev-4" {
		t.Fatalf("replay after ev-2 = %q, want ev-3,ev-4", got)
	}

	// Causal chain: the trace id is how the developer inspector pulls a whole
	// decision (GET /api/v1/dev/traces/:id).
	var inTrace int
	if err := db.QueryRow(`SELECT count(*) FROM events WHERE trace_id = 'trace-1'`).Scan(&inTrace); err != nil {
		t.Fatal(err)
	}
	if inTrace != len(names)+1 {
		t.Fatalf("trace-1 events = %d, want %d", inTrace, len(names)+1)
	}

	// The AUTOINCREMENT sequence must not be reissued after the newest row is
	// deleted, or a client's cursor would silently name a different event.
	mustExec(t, db, `DELETE FROM events WHERE event_id = 'ev-4'`)
	mustExec(t, db, eventBaseline("ev-after-delete").insert())
	var newSeq int64
	if err := db.QueryRow(`SELECT seq FROM events WHERE event_id = 'ev-after-delete'`).Scan(&newSeq); err != nil {
		t.Fatal(err)
	}
	if newSeq <= int64(len(names)) {
		t.Fatalf("seq %d was reused after pruning (events: %v), want a fresh number", newSeq, seqs)
	}
}
