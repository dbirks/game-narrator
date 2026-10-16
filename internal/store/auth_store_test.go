package store

// Tests for the 0006 auth tables: what the schema refuses, what a revocation
// actually writes, and — the property the whole design rests on — that no raw
// credential is ever stored or read back (docs/spec/08 "API version and user
// authentication", docs/spec/14 "stolen old session cookie invalidated",
// ADR-0016).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A 64-hex digest stands in for the SHA-256 output internal/auth produces; the
// column CHECKs only accept that shape, which is itself one of the tests below.
func digest(seed string) string {
	var h [32]byte
	for i := range h {
		h[i] = byte(seed[i%len(seed)] ^ byte(i*7))
	}
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, 64)
	for _, b := range h {
		out = append(out, hexdigits[b>>4], hexdigits[b&0xf])
	}
	return string(out)
}

// hashEnvelope is a valid pbkdf2 envelope shape (internal/auth writes the real
// ones; a storage test must not spend seconds in a KDF to check a CHECK).
const hashEnvelope = "pbkdf2-sha256$600000$zms16-bytes-salt$c2FsdGVkLWtleS1mb3ItdGVzdHM"

func authClock() time.Time { return time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC) }

func TestMigration0006RegistersTheAuthTables(t *testing.T) {
	ctx := testCtx(t)
	s := migratedStore(t)

	if v, err := s.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("schema version after Migrate = %d (%v), want %d: 0006_auth.sql must be part of the dense sequence", v, err, LatestSchemaVersion())
	}
	found := false
	for _, name := range MigrationNames() {
		if name == "0006_auth.sql" {
			found = true
		}
	}
	if !found {
		t.Fatal("0006_auth.sql is not embedded: no existing appliance will ever gain these tables")
	}
	for _, table := range []string{"users", "sessions", "api_tokens", "auth_audit"} {
		var name string
		err := s.db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	// The point of storing id_hash as the primary key: there is no column that
	// could hold a usable session identifier beside it.
	var cols string
	if err := s.db.QueryRowContext(ctx,
		"SELECT group_concat(name, ',') FROM pragma_table_info('sessions')").Scan(&cols); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "secret", "password"} {
		if strings.Contains(strings.ToLower(cols), forbidden) {
			t.Errorf("sessions has a column named %q — only digests may be stored: %s", forbidden, cols)
		}
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatalf("integrity check after 0006: %v", err)
	}
}

func TestAuthStoreUserConstraints(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()

	u, err := a.CreateUser(ctx, CreateUserParams{Username: "admin", PasswordHash: hashEnvelope, Role: RoleAdmin, Now: now})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID == "" || u.CreatedAt.UTC().Second() != now.Second() || u.PasswordChangedAt.IsZero() {
		t.Fatalf("row = %+v", u)
	}
	if u.Disabled {
		t.Error("a new account must start enabled")
	}

	// Case-insensitive uniqueness: the NOCASE index is the whole defence against
	// two principals that login cannot tell apart.
	if _, err := a.CreateUser(ctx, CreateUserParams{Username: "ADMIN", PasswordHash: hashEnvelope, Role: RoleViewer, Now: now}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username (case-flipped) err = %v, want ErrConflict", err)
	}

	// The enum CHECK is reachable: Go validates first, but a hand-written row
	// must not be able to invent a sixth role.
	if _, err := a.CreateUser(ctx, CreateUserParams{Username: "ok-name", PasswordHash: hashEnvelope, Role: "supreme", Now: now}); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("role supreme err = %v, want ErrInvalidRole", err)
	}
	for _, bad := range []struct {
		what, username, hash string
		want                 error
	}{
		{"short username", "ad", hashEnvelope, ErrInvalidUsername},
		{"punctuation", "ad\vm in", hashEnvelope, ErrInvalidUsername},
		{"leading dot", ".admin", hashEnvelope, ErrInvalidUsername},
		{"raw password as hash", "good-name", "hunter2hunter2", ErrInvalidPasswordHash},
		{"empty hash", "good-name2", "", ErrInvalidPasswordHash},
	} {
		_, err := a.CreateUser(ctx, CreateUserParams{Username: bad.username, PasswordHash: bad.hash, Role: RoleViewer, Now: now})
		if !errors.Is(err, bad.want) {
			t.Errorf("%s: err = %v, want %v", bad.what, err, bad.want)
		}
	}

	// A role written by hand below the CHECK must be reported as the constraint
	// violation it is (repository validation is the first line, not the only one).
	if _, err := s06Exec(t, s06DB(t, a), `INSERT INTO users (id, username, password_hash, role, disabled, created_at, password_changed_at)
		VALUES ('x1','sql-role','`+hashEnvelope+`','overlord',0,'2026-10-16T03:04:05Z','2026-10-16T03:04:05Z')`); err == nil {
		t.Error("the users.role CHECK did not reject an unknown role")
	}
	if _, err := s06Exec(t, s06DB(t, a), `INSERT INTO users (id, username, password_hash, role, disabled, created_at, password_changed_at)
		VALUES ('x2','sql-time','`+hashEnvelope+`','viewer',0,'not-a-time','2026-10-16T03:04:05Z')`); err == nil {
		t.Error("the users.created_at CHECK did not reject an unparsable timestamp")
	}
}

