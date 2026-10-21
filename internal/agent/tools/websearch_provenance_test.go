package tools

// Regression fixture (category (a) synthetic stubs, AGENTS.md rule 3) for the
// store-row contract the live web-proposal gate asserts, made deterministic
// without the gateway.
//
// The live TestLiveWebSourceProposalForPublicTitle once fataled on ANY
// web_page row without a raw-body hash — but web_search with profile_id
// files its hits as provenance-only rows by contract: the search provider
// returns URLs and snippets, no page body, so those rows carry
// ContentSHA256 '' (docs/spec/01: "'' = no addressable payload"). The model
// legitimately files the hits on the profile, so the ONLY row that must be
// pinnable (URL + raw-body hash) is the one propose_web_import acts on;
// pinnability is enforced at the propose boundary, which refuses a hashless
// source ("an approval that cannot name the exact bytes is not an approval").
//
// This test drives the real tool set — the fake search provider plus the
// real policy-gated fetcher against a local fixture page — into exactly the
// live-run shape that exposed the over-strict assertion (the search hit is
// filed first, then the fetch of the same URL registers the pinnable row,
// then the proposal targets the fetched row), and pins the resulting row
// shape so a contract drift in web_search/fetch_page/propose_web_import
// fails here, deterministically, without the gateway.

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/agent"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/web/search"
)

func TestWebSearchProvenanceRowsStayPinnableAtTheProposeBoundary(t *testing.T) {
	e := newEnv(t)
	g := e.game("isle-dump", "Isle Dump")
	p := e.draft(e.edition(g, "US", "en", "v1").ID)

	s, _ := newDumpServer(t, []byte(dumpText))
	f := newDumpFetcher(t, s)
	url := serverURL(s, "/dump.txt")

	// One canned hit pointing at the fixture page: the search provider
	// returns the URL and a snippet, never the body.
	fs := &fakeSearch{results: []search.Result{{
		Title: "Isle Dump — lighthouse keeper script (synthetic)", URL: url,
		Snippet: "Invented dialogue text", Source: "fake-search",
		RetrievedAt: time.Now().UTC(),
	}}}
	e.tools(fs, f)
	exec := agent.NewExecutor(e.reg, auth.RoleOperator, 16)

	// 1. The model files the search hit on the profile (provenance-only row).
	res := exec.Execute(e.ctx, agent.Call{
		ID: "search-1", Name: "web_search",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"query":"isle dump script","profile_id":%q}`, p.ID)),
	})
	if res.Status != agent.StatusSucceeded {
		t.Fatalf("web_search = %s: %s", res.Status, res.Error)
	}

	// 2. The model fetches the page it found and registers the pinnable row.
	srcID := e.fetchDump(t, exec, p.ID, url)

	// 3. The proposal targets the fetched row and stops at the review
	// boundary; it must refuse if the target were the hashless search row.
	prop := exec.Execute(e.ctx, agent.Call{
		ID: "propose-1", Name: "propose_web_import",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"profile_id":%q,"source_id":%q}`, p.ID, srcID)),
	})
	if prop.Status != agent.StatusSucceeded || !prop.ReviewRequired {
		t.Fatalf("propose_web_import = %s review_required=%v: %s",
			prop.Status, prop.ReviewRequired, prop.Error)
	}
	// And a proposal of the hashless search row must be refused — that is
	// the boundary the assertion below encodes at the store level.
	hitID := ""
	sources, err := e.store.ListSources(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	for _, s := range sources {
		if s.SourceType == games.SourceWebPage && s.ContentSHA256 == "" {
			hitID = s.ID
		}
	}
	if hitID == "" {
		t.Fatal("no hashless web_page row: the search hit was not filed as a provenance-only row")
	}
	if r := exec.Execute(e.ctx, agent.Call{
		ID: "propose-hashless", Name: "propose_web_import",
		Arguments: json.RawMessage(fmt.Sprintf(
			`{"profile_id":%q,"source_id":%q}`, p.ID, hitID)),
	}); r.Status == agent.StatusSucceeded {
		t.Fatal("propose_web_import accepted a hashless search-hit row: the proposal boundary no longer enforces pinnability")
	}

	// The row shape the live test asserts: every web_page row carries its
	// URL; exactly one row is hashless (the filed search hit, provenance-only
	// by contract) and the proposed row is the fetched page, which is
	// pinnable.
	webPages, proposed, hashless := 0, 0, 0
	for _, s := range sources {
		if s.SourceType != games.SourceWebPage {
			continue
		}
		webPages++
		if s.URI == "" {
			t.Fatalf("web source row %s is missing its URL", s.ID)
		}
		switch {
		case s.ID == srcID:
			proposed++
			if len(s.ContentSHA256) != 64 {
				t.Fatalf("the proposed row %s has no 64-hex-char raw-body hash: the proposal would be unpinnable", s.ID)
			}
		case s.ContentSHA256 == "":
			hashless++
		}
	}
	if webPages != 2 {
		t.Fatalf("web_page rows = %d, want 2 (one hashless search hit + the fetched page)", webPages)
	}
	if proposed != 1 {
		t.Fatalf("proposed source rows = %d, want 1", proposed)
	}
	if hashless != 1 {
		t.Fatalf("hashless web_page rows = %d, want 1 (the filed search hit)", hashless)
	}

	// Nothing was imported: no messages, draft status.
	msgs, err := e.deps.listMessages(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("listMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("%d messages after a proposal-only run: nothing may be imported before the approval", len(msgs))
	}
	final, _, err := e.store.GetProfile(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if final.Status != games.StatusDraft {
		t.Fatalf("profile status = %s, want draft (the proposal must not publish)", final.Status)
	}
	t.Logf("web provenance shape: rows=%d (hashless=%d pinnable=%d) proposed=%s",
		hashless+proposed, hashless, proposed, srcID)
}
