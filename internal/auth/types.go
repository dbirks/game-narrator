package auth

import (
	"github.com/dbirks/game-narrator/internal/store"
)

// The service speaks these aliases rather than store.X directly, so that a fake
// Repo in a unit test can satisfy the interface without the test importing
// anything database-shaped, and so internal/api has one vocabulary for auth
// objects. They are type *aliases* (not wrappers): the same struct, one name at
// the boundary that matters.
type (
	// UserCreation is store.CreateUserParams.
	UserCreation = store.CreateUserParams
	// SessionCreation is store.InsertSessionParams.
	SessionCreation = store.InsertSessionParams
	// TokenCreation is store.InsertAPITokenParams.
	TokenCreation = store.InsertAPITokenParams
	// PasswordRotation is store.RotatePasswordParams.
	PasswordRotation = store.RotatePasswordParams
	// StoredUser is a users row.
	StoredUser = store.User
	// StoredSession is a sessions row (digests only, never a token).
	StoredSession = store.Session
	// StoredToken is an api_tokens row (digest only).
	StoredToken = store.APIToken
	// Revocations counts what a password change or disable voided.
	Revocations = store.RevocationReport
	// PruneReport counts what retention deleted.
	PruneReport = store.PruneAuthReport
	// AuditRecord is store.AuthAuditParams.
	AuditRecord = store.AuthAuditParams
)

// ErrNoSuchUser is what a lookup returns for a missing row. It is re-exported so
// the auth package's error vocabulary (ErrInvalidCredentials, ErrUnknownSession)
// can be decided without importing store at every branch.
var ErrNoSuchUser = store.ErrNotFound