func TestAuthStoreReadsAndRolePresence(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()

	viewer, err := a.CreateUser(ctx, CreateUserParams{Username: "viewer1", PasswordHash: hashEnvelope, Role: RoleViewer, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.UserByID(ctx, viewer.ID); err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	byName, err := a.UserByUsername(ctx, "VIEWER1")
	if err != nil {
		t.Fatalf("UserByUsername is case-insensitive by schema: %v", err)
	}
	if byName.ID != viewer.ID {
		t.Fatal("the two lookups disagree about who this is")
	}
	if _, err := a.UserByUsername(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user err = %v, want ErrNotFound", err)
	}

	// First-run setup asks exactly this question, and a *disabled* admin must keep
	// it answered "yes": otherwise disabling the account you minted would hand the
	// appliance back to whoever can reach the setup route.
	if has, err := a.HasRole(ctx, RoleAdmin); err != nil || has {
		t.Fatalf("HasRole(admin) = %v (%v) with only a viewer", has, err)
	}
	admin, err := a.CreateUser(ctx, CreateUserParams{Username: "admin1", PasswordHash: hashEnvelope, Role: RoleAdmin, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if has, err := a.HasRole(ctx, RoleAdmin); err != nil || !has {
		t.Fatalf("HasRole(admin) = %v (%v) after creating one", has, err)
	}
	if _, err := a.SetUserDisabled(ctx, admin.ID, true, now); err != nil {
		t.Fatal(err)
	}
	if has, err := a.HasRole(ctx, RoleAdmin); err != nil || !has {
		t.Errorf("HasRole(admin) = %v (%v) after disabling: setup must stay closed (0006's comment on users.disabled)", has, err)
	}
	if _, err := a.CreateUser(ctx, CreateUserParams{Username: "viewer2", PasswordHash: hashEnvelope, Role: "nope", Now: now}); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("CreateUser accepted an unknown role: %v", err)
	}
	users, err := a.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("ListUsers returned %d rows, want both accounts", len(users))
	}
}

func TestAuthStoreSessionLifecycle(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()
	u, err := a.CreateUser(ctx, CreateUserParams{Username: "operator1", PasswordHash: hashEnvelope, Role: RoleOperator, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	rawToken := "session-token-value-not-stored-anywhere"
	rawCSRF := "csrf-value-not-stored-anywhere"

	sess, err := a.InsertSession(ctx, InsertSessionParams{
		IDHash: HashSecret(rawToken), UserID: u.ID, CSRFHash: HashSecret(rawCSRF),
		CreatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour), UserAgent: "curl/8.9",
	})
	if err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if !sess.LastSeenAt.Equal(sess.CreatedAt) {
		t.Errorf("a new session's idle clock starts at %s, want its creation %s", sess.LastSeenAt, sess.CreatedAt)
	}
	if sess.Revoked() {
		t.Error("a new session must not be revoked")
	}

	got, err := a.SessionByHash(ctx, HashSecret(rawToken))
	if err != nil {
		t.Fatalf("SessionByHash: %v", err)
	}
	if got.UserID != u.ID || got.CSRFHash != HashSecret(rawCSRF) {
		t.Fatalf("session = %+v", got)
	}
	// The raw value must not resolve, and must not be recoverable from the row.
	if _, err := a.SessionByHash(ctx, rawToken); !errors.Is(err, ErrInvalidSecret) {
		t.Errorf("a raw token was accepted as a digest: %v — 0006's length CHECK is what makes this impossible", err)
	}
	if raw := s06RawRow(t, s06DB(t, a), "sessions"); strings.Contains(raw, rawToken) || strings.Contains(raw, rawCSRF) {
		t.Errorf("the sessions table physically contains a raw credential: %s", raw)
	}

	later := now.Add(3 * time.Hour)
	if err := a.TouchSession(ctx, sess.IDHash, later); err != nil {
		t.Fatal(err)
	}
	touched, err := a.SessionByHash(ctx, sess.IDHash)
	if err != nil {
		t.Fatal(err)
	}
	if !touched.LastSeenAt.Equal(later) {
		t.Fatalf("last_seen_at = %s, want %s (TEXT comparison must stay chronological)", touched.LastSeenAt, later)
	}
	if !touched.ExpiresAt.Equal(sess.ExpiresAt) {
		t.Error("touching a session moved its absolute deadline, which would make the 7-day cap unreachable")
	}

	if err := a.RevokeSession(ctx, sess.IDHash, now.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	revoked, err := a.SessionByHash(ctx, sess.IDHash)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked.Revoked() {
		t.Fatal("RevokeSession did not set revoked_at")
	}
	// Idempotent: logging out twice (a double-clicked button) is not an error.
	if err := a.RevokeSession(ctx, sess.IDHash, now.Add(5*time.Hour)); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	if err := a.RevokeSession(ctx, digest("never-issued"), now); err != nil {
		t.Errorf("revoking an unknown session: %v", err)
	}

	// A session for a principal that does not exist is refused by the foreign key
	// (0006 justifies CASCADE there: a session outliving its user is a live cookie
	// pointing at nothing).
	if _, err := a.InsertSession(ctx, InsertSessionParams{
		IDHash: digest("orphan"), UserID: "no-such-user", CSRFHash: digest("csrf"),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); !errors.Is(err, ErrConflict) {
		t.Errorf("orphan session err = %v, want ErrConflict (foreign key)", err)
	}
	if _, err := a.InsertSession(ctx, InsertSessionParams{
		IDHash: digest("no-expiry"), UserID: u.ID, CSRFHash: digest("csrf"), CreatedAt: now,
	}); err == nil {
		t.Error("a session with no absolute expiry was accepted — the 7-day cap is a schema requirement, not a caller's good manners")
	}
	sessions, err := a.ListUserSessions(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the one valid session: the two rejected inserts wrote nothing, which
	// is what makes the count an assertion rather than a formality.
	if len(sessions) != 1 || sessions[0].IDHash != sess.IDHash {
		t.Fatalf("ListUserSessions = %+v, want only the session that was accepted", sessions)
	}
}

func TestAuthStorePasswordRotationRevokesEverythingInOneWrite(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()
	u, err := a.CreateUser(ctx, CreateUserParams{Username: "admin", PasswordHash: hashEnvelope, Role: RoleAdmin, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := a.InsertSession(ctx, InsertSessionParams{
			IDHash: digest("session" + string(rune('a'+i))), UserID: u.ID, CSRFHash: digest("csrf"),
			CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.InsertAPIToken(ctx, InsertAPITokenParams{
		UserID: u.ID, Name: "ci", TokenHash: digest("tok1"), Role: RoleAdmin, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	newHash := "pbkdf2-sha256$600000$bmV3LXNhbHQtc2l4dGVlbg$bmV3LWRlcml2ZWQta2V5LWZvci10ZXN0"
	rep, err := a.RotatePassword(ctx, RotatePasswordParams{
		UserID: u.ID, PasswordHash: newHash, Now: now.Add(time.Hour),
		RevokeSessions: true, RevokeAPITokens: true,
	})
	if err != nil {
		t.Fatalf("RotatePassword: %v", err)
	}
	if rep.Sessions != 3 || rep.Tokens != 1 {
		t.Fatalf("revoked %+v, want all three sessions and the one token", rep)
	}
	fresh, err := a.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.PasswordHash != newHash {
		t.Error("the hash did not change")
	}
	if !fresh.PasswordChangedAt.After(u.PasswordChangedAt) {
		t.Errorf("password_changed_at = %s, want it to move past %s", fresh.PasswordChangedAt, u.PasswordChangedAt)
	}
	for _, want := range []struct {
		table, query string
	}{
		{"sessions", "SELECT count(*) FROM sessions WHERE revoked_at IS NULL"},
		{"api_tokens", "SELECT count(*) FROM api_tokens WHERE revoked_at IS NULL"},
	} {
		var n int
		if err := s06DB(t, a).QueryRow(want.query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s live rows after a password change = %d, want 0 (docs/spec/14: stolen old session cookie invalidated)", want.table, n)
		}
	}
	// A rotation that keeps sessions (the parameter-upgrade path in
	// internal/auth) must keep them.
	if _, err := a.InsertSession(ctx, InsertSessionParams{
		IDHash: digest("keeper"), UserID: u.ID, CSRFHash: digest("csrf"),
		CreatedAt: now.Add(time.Hour), ExpiresAt: now.Add(48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if rep, err := a.RotatePassword(ctx, RotatePasswordParams{UserID: u.ID, PasswordHash: hashEnvelope, Now: now.Add(2 * time.Hour)}); err != nil || rep.Sessions != 0 {
		t.Fatalf("an upgrade-path rotation revoked %+v (err %v), want nothing", rep, err)
	}
	live, err := a.SessionByHash(ctx, digest("keeper"))
	if err != nil {
		t.Fatal(err)
	}
	if live.Revoked() {
		t.Error("a hash-parameter upgrade killed a working session")
	}
	if _, err := a.RotatePassword(ctx, RotatePasswordParams{UserID: "nobody", PasswordHash: hashEnvelope, Now: now}); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotating an unknown user err = %v, want ErrNotFound", err)
	}
	if _, err := a.RotatePassword(ctx, RotatePasswordParams{UserID: u.ID, PasswordHash: "plaintext", Now: now}); !errors.Is(err, ErrInvalidPasswordHash) {
		t.Errorf("rotating to a non-envelope err = %v, want ErrInvalidPasswordHash", err)
	}
}

func TestAuthStoreDisableWithdrawsAndEnableDoesNotRestore(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()
	u, _ := a.CreateUser(ctx, CreateUserParams{Username: "dev1", PasswordHash: hashEnvelope, Role: RoleDeveloper, Now: now})
	if _, err := a.InsertSession(ctx, InsertSessionParams{
		IDHash: digest("devsession"), UserID: u.ID, CSRFHash: digest("csrf"), CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.InsertAPIToken(ctx, InsertAPITokenParams{
		UserID: u.ID, Name: "harness", TokenHash: digest("devtok"), Role: RoleDeveloper, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := a.SetUserDisabled(ctx, u.ID, true, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sessions != 1 || rep.Tokens != 1 {
		t.Fatalf("disable revoked %+v, want the session and the token", rep)
	}
	if _, err := a.SetUserDisabled(ctx, u.ID, false, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	enabled, err := a.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.Disabled {
		t.Error("SetUserDisabled(false) did not restore the account")
	}
	if s, err := a.SessionByHash(ctx, digest("devsession")); err != nil || !s.Revoked() {
		t.Error("re-enabling the user resurrected their old session")
	}
	if tok, err := a.APITokenByHash(ctx, digest("devtok")); err != nil || tok.Live(now) {
		t.Error("re-enabling the user resurrected their old token")
	}
	if _, err := a.SetUserDisabled(ctx, "nobody", true, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabling an unknown user err = %v, want ErrNotFound", err)
	}
}

func TestAuthStoreAPITokens(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()
	u, _ := a.CreateUser(ctx, CreateUserParams{Username: "svc", PasswordHash: hashEnvelope, Role: RoleOperator, Now: now})
	raw := "gn_WHOLETOKENVALUENOTSTORED"

	tok, err := a.InsertAPIToken(ctx, InsertAPITokenParams{
		UserID: u.ID, Name: "harness", TokenHash: HashSecret(raw), Role: RoleOperator, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("InsertAPIToken: %v", err)
	}
	if !tok.ExpiresAt.IsZero() {
		t.Error("a token with no expiry must not acquire one")
	}
	if !tok.Live(now.Add(time.Hour)) {
		t.Error("a token with no expiry was reported dead — the zero time is 'never', not the epoch")
	}
	got, err := a.APITokenByHash(ctx, HashSecret(raw))
	if err != nil {
		t.Fatalf("APITokenByHash: %v", err)
	}
	if got.ID != tok.ID || got.Role != RoleOperator {
		t.Fatalf("token = %+v", got)
	}
	if raw2 := s06RawRow(t, s06DB(t, a), "api_tokens"); strings.Contains(raw2, raw) {
		t.Errorf("the api_tokens table contains the raw token: %s", raw2)
	}

	// Expiry.
	expiring, err := a.InsertAPIToken(ctx, InsertAPITokenParams{
		UserID: u.ID, Name: "temp", TokenHash: digest("expiring"), Role: RoleViewer,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !expiring.Live(now.Add(time.Minute)) {
		t.Error("a token was dead before its deadline")
	}
	if expiring.Live(now.Add(2 * time.Hour)) {
		t.Error("a token lived past its deadline")
	}
	if _, err := a.InsertAPIToken(ctx, InsertAPITokenParams{
		UserID: u.ID, Name: "backwards", TokenHash: digest("backwards"), Role: RoleViewer,
		CreatedAt: now, ExpiresAt: now.Add(-time.Hour),
	}); err == nil {
		t.Error("an expires_at in the past was accepted")
	}
	// A duplicate digest is the UNIQUE index's business (two rows cannot resolve
	// from one credential).
	if _, err := a.InsertAPIToken(ctx, InsertAPITokenParams{
		UserID: u.ID, Name: "clone", TokenHash: HashSecret(raw), Role: RoleViewer, CreatedAt: now,
	}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate token digest err = %v, want ErrConflict", err)
	}
	for _, bad := range []struct {
		what string
		p    InsertAPITokenParams
	}{
		{"no name", InsertAPITokenParams{UserID: u.ID, TokenHash: digest("x1"), Role: RoleViewer, CreatedAt: now}},
		{"no owner", InsertAPITokenParams{Name: "n", TokenHash: digest("x2"), Role: RoleViewer, CreatedAt: now}},
		{"bad role", InsertAPITokenParams{UserID: u.ID, Name: "n", TokenHash: digest("x3"), Role: "root", CreatedAt: now}},
		{"raw token as hash", InsertAPITokenParams{UserID: u.ID, Name: "n", TokenHash: "gn_SHORT", Role: RoleViewer, CreatedAt: now}},
	} {
		if _, err := a.InsertAPIToken(ctx, bad.p); err == nil {
			t.Errorf("%s: the insert was accepted", bad.what)
		} else if bad.what == "bad role" && !errors.Is(err, ErrInvalidRole) {
			t.Errorf("bad role err = %v, want ErrInvalidRole", err)
		} else if bad.what == "raw token as hash" && !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("raw-as-hash err = %v, want ErrInvalidSecret", err)
		}
	}

	if err := a.TouchAPIToken(ctx, tok.ID, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	touched, err := a.APITokenByHash(ctx, HashSecret(raw))
	if err != nil {
		t.Fatal(err)
	}
	if touched.LastUsedAt.IsZero() {
		t.Error("TouchAPIToken did not record use — an operator cannot tell a live credential from an idle one")
	}
	if err := a.RevokeAPIToken(ctx, "wrong-owner", tok.ID, now); err != nil {
		t.Fatal(err)
	}
	if still, err := a.APITokenByHash(ctx, HashSecret(raw)); err != nil || !still.Live(now) {
		t.Error("revoking another owner's token id affected this one (the row is scoped by user_id)")
	}
	if err := a.RevokeAPIToken(ctx, u.ID, tok.ID, now); err != nil {
		t.Fatal(err)
	}
	if dead, err := a.APITokenByHash(ctx, HashSecret(raw)); err != nil || dead.Live(now) {
		t.Fatal("RevokeAPIToken did not take effect")
	}
	list, err := a.ListAPITokens(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range list {
		// The list projection deliberately carries no digest (see ListAPITokens).
		if row.TokenHash != "" {
			t.Errorf("ListAPITokens carried a token digest: %q", row.TokenHash)
		}
	}
	if len(list) != 2 {
		t.Fatalf("ListAPITokens = %d rows, want both tokens", len(list))
	}
	n, err := a.RevokeUserTokens(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("RevokeUserTokens = %d, want the one still-live token", n)
	}
}

func TestAuthStorePruneBoundsTheOperationalTables(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()
	u, _ := a.CreateUser(ctx, CreateUserParams{Username: "pruner", PasswordHash: hashEnvelope, Role: RoleViewer, Now: now})

	// One live session, one idle-expired, one past its absolute deadline, one
	// revoked long ago; one live token, one expired.
	sessions := []struct {
		name     string
		created  time.Time
		lastSeen time.Time
		expires  time.Time
		revoked  time.Time
	}{
		{"live", now, now, now.Add(7 * 24 * time.Hour), time.Time{}},
		{"idle", now.Add(-20 * time.Hour), now.Add(-13 * time.Hour), now.Add(7 * 24 * time.Hour), time.Time{}},
		{"absolute", now.Add(-8 * 24 * time.Hour), now.Add(-8 * 24 * time.Hour), now.Add(-time.Hour), time.Time{}},
		{"revoked", now.Add(-30 * 24 * time.Hour), now.Add(-30 * 24 * time.Hour), now.Add(7 * 24 * time.Hour), now.Add(-20 * 24 * time.Hour)},
	}
	for _, c := range sessions {
		hash := digest("prune-" + c.name)
		if _, err := a.InsertSession(ctx, InsertSessionParams{
			IDHash: hash, UserID: u.ID, CSRFHash: digest("c"), CreatedAt: c.created, ExpiresAt: c.expires,
		}); err != nil {
			t.Fatal(err)
		}
		if err := a.TouchSession(ctx, hash, c.lastSeen); err != nil {
			t.Fatal(err)
		}
		if !c.revoked.IsZero() {
			if err := a.RevokeSession(ctx, hash, c.revoked); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		name    string
		expires time.Time
	}{
		{"keep", time.Time{}},
		{"gone", now},
	} {
		if _, err := a.InsertAPIToken(ctx, InsertAPITokenParams{
			UserID: u.ID, Name: c.name, TokenHash: digest("prune-tok-" + c.name), Role: RoleViewer,
			CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: c.expires,
		}); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-200 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		if err := a.RecordAuthAudit(ctx, AuthAuditParams{OccurredAt: old, Event: "login.failure", RemoteAddr: "192.0.2.1:1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.RecordAuthAudit(ctx, AuthAuditParams{OccurredAt: now, Event: "login.success", UserID: u.ID}); err != nil {
		t.Fatal(err)
	}

	rep, err := a.PruneExpired(ctx, now, 12*time.Hour, 90*24*time.Hour, 10)
	if err != nil {
		t.Fatalf("PruneExpired: %v", err)
	}
	if rep.Sessions != 3 {
		t.Errorf("pruned %d sessions, want idle + absolute + long-revoked", rep.Sessions)
	}
	if rep.Tokens != 1 {
		t.Errorf("pruned %d tokens, want the expired one", rep.Tokens)
	}
	if rep.Audit != 3 {
		t.Errorf("pruned %d audit rows, want the three older than the retention window", rep.Audit)
	}
	live, err := a.ListUserSessions(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].IDHash != digest("prune-live") {
		t.Fatalf("surviving sessions = %+v, want only the live one", live)
	}
	audit, err := a.ListAuthAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Event != "login.success" {
		t.Fatalf("surviving audit = %+v", audit)
	}
	// A prune must be safe to repeat and must not be able to eat the batch cap
	// into an unbounded delete.
	if _, err := a.PruneExpired(ctx, now, 12*time.Hour, 90*24*time.Hour, 1); err != nil {
		t.Errorf("second prune: %v", err)
	}
}

func TestAuthStoreAuditShapeAndSecretHygiene(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()
	u, _ := a.CreateUser(ctx, CreateUserParams{Username: "auditee", PasswordHash: hashEnvelope, Role: RoleCurator, Now: now})

	if err := a.RecordAuthAudit(ctx, AuthAuditParams{
		OccurredAt: now, UserID: u.ID, Event: "login.success", RemoteAddr: "127.0.0.1:52000",
		Detail: json.RawMessage(`{"username":"auditee"}`),
	}); err != nil {
		t.Fatalf("RecordAuthAudit: %v", err)
	}
	// No principal established (a failed login for an unknown name) is NULL, not
	// an empty string: 0006's CHECK allows only those two shapes.
	if err := a.RecordAuthAudit(ctx, AuthAuditParams{OccurredAt: now, Event: "login.failure", RemoteAddr: "192.0.2.1:1"}); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := s06DB(t, a).QueryRow(`SELECT user_id IS NULL FROM auth_audit WHERE event = 'login.failure'`).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("an anonymous audit row stored an empty string where 0006 documents NULL")
	}
	for _, bad := range []struct {
		what string
		p    AuthAuditParams
	}{
		{"empty event", AuthAuditParams{Event: ""}},
		{"oversized event", AuthAuditParams{Event: strings.Repeat("e", 101)}},
		{"non-json detail", AuthAuditParams{Event: "login.failure", Detail: json.RawMessage(`not json`)}},
	} {
		if err := a.RecordAuthAudit(ctx, bad.p); err == nil {
			t.Errorf("%s: the audit insert was accepted", bad.what)
		}
	}
	rows, err := a.ListAuthAudit(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first. The fixture clock is frozen, so both rows carry the same
	// occurred_at and only the append-order id (0006's AUTOINCREMENT rowid, which
	// is why the table is not keyed on a time) can order them — `ORDER BY
	// occurred_at DESC` would return these two in either order and an operator
	// reading "the last five auth events" would see them shuffled.
	if len(rows) != 2 || rows[0].Event != "login.failure" || rows[1].Event != "login.success" {
		t.Fatalf("audit rows = %+v", rows)
	}
	if _, err := a.ListAuthAudit(ctx, 0); err != nil {
		t.Errorf("ListAuthAudit(0): %v", err)
	}
	// The user_id is not a foreign key: an audit row must outlive the account it
	// describes (0006 explains why CASCADE/SET NULL would be wrong).
	if _, err := s06DB(t, a).Exec(`DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	n, err := a.ListAuthAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(n) != 2 {
		t.Errorf("deleting the user took their audit history with it (%d rows left)", len(n))
	}
}

func TestAuthStoreCascadesSessionsButNotAudit(t *testing.T) {
	ctx := testCtx(t)
	a := NewAuthStore(migratedStore(t))
	now := authClock()
	u, _ := a.CreateUser(ctx, CreateUserParams{Username: "cascades", PasswordHash: hashEnvelope, Role: RoleViewer, Now: now})
	if _, err := a.InsertSession(ctx, InsertSessionParams{
		IDHash: digest("cascade-session"), UserID: u.ID, CSRFHash: digest("c"), CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.InsertAPIToken(ctx, InsertAPITokenParams{
		UserID: u.ID, Name: "t", TokenHash: digest("cascade-token"), Role: RoleViewer, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordAuthAudit(ctx, AuthAuditParams{OccurredAt: now, UserID: u.ID, Event: "user.create"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s06DB(t, a).Exec(`DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	var sessions, tokens int
	if err := s06DB(t, a).QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := s06DB(t, a).QueryRow(`SELECT count(*) FROM api_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || tokens != 0 {
		t.Errorf("credentials survived their principal (sessions=%d tokens=%d); 0006 requires CASCADE", sessions, tokens)
	}
	var audit int
	if err := s06DB(t, a).QueryRow(`SELECT count(*) FROM auth_audit WHERE user_id = ?`, u.ID).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != 1 {
		t.Errorf("the audit row vanished with the user (%d), want it retained for the trail", audit)
	}
}

func TestHashSecretIsTheOnlyFormStored(t *testing.T) {
	// HashSecret is exported so internal/auth and internal/store cannot disagree
	// about the encoding of a credential digest; a change here breaks every
	// existing session row, which is why it is pinned rather than trusted.
	const token = "gn_ExampleTokenForDigestStability"
	got := HashSecret(token)
	if len(got) != 64 || strings.ToLower(got) != got {
		t.Fatalf("HashSecret = %q, want 64 lowercase hex", got)
	}
	// SHA-256("") — the digest an implementation that hashed nothing, or hashed
	// an empty string, would produce.
	if got == "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal("HashSecret hashed the empty string")
	}
	if err := checkDigest("x", got); err != nil {
		t.Errorf("our own digest fails the schema's CHECK: %v", err)
	}
	if err := checkDigest("x", ""); !errors.Is(err, ErrInvalidSecret) {
		t.Errorf("an empty digest was accepted: %v", err)
	}
	if err := checkDigest("x", strings.ToUpper(got)); !errors.Is(err, ErrInvalidSecret) {
		t.Error("an uppercase digest was accepted; SQLite compares digests case-sensitively, so lookups would miss")
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

// s06DB reaches the pool behind a repository. These tests must be able to write
// rows the Go validators would refuse, because that is the only way to prove the
// schema CHECKs do their job (a constraint nobody can violate is decoration).
func s06DB(t *testing.T, a *AuthStore) *sql.DB {
	t.Helper()
	return a.store.db
}

func s06Exec(t *testing.T, db *sql.DB, query string, args ...any) (sql.Result, error) {
	t.Helper()
	return db.ExecContext(testCtx(t), query, args...)
}

// s06RawRow dumps every text cell of a table, to prove a raw credential is not
// sitting in it next to its digest.
func s06RawRow(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.QueryContext(testCtx(t), "SELECT * FROM "+table)
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			if b, ok := v.([]byte); ok {
				out.Write(b)
			}
			if s, ok := v.(string); ok {
				out.WriteString(s)
			}
			out.WriteByte('|')
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}
