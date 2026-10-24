package cast

import (
	"testing"
)

// The keyword regex cache must be safe for concurrent use: the cast audit
// endpoint (ADR-0171) runs ExpectedClass per roster row under the server's
// request goroutines, so two requests racing on the first cache fill must
// not corrupt the shared map. Run with -race.
func TestExpectedClassConcurrentCacheFill(t *testing.T) {
	const n = 32
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			// Alternate keywords so distinct goroutines fill distinct
			// entries while others read the map.
			_ = ExpectedClass("a male man who is a boy")
			_ = ExpectedClass("a female woman who is a girl")
			_ = ExpectedClass("the kid in the wardrobe")
			_ = ExpectedClass("an old creature")
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
}
