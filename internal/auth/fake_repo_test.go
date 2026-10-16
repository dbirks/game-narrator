package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/store"
)

// fakeRepo is an in-memory Repo: the same rules the service asks the database
// for, without a database. It exists so the credential state machine (what dies
// when a password changes, which deadline fires first, what a token's ceiling
// does to a role) is testable at second-scale clock jumps — and because the
// SQLite-backed behaviour has its own tests in internal/store
// (auth_store_test.go), so a service test that passed against a lying fake would
// be the only kind of fake this project refuses to trust (AGENTS.md "no
// simulation theater").
type fakeRepo struct {
	mu       sync.Mutex
	users    map[string]*store.User
	sessions map[string]*store.Session
	tokens   map[string]*store.APIToken
	audits   []store.AuthAuditParams
	prunes   int

	// failNext makes one named operation error, to prove the service does not
	// turn a storage fault into an authorization answer.
	failNext string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		users:    map[string]*store.User{},
		sessions: map[string]*store.Session{},
		tokens:   map[string]*store.APIToken{},
	}
}

func (f *fakeRepo) failOn(op string) { f.mu.Lock(); f.failNext = op; f.mu.Unlock() }

func (f *fakeRepo) fail(op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext == op {
		f.failNext = ""
		return errors.New("fake: storage fault in " + op)
	}
	return nil
}

func (f *fakeRepo) CreateUser(_ context.Context, p UserCreation) (*store.User, error) {
	if err := f.fail("CreateUser"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if strings.EqualFold(u.Username, p.Username) {
			return nil, fmt.Errorf("fake: duplicate username: %w", store.ErrConflict)
		}
	}
	now := p.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	u := &store.User{
		ID:                fmt.Sprintf("user-%d", len(f.users)+1),
		Username:          p.Username,
		PasswordHash:      p.PasswordHash,
		Role:              p.Role,
		CreatedAt:         now,
		PasswordChangedAt: now,
	}
	f.users[u.ID] = u
	return u, nil
}

func (f *fakeRepo) UserByID(_ context.Context, id string) (*store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, fmt.Errorf("fake: user %s: %w", id, store.ErrNotFound)
}

func (f *fakeRepo) UserByUsername(_ context.Context, name string) (*store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if strings.EqualFold(u.Username, name) {
			return u, nil
		}
	}
	return nil, fmt.Errorf("fake: user %s: %w", name, store.ErrNotFound)
}

