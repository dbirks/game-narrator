package doctor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// ProbeRole is the API's door into the CLI's providers.<role> check
// (ADR-0087): it must answer the same Check the report row would, so the page
// and the CLI cannot disagree about a role.

func TestProbeRoleAnswersTheRoleCheck(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Roles = configuredRoles(providers.RoleFastReasoning, "fake-model")
	h.opts.Probe = &fakeProbe{
		models: []string{"fake-model"},
		chat: func(string, [][]byte) (ChatResult, error) {
			return ChatResult{Text: "pong", Model: "fake-model", Latency: 400 * time.Millisecond}, nil
		},
	}
	c, err := ProbeRole(context.Background(), h.opts, providers.RoleFastReasoning)
	if err != nil {
		t.Fatalf("ProbeRole: %v", err)
	}
	mustStatus(t, c, StatusOK)
	if c.ID != "providers.fast_reasoning" {
		t.Fatalf("check id = %q, want the report row's id", c.ID)
	}
	if c.Duration <= 0 {
		t.Fatal("the probe's measured duration must be positive")
	}
	if !strings.Contains(c.Summary, "answered the chat probe in 400ms") {
		t.Fatalf("summary = %q, want the chat probe's measured answer", c.Summary)
	}
	// The same host, the same role: the report's row and ProbeRole agree.
	cli := h.check(h.run(), "providers.fast_reasoning")
	if cli.Status != c.Status || cli.Summary != c.Summary {
		t.Fatalf("ProbeRole and the report row disagree: %q/%q vs %q/%q",
			cli.Status, cli.Summary, c.Status, c.Summary)
	}
}

func TestProbeRoleUnconfiguredRoleIsNotConfigured(t *testing.T) {
	h := newFakeHost(t)
	// No role env vars: the same NOT CONFIGURED row the report renders.
	c, err := ProbeRole(context.Background(), h.opts, providers.RoleDecision)
	if err != nil {
		t.Fatalf("ProbeRole: %v", err)
	}
	mustStatus(t, c, StatusNotConfigured)
	if !strings.Contains(c.Remedy, "set ") {
		t.Fatalf("the remedy must name the env var: %q", c.Remedy)
	}
}

func TestProbeRoleUnknownRoleIsAnError(t *testing.T) {
	h := newFakeHost(t)
	if _, err := ProbeRole(context.Background(), h.opts, providers.Role("bogus")); err == nil {
		t.Fatal("an unknown role name must be an error, not a Check")
	}
}

func TestProbeRoleCarriesTheFailureTaxonomy(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Roles = configuredRoles(providers.RoleFastReasoning, "fake-model")
	h.opts.Probe = &fakeProbe{
		models: []string{"fake-model"},
		chat: func(string, [][]byte) (ChatResult, error) {
			return ChatResult{}, providers.NewError(providers.KindTimeout, 408, "gateway too slow")
		},
	}
	c, err := ProbeRole(context.Background(), h.opts, providers.RoleFastReasoning)
	if err != nil {
		t.Fatalf("ProbeRole: %v", err)
	}
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindTimeout)) {
		t.Fatalf("summary = %q, want the taxonomy kind", c.Summary)
	}
}

func TestProbeRoleInvalidConfigurationIsAnError(t *testing.T) {
	// A malformed GAME_NARRATOR_* value: the report hides the seven rows
	// behind one FAIL config row; the API door answers the error so the
	// caller can map it onto a documented problem instead of a row that
	// names no role.
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_ROLE_FAST_REASONING_TIMEOUT", "not-a-duration")
	if _, err := ProbeRole(context.Background(), h.opts, providers.RoleFastReasoning); err == nil {
		t.Fatal("an invalid role configuration must be an error")
	}
}
