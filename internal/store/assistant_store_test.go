package store

// The assistant thread log's migration and round-trip proof (ADR-0099,
// AGENTS.md rule 10): migration 0016 applies on a blank database, the
// thread/question rows survive the Backup → Restore round trip with their
// fields intact, and the question lifecycle's guarded writes refuse the
// transitions the schema CHECKs close.

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// seedAssistantThread creates a run + thread + one question + one answered
// question, the way the API writes them, and returns the ids the assertions
// read back.
func seedAssistantThread(t *testing.T, s *Store, user, runID, threadID string) (qOpen, qAnswered string) {
	t.Helper()
	ctx := testCtx(t)
	threads := NewThreadStore(s)
	questions := NewQuestionStore(s)
	agentStore := NewAgentStore(s)

	plan := []byte(`{"thread_id":"` + threadID + `"}`)
	if err := agentStore.CreateAgentRun(ctx, AgentRun{
		ID: runID, ProfileID: "", State: AgentStateQueued,
		PlanJSON: plan, PromptVersion: "assistant-1",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := threads.CreateThread(ctx, AssistantThread{
		ID: threadID, UserID: user, RunID: runID, Title: "seed thread",
	}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	qOpen = games.NewID()
	if err := questions.CreateQuestion(ctx, AssistantQuestion{
		ID: qOpen, ThreadID: threadID, RunID: runID,
		Kind:   AssistantQuestionKindQuestion,
		Prompt: "which edition?", Options: []string{"us", "eu"},
	}); err != nil {
		t.Fatalf("create question: %v", err)
	}
	qAnswered = games.NewID()
	if err := questions.CreateQuestion(ctx, AssistantQuestion{
		ID: qAnswered, ThreadID: threadID, RunID: runID,
		Kind:   AssistantQuestionKindConfirmation,
		Prompt: "re-point the lines?", Options: []string{"confirm", "cancel"},
		ContextRaw: []byte(`{"tool":"relink_speaker","args":"{}"}`),
	}); err != nil {
		t.Fatalf("create confirmation: %v", err)
	}
	// The answered half: an initial turn (seq 0) and the answer's durable
	// message (seq 1), the way the API writes them — the answer row records
	// the appended message's seq in the same write.
	if _, err := agentStore.AppendAgentMessage(ctx, runID, "user",
		[]byte(`{"role":"user","content":"first turn"}`)); err != nil {
		t.Fatalf("append first turn: %v", err)
	}
	if seq, err := agentStore.AppendAgentMessage(ctx, runID, "user",
		[]byte(`{"role":"user","content":"the user answered"}`)); err != nil || seq != 1 {
		t.Fatalf("append answer message: seq %d, %v; want 1", seq, err)
	}
	if err := questions.AnswerQuestion(ctx, qAnswered, "confirm", 1); err != nil {
		t.Fatalf("answer confirmation: %v", err)
	}
	return qOpen, qAnswered
}

// TestAssistantTablesMigrateOnBlankDatabase is the forward-migration half of
// rule 10: a fresh database runs 0001→0016 and the assistant tables exist
// with their CHECKs armed.
func TestAssistantTablesMigrateOnBlankDatabase(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	for _, table := range []string{"assistant_threads", "assistant_questions"} {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s after blank-database migration: n=%d err=%v", table, n, err)
		}
	}
	// The CHECKs are armed: an invalid kind is refused by the table, not by
	// the caller.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO assistant_questions (id, thread_id, run_id, kind, prompt, options_json, answer_seq, created_at)
		 VALUES (?, ?, ?, 'bogus', 'p', '[]', -1, ?)`,
		games.NewID(), games.NewID(), games.NewID(), nowStamp())
	if err == nil {
		t.Fatal("a bogus question kind was accepted; the CHECK is not armed")
	}
	// A thread without its run row is refused by the FK.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO assistant_threads (id, user_id, run_id, title, created_at, updated_at)
		 VALUES (?, 'u1', ?, 't', ?, ?)`,
		games.NewID(), games.NewID(), nowStamp(), nowStamp()); err == nil {
		t.Fatal("a thread without its run row was accepted; the FK is not armed")
	}
}

// TestAssistantThreadsSurviveBackupRoundTrip is the backup/restore half of
// rule 10: seeded rows come back through a real Backup → Restore with their
// answered-state intact (the answer_seq / applied pair is what the
// exactly-one-follow-up predicate reads, so it is the field a round trip must
// not corrupt).
func TestAssistantThreadsSurviveBackupRoundTrip(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	runID, threadID := games.NewID(), games.NewID()
	qOpen, qAnswered := seedAssistantThread(t, s, "user-a", runID, threadID)

	// A second user's thread: ownership must survive the round trip too.
	otherThread := games.NewID()
	if err := NewAgentStore(s).CreateAgentRun(ctx, AgentRun{
		ID: games.NewID(), ProfileID: "", State: AgentStateQueued,
		PlanJSON: []byte(`{}`), PromptVersion: "assistant-1",
	}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	otherRun := func() string {
		var id string
		if err := s.db.QueryRowContext(ctx,
			`SELECT id FROM agent_runs ORDER BY created_at DESC, rowid DESC LIMIT 1`).Scan(&id); err != nil {
			t.Fatalf("read second run: %v", err)
		}
		return id
	}()
	if err := NewThreadStore(s).CreateThread(ctx, AssistantThread{
		ID: otherThread, UserID: "user-b", RunID: otherRun, Title: "other thread",
	}); err != nil {
		t.Fatalf("second thread: %v", err)
	}

	// Backup → open the backup and read the rows back.
	backupPath := DBPath(t.TempDir())
	if err := s.Backup(ctx, backupPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	backup, err := Open(ctx, backupPath)
	if err != nil {
		t.Fatalf("Open backup: %v", err)
	}
	if n := countRows(t, backup.db, "assistant_threads"); n != 2 {
		t.Fatalf("backup assistant_threads = %d, want 2", n)
	}
	if n := countRows(t, backup.db, "assistant_questions"); n != 2 {
		t.Fatalf("backup assistant_questions = %d, want 2", n)
	}
	qs := NewQuestionStore(backup)
	got, err := qs.GetQuestion(ctx, qAnswered)
	if err != nil {
		t.Fatalf("backup GetQuestion: %v", err)
	}
	if got.Answer != "confirm" || got.AnswerSeq != 1 || !got.Applied || got.AnsweredAt == nil {
		t.Fatalf("backup answered state = answer %q seq %d applied %v answered_at %v; want confirm/1/true/set",
			got.Answer, got.AnswerSeq, got.Applied, got.AnsweredAt)
	}
	gotOpen, err := qs.GetQuestion(ctx, qOpen)
	if err != nil {
		t.Fatalf("backup GetQuestion(open): %v", err)
	}
	if gotOpen.Answer != "" || gotOpen.Applied {
		t.Fatalf("backup open question = %+v; want still open", gotOpen)
	}
	if err := backup.Close(); err != nil {
		t.Fatalf("close backup: %v", err)
	}

	// Restore into a fresh path and re-verify through the stores.
	restoredPath := DBPath(t.TempDir())
	if err := Restore(ctx, backupPath, restoredPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, err := Open(ctx, restoredPath)
	if err != nil {
		t.Fatalf("Open restored: %v", err)
	}
	defer restored.Close()
	ths := NewThreadStore(restored)
	th, err := ths.GetThread(ctx, threadID)
	if err != nil {
		t.Fatalf("restored GetThread: %v", err)
	}
	if th.UserID != "user-a" || th.RunID != runID || th.Title != "seed thread" {
		t.Fatalf("restored thread = %+v", th)
	}
	// user-b sees only their thread after the round trip.
	list, _, err := ths.ListThreads(ctx, "user-b", "", 0)
	if err != nil {
		t.Fatalf("restored ListThreads: %v", err)
	}
	if len(list) != 1 || list[0].ID != otherThread {
		t.Fatalf("restored user-b threads = %+v; want only the second thread", list)
	}
	// The follow-up predicate survives: an attempt that started after the
	// first turn (start seq 0) owes one follow-up — the answer's message (seq
	// 1) postdates it; the open question does not count at all.
	unseen, err := NewQuestionStore(restored).UnseenAnsweredQuestions(ctx, threadID, 0)
	if err != nil {
		t.Fatalf("restored UnseenAnsweredQuestions: %v", err)
	}
	if len(unseen) != 1 || unseen[0].ID != qAnswered {
		t.Fatalf("restored unseen = %+v; want the answered confirmation only", unseen)
	}
	// A start seq at or after the answer's seq owes nothing.
	if unseen, err = NewQuestionStore(restored).UnseenAnsweredQuestions(ctx, threadID, 1); err != nil || len(unseen) != 0 {
		t.Fatalf("restored unseen at seq 1 = %+v, %v; want none", unseen, err)
	}
}

// TestAssistantQuestionAnswerIsGuarded is the lifecycle half: an answered
// question cannot be answered a second time (the double-follow-up guard at
// the store layer), and the thread list is per-user.
func TestAssistantQuestionAnswerIsGuarded(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	runID, threadID := games.NewID(), games.NewID()
	_, q := seedAssistantThread(t, s, "user-a", runID, threadID)
	qs := NewQuestionStore(s)

	if err := qs.AnswerQuestion(ctx, q, "cancel", 2); err == nil {
		t.Fatal("a second answer to an answered question was accepted")
	}
	// The original answer is untouched.
	got, err := qs.GetQuestion(ctx, q)
	if err != nil {
		t.Fatalf("GetQuestion: %v", err)
	}
	if got.Answer != "confirm" || got.AnswerSeq != 1 {
		t.Fatalf("answer after refused re-answer = %q seq %d; want confirm/1", got.Answer, got.AnswerSeq)
	}

	// ListQuestions is thread-scoped.
	qsList, err := qs.ListQuestions(ctx, threadID, 0)
	if err != nil {
		t.Fatalf("ListQuestions: %v", err)
	}
	if len(qsList) != 2 {
		t.Fatalf("ListQuestions = %d, want 2", len(qsList))
	}
}

// TestMaxAgentMessageSeq is the attempt's start-seq marker: -1 for an empty
// run, the last seq after a message, the way the follow-up predicate reads.
func TestMaxAgentMessageSeq(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	agentStore := NewAgentStore(s)
	runID := games.NewID()
	if err := agentStore.CreateAgentRun(ctx, AgentRun{
		ID: runID, ProfileID: "", State: AgentStateQueued,
		PlanJSON: []byte(`{}`), PromptVersion: "assistant-1",
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	seq, err := s.MaxAgentMessageSeq(ctx, runID)
	if err != nil || seq != -1 {
		t.Fatalf("empty run seq = %d, %v; want -1", seq, err)
	}
	if seq, err := agentStore.AppendAgentMessage(ctx, runID, "user", []byte(`{"role":"user","content":"hi"}`)); err != nil || seq != 0 {
		t.Fatalf("append: seq %d, %v; want 0 (zero-based)", seq, err)
	}
	seq, err = s.MaxAgentMessageSeq(ctx, runID)
	if err != nil || seq != 0 {
		t.Fatalf("after one message seq = %d, %v; want 0", seq, err)
	}
	if seq, err := agentStore.AppendAgentMessage(ctx, runID, "assistant", []byte(`{"role":"assistant","content":"hello"}`)); err != nil || seq != 1 {
		t.Fatalf("append second: seq %d, %v; want 1", seq, err)
	}
	seq, err = s.MaxAgentMessageSeq(ctx, runID)
	if err != nil || seq != 1 {
		t.Fatalf("after two messages seq = %d, %v; want 1", seq, err)
	}
}

// TestAssistantConfirmMutationsSetting is the per-user setting the
// confirmation gate reads: default-on when absent, explicit off honored, and
// per-user isolation (the key carries the user id).
func TestAssistantConfirmMutationsSetting(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)

	v, ok, err := s.GetAssistantConfirmMutations(ctx, "user-a")
	if err != nil || ok || v != true {
		t.Fatalf("absent setting = %v ok=%v %v; want default true, ok=false", v, ok, err)
	}
	if err := s.SetAssistantConfirmMutations(ctx, "user-a", false); err != nil {
		t.Fatalf("set off: %v", err)
	}
	v, ok, err = s.GetAssistantConfirmMutations(ctx, "user-a")
	if err != nil || !ok || v != false {
		t.Fatalf("set-off setting = %v ok=%v %v; want false/true", v, ok, err)
	}
	// user-b is unaffected.
	if v, ok, err = s.GetAssistantConfirmMutations(ctx, "user-b"); err != nil || ok || v != true {
		t.Fatalf("user-b setting = %v ok=%v %v; want default true, ok=false", v, ok, err)
	}
}
