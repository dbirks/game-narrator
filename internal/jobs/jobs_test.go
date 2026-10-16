package jobs

import (
	"errors"
	"fmt"
	"testing"
)

// TestPermanentClassification pins the error classifier: only the explicit
// Permanent wrapper suppresses retry. A queue that retried deterministic
// failures would burn the whole budget on the same bad input while a live
// session waits behind it.
func TestPermanentClassification(t *testing.T) {
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must stay nil")
	}
	cause := errors.New("source hash unknown")
	if !IsPermanent(Permanent(cause)) {
		t.Fatal("Permanent error not classified as permanent")
	}
	if IsPermanent(fmt.Errorf("wrapped: %w", cause)) {
		t.Fatal("an ordinary wrapped error must stay retryable")
	}
	if !IsPermanent(fmt.Errorf("layer: %w", Permanent(cause))) {
		t.Fatal("permanence must survive one more wrap")
	}
	if IsPermanent(nil) {
		t.Fatal("nil is not permanent")
	}
}