func (f *fakeRepo) ListUsers(context.Context) ([]*store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*store.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeRepo) HasRole(_ context.Context, role string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.Role == role {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRepo) RotatePassword(_ context.Context, p PasswordRotation) (Revocations, error) {
	if err := f.fail("RotatePassword"); err != nil {
		return Revocations{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[p.UserID]
	if !ok {
		return Revocations{}, fmt.Errorf("fake: user %s: %w", p.UserID, store.ErrNotFound)
	}
	now := p.Now
	u.PasswordHash = p.PasswordHash
	u.PasswordChangedAt = now
	var out Revocations
	if p.RevokeSessions {
		for _, s := range f.sessions {
			if s.UserID == p.UserID && s.RevokedAt.IsZero() {
				s.RevokedAt = now
				out.Sessions++
			}
		}
	}
	if p.RevokeAPITokens {
		for _, t := range f.tokens {
			if t.UserID == p.UserID && t.RevokedAt.IsZero() {
				t.RevokedAt = now
				out.Tokens++
			}
		}
	}
	return out, nil
}

func (f *fakeRepo) SetUserDisabled(_ context.Context, userID string, disabled bool, at time.Time) (Revocations, error) {
	if err := f.fail("SetUserDisabled"); err != nil {
		return Revocations{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	if !ok {
		return Revocations{}, fmt.Errorf("fake: user %s: %w", userID, store.ErrNotFound)
	}
	u.Disabled = disabled
	var out Revocations
	if !disabled {
		return out, nil
	}
	for _, s := range f.sessions {
		if s.UserID == userID && s.RevokedAt.IsZero() {
			s.RevokedAt = at
			out.Sessions++
		}
	}
	for _, t := range f.tokens {
		if t.UserID == userID && t.RevokedAt.IsZero() {
			t.RevokedAt = at
			out.Tokens++
		}
	}
	return out, nil
}

func (f *fakeRepo) InsertSession(_ context.Context, p SessionCreation) (*store.Session, error) {
	if err := f.fail("InsertSession"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &store.Session{
		IDHash:     p.IDHash,
		UserID:     p.UserID,
		CSRFHash:   p.CSRFHash,
		CreatedAt:  p.CreatedAt,
		LastSeenAt: p.CreatedAt,
		ExpiresAt:  p.ExpiresAt,
		UserAgent:  p.UserAgent,
	}
	f.sessions[s.IDHash] = s
	return s, nil
}

func (f *fakeRepo) SessionByHash(_ context.Context, hash string) (*store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[hash]; ok {
		copied := *s
		return &copied, nil
	}
	return nil, fmt.Errorf("fake: session: %w", store.ErrNotFound)
}

func (f *fakeRepo) TouchSession(_ context.Context, hash string, seen time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[hash]; ok {
		s.LastSeenAt = seen
	}
	return nil
}

func (f *fakeRepo) RevokeSession(_ context.Context, hash string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[hash]; ok {
		s.RevokedAt = at
	}
	return nil
}

func (f *fakeRepo) RevokeUserSessions(_ context.Context, userID string, at time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, s := range f.sessions {
		if s.UserID == userID && s.RevokedAt.IsZero() {
			s.RevokedAt = at
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) ListUserSessions(_ context.Context, userID string) ([]*store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*store.Session
	for _, s := range f.sessions {
		if s.UserID == userID {
			copied := *s
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (f *fakeRepo) InsertAPIToken(_ context.Context, p TokenCreation) (*store.APIToken, error) {
	if err := f.fail("InsertAPIToken"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &store.APIToken{
		ID:        fmt.Sprintf("tok-%d", len(f.tokens)+1),
		UserID:    p.UserID,
		Name:      p.Name,
		TokenHash: p.TokenHash,
		Role:      p.Role,
		CreatedAt: p.CreatedAt,
		ExpiresAt: p.ExpiresAt,
	}
	f.tokens[t.TokenHash] = t
	return t, nil
}

func (f *fakeRepo) APITokenByHash(_ context.Context, hash string) (*store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tokens[hash]; ok {
		copied := *t
		return &copied, nil
	}
	return nil, fmt.Errorf("fake: token: %w", store.ErrNotFound)
}

func (f *fakeRepo) TouchAPIToken(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tokens {
		if t.ID == id {
			t.LastUsedAt = at
		}
	}
	return nil
}

func (f *fakeRepo) RevokeAPIToken(_ context.Context, userID, tokenID string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tokens {
		if t.ID == tokenID && t.UserID == userID {
			t.RevokedAt = at
		}
	}
	return nil
}

func (f *fakeRepo) RevokeUserTokens(_ context.Context, userID string, at time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for _, t := range f.tokens {
		if t.UserID == userID && t.RevokedAt.IsZero() {
			t.RevokedAt = at
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) ListAPITokens(_ context.Context, userID string) ([]*store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*store.APIToken
	for _, t := range f.tokens {
		if t.UserID == userID {
			copied := *t
			copied.TokenHash = "" // the real projection omits the digest
			out = append(out, &copied)
		}
	}
	return out, nil
}

func (f *fakeRepo) PruneExpired(_ context.Context, now time.Time, idle, retain time.Duration, _ int) (PruneReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prunes++
	var out PruneReport
	for h, s := range f.sessions {
		if s.ExpiresAt.Before(now) || s.RevokedAt.IsZero() == false || now.Sub(s.LastSeenAt) >= idle {
			delete(f.sessions, h)
			out.Sessions++
		}
	}
	return out, nil
}

func (f *fakeRepo) RecordAuthAudit(_ context.Context, p store.AuthAuditParams) error {
	if err := f.fail("RecordAuthAudit"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audits = append(f.audits, p)
	return nil
}

// auditEvents returns the recorded event names, oldest first.
func (f *fakeRepo) auditEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.audits))
	for _, a := range f.audits {
		out = append(out, a.Event)
	}
	return out
}

// lastAudit returns the newest audit row.
func (f *fakeRepo) lastAudit() store.AuthAuditParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.audits) == 0 {
		return store.AuthAuditParams{}
	}
	return f.audits[len(f.audits)-1]
}

// userByID is a test accessor that ignores the error a real repo would return.
func (f *fakeRepo) userByID(id string) *store.User {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[id]
}

var _ Repo = (*fakeRepo)(nil)
