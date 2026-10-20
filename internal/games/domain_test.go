package games

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func frac(t *testing.T, v float64) *float64 {
	t.Helper()
	return &v
}

func ptr[T any](v T) *T { return &v }

//
// Valid rows: the happy path for every type, built with fresh ids. If one of
// these stops validating, every rejection case below it stops meaning anything.
//

func TestValidRows(t *testing.T) {
	charID, sceneID, msgID, srcID := NewID(), NewID(), NewID(), NewID()

	rows := []interface{ Validate() error }{
		Game{ID: NewID(), Slug: "synthetic-title", Title: "Synthetic Title",
			Platform: "gamecube", UserTags: raw(`["synthetic"]`), CreatedAt: nowStamp()},
		Edition{ID: NewID(), GameID: NewID(), Region: "US", Language: "en",
			Revision: "rev0", Locale: "en_US", DisplayAspect: frac(t, 1.3333)},
		Profile{ID: NewID(), EditionID: NewID(), RevisionNum: 1, Status: StatusDraft},
		Profile{ID: NewID(), EditionID: NewID(), RevisionNum: 2, ParentRevision: ptr(1),
			Status: StatusPublished, ManifestDigest: strings.Repeat("a", 64),
			PublishedAt: nowStamp(), PublicationAuthor: "curator"},
		Source{ID: srcID, ProfileID: NewID(), SourceType: SourceUserFile,
			URI: "file:///user/supplied/script.csv", RetrievedAt: nowStamp(),
			ContentSHA256: strings.Repeat("b", 64), Rights: RightsUserSupplied,
			Trust: 1, Parser: "generic-csv"},
		Message{ID: msgID, ProfileID: NewID(), SourceID: srcID, SourceMessageID: "bmg-142",
			OriginalText: "Hello there!", MarkupJSON: raw(`[]`), NormalizedText: "hello there",
			SpeechText: "Hello there!", Language: "en", TextKind: TextDialogue,
			SceneID: sceneID, SpeakerID: charID, AttributionStatus: AttributionConfirmed,
			AttributionConfidence: 0.95, AttributionEvidence: raw(`[{"method":"script_order"}]`),
			Flags: raw(`{"spoiler":false}`), Revision: 1, CreatedAt: nowStamp(), UpdatedAt: nowStamp()},
		Message{ID: NewID(), ProfileID: NewID(), OriginalText: "A sign",
			NormalizedText: "a sign", TextKind: TextSign,
			AttributionStatus: AttributionUnknown, Revision: 1,
			CreatedAt: nowStamp(), UpdatedAt: nowStamp()},
		MessagePage{MessageID: msgID, Ordinal: 1, PageText: "Hello there!",
			VisiblePrefixes: raw(`["hello"]`), DisplayControls: raw(`["A"]`),
			TimingJSON: raw(`{"ms_per_char":40}`)},
		MessageEdge{FromID: msgID, ToID: NewID(), EdgeType: EdgeNext, Confidence: 1,
			Evidence: raw(`[{"source":"bmg"}]`), CreatedAt: nowStamp()},
		MessageEdge{FromID: msgID, ToID: NewID(), EdgeType: EdgeContextual,
			ConditionExpr: "flags.has_herosuit", Confidence: 0.8, CreatedAt: nowStamp()},
		MessageEdge{FromID: msgID, ToID: msgID, EdgeType: EdgeRepeat, Confidence: 1,
			CreatedAt: nowStamp()},
		MessageEdge{FromID: msgID, ToID: NewID(), EdgeType: EdgeInferred, Confidence: 0.6,
			CreatedAt: nowStamp()},
		Character{ID: charID, ProfileID: NewID(), Name: "Hero", Aliases: raw(`["Link"]`),
			VisualDescriptors: raw(`["blue tunic"]`), PersonalitySummary: "brave",
			Role: "player_one", VerifiedSourceRefs: raw(`["src"]`), Status: CharacterConfirmed,
			CreatedAt: nowStamp()},
		Scene{ID: sceneID, ProfileID: NewID(), Name: "Deck", Aliases: raw(`[]`),
			RegionFingerprint: "fp-1", VisualAnchors: raw(`[]`), ProgressJSON: raw(`{}`),
			CreatedAt: nowStamp()},
		Region{ID: NewID(), ProfileID: NewID(), RegionType: RegionTextbox,
			DetectMethod: DetectManual, RectX: 0.1, RectY: 0.7, RectW: 0.8, RectH: 0.2,
			MinAspect: frac(t, 1.3), MaxAspect: frac(t, 1.7778), OCRSettings: raw(`{}`),
			MaskAssetIDs: raw(`[]`), Reliability: frac(t, 0.95), Revision: 1,
			UpdatedAt: nowStamp()},
		Region{ID: NewID(), ProfileID: NewID(), RegionType: RegionSign, DetectMethod: DetectAutoDiscovered,
			RectX: 0, RectY: 0, RectW: 1, RectH: 1, OCRSettings: raw(`{}`), Revision: 1,
			UpdatedAt: nowStamp()},
		SpeakerAssertion{ID: NewID(), ProfileID: NewID(), MessageID: msgID, CharacterID: charID,
			Method: AssertionOnScreenLabel, AssertedAt: nowStamp(), Evidence: raw(`[]`),
			Posterior: 0.7},
		SpeakerAssertion{ID: NewID(), ProfileID: NewID(), MessageID: msgID, CharacterID: charID,
			Method: AssertionManualCorrection, AssertedAt: nowStamp(), Posterior: 1,
			Reviewer: "curator@example"},
		Correction{ID: NewID(), ProfileID: NewID(), Kind: CorrectionSpeaker,
			TargetType: TargetMessage, TargetID: msgID, MessageID: msgID, BeforeValue: "Old",
			AfterValue: "Hero", Actor: "curator@example", Evidence: raw(`[]`),
			Status: CorrectionAccepted, Reviewer: "admin", ReviewedAt: nowStamp(),
			CreatedAt: nowStamp()},
	}

	for i, row := range rows {
		if err := row.Validate(); err != nil {
			t.Errorf("rows[%d] %T: %v", i, row, err)
		}
	}
}

//
// Rejections: one bad field at a time, each naming the field it complained
// about, so a UI can highlight the right input (docs/spec/09).
//

func TestValidateRejects(t *testing.T) {
	valid := func() Message {
		return Message{ID: NewID(), ProfileID: NewID(), OriginalText: "Hello there!",
			NormalizedText: "hello there", TextKind: TextDialogue,
			AttributionStatus: AttributionUnknown, Revision: 1,
			CreatedAt: nowStamp(), UpdatedAt: nowStamp()}
	}

	t.Run("Message", func(t *testing.T) {
		cases := []struct {
			name    string
			corrupt func(*Message)
			field   string
		}{
			{"missing id", func(m *Message) { m.ID = "" }, "Message.id"},
			{"malformed id", func(m *Message) { m.ID = "abc" }, "Message.id"},
			{"missing profile", func(m *Message) { m.ProfileID = "" }, "Message.profileID"},
			{"empty original", func(m *Message) { m.OriginalText = "   " }, "Message.originalText"},
			{"empty normalized", func(m *Message) { m.NormalizedText = "" }, "Message.normalizedText"},
			{"unknown text kind", func(m *Message) { m.TextKind = TextKind("cutscene") }, "Message.textKind"},
			{"unknown attribution", func(m *Message) { m.AttributionStatus = AttributionStatus("guessed") }, "Message.attributionStatus"},
			{"confidence above one", func(m *Message) { m.AttributionConfidence = 1.4 }, "Message.attributionConfidence"},
			{"confidence below zero", func(m *Message) { m.AttributionConfidence = -0.4 }, "Message.attributionConfidence"},
			{"revision zero", func(m *Message) { m.Revision = 0 }, "Message.revision"},
			{"bad markup json", func(m *Message) { m.MarkupJSON = raw("not json") }, "Message.markupJSON"},
			{"markup json as object", func(m *Message) { m.MarkupJSON = raw("{}") }, "Message.markupJSON"},
			{"flags as array", func(m *Message) { m.Flags = raw("[]") }, "Message.flags"},
			{"flags null", func(m *Message) { m.Flags = raw("null") }, "Message.flags"},
			{"malformed scene ref", func(m *Message) { m.SceneID = "scene name" }, "Message.sceneID"},
			{"malformed speaker ref", func(m *Message) { m.SpeakerID = "1" }, "Message.speakerID"},
			{"source without identity", func(m *Message) { m.SourceID = NewID() }, "Message.sourceMessageID"},
			{"identity with a space", func(m *Message) {
				m.SourceMessageID = "row 12"
			}, "Message.sourceMessageID"},
			{"identity too long", func(m *Message) { m.SourceMessageID = strings.Repeat("x", 121) }, "Message.sourceMessageID"},
			{"speaker set while rejected", func(m *Message) {
				m.SpeakerID, m.AttributionStatus = NewID(), AttributionRejected
			}, "Message.speakerID"},
			{"missing created at", func(m *Message) { m.CreatedAt = "" }, "Message.createdAt"},
			{"sqlite-only date", func(m *Message) { m.UpdatedAt = "2026-10-16" }, "Message.updatedAt"},
			{"clock time without offset", func(m *Message) { m.UpdatedAt = "2026-10-16T12:00:00" }, "Message.updatedAt"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := valid()
				tc.corrupt(&m)
				requireFieldError(t, m.Validate(), tc.field)
			})
		}
	})

	t.Run("Game", func(t *testing.T) {
		base := func() Game {
			return Game{ID: NewID(), Slug: "ok-slug", Title: "Ok", Platform: "gamecube",
				UserTags: raw(`[]`), CreatedAt: nowStamp()}
		}
		cases := []struct {
			name    string
			corrupt func(*Game)
			field   string
		}{
			{"empty slug", func(g *Game) { g.Slug = "" }, "Game.slug"},
			{"uppercase slug", func(g *Game) { g.Slug = "WindWaker" }, "Game.slug"},
			{"slug with slash", func(g *Game) { g.Slug = "a/b" }, "Game.slug"},
			{"slug with dot", func(g *Game) { g.Slug = "a.b" }, "Game.slug"},
			{"slug with space", func(g *Game) { g.Slug = "a b" }, "Game.slug"},
			{"leading dash slug", func(g *Game) { g.Slug = "-a" }, "Game.slug"},
			{"trailing dash slug", func(g *Game) { g.Slug = "a-" }, "Game.slug"},
			{"empty title", func(g *Game) { g.Title = " " }, "Game.title"},
			{"tags not json", func(g *Game) { g.UserTags = raw("[oops]") }, "Game.userTags"},
			{"tags as object", func(g *Game) { g.UserTags = raw("{}") }, "Game.userTags"},
			{"no created at", func(g *Game) { g.CreatedAt = "" }, "Game.createdAt"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				g := base()
				tc.corrupt(&g)
				requireFieldError(t, g.Validate(), tc.field)
			})
		}
	})

	t.Run("Profile", func(t *testing.T) {
		base := func() Profile {
			return Profile{ID: NewID(), EditionID: NewID(), RevisionNum: 1, Status: StatusDraft}
		}
		cases := []struct {
			name    string
			corrupt func(*Profile)
			field   string
		}{
			{"revision zero", func(p *Profile) { p.RevisionNum = 0 }, "Profile.revisionNum"},
			{"negative parent", func(p *Profile) { p.ParentRevision = ptr(-1) }, "Profile.parentRevision"},
			{"unknown status", func(p *Profile) { p.Status = ProfileStatus("pending") }, "Profile.status"},
			{"published without time", func(p *Profile) {
				p.Status, p.ManifestDigest = StatusPublished, strings.Repeat("c", 64)
			}, "Profile.publishedAt"},
			{"published without digest", func(p *Profile) {
				p.Status, p.PublishedAt = StatusPublished, nowStamp()
			}, "Profile.manifestDigest"},
			{"draft with a publication time", func(p *Profile) { p.PublishedAt = nowStamp() }, "Profile.publishedAt"},
			{"digest not hex", func(p *Profile) {
				p.Status, p.PublishedAt = StatusPublished, nowStamp()
				p.ManifestDigest = strings.Repeat("z", 64)
			}, "Profile.manifestDigest"},
			{"digest short", func(p *Profile) {
				p.Status, p.PublishedAt = StatusPublished, nowStamp()
				p.ManifestDigest = "abc"
			}, "Profile.manifestDigest"},
			{"malformed voice ref", func(p *Profile) { p.PreferredVoiceID = "voice-1" }, "Profile.preferredVoiceID"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				p := base()
				tc.corrupt(&p)
				requireFieldError(t, p.Validate(), tc.field)
			})
		}
	})

	t.Run("Source", func(t *testing.T) {
		base := func() Source {
			return Source{ID: NewID(), ProfileID: NewID(), SourceType: SourceUserFile,
				URI: "file:///user/supplied.csv", RetrievedAt: nowStamp(),
				ContentSHA256: strings.Repeat("d", 64), Rights: RightsUserSupplied, Trust: 1}
		}
		cases := []struct {
			name    string
			corrupt func(*Source)
			field   string
		}{
			{"unknown source type", func(s *Source) { s.SourceType = SourceType("torrent") }, "Source.sourceType"},
			{"no uri", func(s *Source) { s.URI = "" }, "Source.uri"},
			{"trust above one", func(s *Source) { s.Trust = 2 }, "Source.trust"},
			{"trust below zero", func(s *Source) { s.Trust = -1 }, "Source.trust"},
			{"sha uppercase", func(s *Source) { s.ContentSHA256 = strings.Repeat("D", 64) }, "Source.contentSHA256"},
			{"sha wrong length", func(s *Source) { s.ContentSHA256 = "abc" }, "Source.contentSHA256"},
			{"unknown rights on a user file", func(s *Source) { s.Rights = RightsUnknown }, "Source.rights"},
			{"bad retrieved at", func(s *Source) { s.RetrievedAt = "last week" }, "Source.retrievedAt"},
			{"unknown rights type", func(s *Source) { s.Rights = RightsStatus("public") }, "Source.rights"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s := base()
				tc.corrupt(&s)
				requireFieldError(t, s.Validate(), tc.field)
			})
		}
	})

	t.Run("MessageEdge", func(t *testing.T) {
		base := func() MessageEdge {
			return MessageEdge{FromID: NewID(), ToID: NewID(), EdgeType: EdgeNext,
				Confidence: 1, CreatedAt: nowStamp()}
		}
		cases := []struct {
			name    string
			corrupt func(*MessageEdge)
			field   string
		}{
			{"missing from", func(e *MessageEdge) { e.FromID = "" }, "MessageEdge.fromID"},
			{"self edge as next", func(e *MessageEdge) { e.ToID = e.FromID }, "MessageEdge.toID"},
			{"unknown type", func(e *MessageEdge) { e.EdgeType = EdgeType("maybe") }, "MessageEdge.edgeType"},
			{"inferred edge at full confidence", func(e *MessageEdge) {
				e.EdgeType, e.Confidence = EdgeInferred, 0.95
			}, "MessageEdge.confidence"},
			{"contextual without condition", func(e *MessageEdge) { e.EdgeType = EdgeContextual }, "MessageEdge.conditionExpr"},
			{"condition too long", func(e *MessageEdge) {
				e.EdgeType, e.ConditionExpr = EdgeContextual, strings.Repeat("a", MaxConditionExpr+1)
			}, "MessageEdge.conditionExpr"},
			{"confidence out of range", func(e *MessageEdge) { e.Confidence = -0.1 }, "MessageEdge.confidence"},
			{"evidence not json", func(e *MessageEdge) { e.Evidence = raw("nope") }, "MessageEdge.evidence"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				e := base()
				tc.corrupt(&e)
				requireFieldError(t, e.Validate(), tc.field)
			})
		}
	})

	t.Run("Region", func(t *testing.T) {
		base := func() Region {
			return Region{ID: NewID(), ProfileID: NewID(), RegionType: RegionTextbox,
				DetectMethod: DetectManual, RectX: 0.1, RectY: 0.7, RectW: 0.8, RectH: 0.2,
				Revision: 1, UpdatedAt: nowStamp()}
		}
		cases := []struct {
			name    string
			corrupt func(*Region)
			field   string
		}{
			{"negative x", func(r *Region) { r.RectX = -0.01 }, "Region.rectX"},
			{"width above one", func(r *Region) { r.RectW = 1.1 }, "Region.rectW"},
			{"negative height", func(r *Region) { r.RectH = -0.5 }, "Region.rectH"},
			{"box past the right edge", func(r *Region) { r.RectX, r.RectW = 0.5, 0.6 }, "Region.rectW"},
			{"box past the bottom edge", func(r *Region) { r.RectY, r.RectH = 0.9, 0.2 }, "Region.rectH"},
			{"aspect band inverted", func(r *Region) {
				r.MinAspect, r.MaxAspect = frac(t, 1.8), frac(t, 1.3)
			}, "Region.maxAspect"},
			{"zero aspect", func(r *Region) { r.MinAspect = frac(t, 0) }, "Region.minAspect"},
			{"zero min width", func(r *Region) { r.MinWidthPx = ptr(0) }, "Region.minWidthPx"},
			{"reliability above one", func(r *Region) { r.Reliability = frac(t, 1.5) }, "Region.reliability"},
			{"unknown region type", func(r *Region) { r.RegionType = RegionType("speech_bubble") }, "Region.regionType"},
			{"unknown detect method", func(r *Region) { r.DetectMethod = DetectMethod("guessed") }, "Region.detectMethod"},
			{"ocr settings as array", func(r *Region) { r.OCRSettings = raw("[]") }, "Region.ocrSettings"},
			{"revision zero", func(r *Region) { r.Revision = 0 }, "Region.revision"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				r := base()
				tc.corrupt(&r)
				requireFieldError(t, r.Validate(), tc.field)
			})
		}
	})

	t.Run("SpeakerAssertion", func(t *testing.T) {
		base := func() SpeakerAssertion {
			return SpeakerAssertion{ID: NewID(), ProfileID: NewID(), MessageID: NewID(),
				CharacterID: NewID(), Method: AssertionModel, AssertedAt: nowStamp(),
				Posterior: 0.6}
		}
		cases := []struct {
			name    string
			corrupt func(*SpeakerAssertion)
			field   string
		}{
			{"unknown method", func(a *SpeakerAssertion) { a.Method = AssertionMethod("vibes") }, "SpeakerAssertion.method"},
			{"posterior above one", func(a *SpeakerAssertion) { a.Posterior = 1.1 }, "SpeakerAssertion.posterior"},
			{"manual without reviewer", func(a *SpeakerAssertion) {
				a.Method, a.Reviewer = AssertionManual, ""
			}, "SpeakerAssertion.reviewer"},
			{"automated with reviewer", func(a *SpeakerAssertion) {
				a.Method, a.Reviewer = AssertionModel, "someone"
			}, "SpeakerAssertion.reviewer"},
			{"missing message", func(a *SpeakerAssertion) { a.MessageID = "" }, "SpeakerAssertion.messageID"},
			{"evidence not json", func(a *SpeakerAssertion) { a.Evidence = raw("shot-1") }, "SpeakerAssertion.evidence"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				a := base()
				tc.corrupt(&a)
				requireFieldError(t, a.Validate(), tc.field)
			})
		}
	})

	t.Run("Correction", func(t *testing.T) {
		base := func() Correction {
			return Correction{ID: NewID(), ProfileID: NewID(), Kind: CorrectionSpeaker,
				TargetType: TargetMessage, TargetID: NewID(), Actor: "curator",
				Status: CorrectionProposed, CreatedAt: nowStamp()}
		}
		cases := []struct {
			name    string
			corrupt func(*Correction)
			field   string
		}{
			{"unknown kind", func(c *Correction) { c.Kind = CorrectionKind("vibe") }, "Correction.kind"},
			{"unknown target", func(c *Correction) { c.TargetType = CorrectionTarget("universe") }, "Correction.targetType"},
			{"no target id", func(c *Correction) { c.TargetID = "" }, "Correction.targetID"},
			{"anonymous actor", func(c *Correction) { c.Actor = "  " }, "Correction.actor"},
			{"accepted without reviewer", func(c *Correction) {
				c.Status, c.AfterValue = CorrectionAccepted, "Hero"
			}, "Correction.reviewer"},
			{"accepted with review time but no reviewer", func(c *Correction) {
				c.Status, c.AfterValue, c.ReviewedAt = CorrectionAccepted, "Hero", nowStamp()
			}, "Correction.reviewer"},
			{"proposed with a reviewer", func(c *Correction) { c.Reviewer = "admin" }, "Correction.status"},
			{"accepted with nothing to apply", func(c *Correction) {
				c.Status, c.Reviewer, c.ReviewedAt = CorrectionAccepted, "admin", nowStamp()
			}, "Correction.afterValue"},
			{"region kind on a message target", func(c *Correction) {
				c.Kind, c.TargetType = CorrectionRegion, TargetMessage
			}, "Correction.targetType"},
			{"message id disagreeing with target", func(c *Correction) {
				c.MessageID = NewID()
			}, "Correction.messageID"},
			{"unknown status", func(c *Correction) { c.Status = CorrectionStatus("applied") }, "Correction.status"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := base()
				tc.corrupt(&c)
				requireFieldError(t, c.Validate(), tc.field)
			})
		}
	})

	t.Run("Character/Scene/Page", func(t *testing.T) {
		char := func() Character {
			return Character{ID: NewID(), ProfileID: NewID(), Name: "Hero",
				Status: CharacterProposed, CreatedAt: nowStamp()}
		}
		c := char()
		c.PortraitAssetID = "abc123"
		requireFieldError(t, c.Validate(), "Character.portraitAssetID")

		c = char()
		c.Status = CharacterStatus("merged")
		requireFieldError(t, c.Validate(), "Character.status")

		scene := func() Scene {
			return Scene{ID: NewID(), ProfileID: NewID(), Name: "Deck", CreatedAt: nowStamp()}
		}
		s := scene()
		s.ProgressJSON = raw("0.4")
		requireFieldError(t, s.Validate(), "Scene.progressJSON")

		s = scene()
		s.Aliases = raw("Deck, Ship")
		requireFieldError(t, s.Validate(), "Scene.aliases")

		page := MessagePage{MessageID: NewID(), Ordinal: 1, PageText: "text"}
		page.Ordinal = 0
		requireFieldError(t, page.Validate(), "MessagePage.ordinal")

		page = MessagePage{MessageID: NewID(), Ordinal: 1, PageText: "text", VisiblePrefixes: raw("hello")}
		requireFieldError(t, page.Validate(), "MessagePage.visiblePrefixes")

		page = MessagePage{MessageID: NewID(), Ordinal: 1, PageText: "text", SourcePageID: "row 3"}
		requireFieldError(t, page.Validate(), "MessagePage.sourcePageID")
	})
}

// TestValidateReportsEveryProblemAtOnce: a client fixing a form should see all
// of it, not one error per round trip.
func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	m := Message{ID: "bad", ProfileID: "", OriginalText: "", TextKind: TextKind("nope")}
	err := m.Validate()
	if err == nil {
		t.Fatal("invalid message accepted")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error %v does not wrap ErrValidation", err)
	}
	text := err.Error()
	for _, field := range []string{"Message.id", "Message.profileID", "Message.originalText", "Message.textKind"} {
		if !strings.Contains(text, field) {
			t.Errorf("error %q does not mention %s", text, field)
		}
	}
}

func requireFieldError(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Validate() = nil, want a complaint about %s", field)
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error %v does not wrap ErrValidation", err)
	}
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("error %q does not name the field %q", err, field)
	}
}

//
// Status machine
//

func TestProfileStatusTransitions(t *testing.T) {
	allowed := [][2]ProfileStatus{
		{StatusDraft, StatusReview},
		{StatusReview, StatusDraft},
		{StatusReview, StatusPublished},
		{StatusPublished, StatusArchived},
	}
	for _, pair := range allowed {
		if !pair[0].CanTransition(pair[1]) {
			t.Errorf("%s -> %s rejected, want allowed", pair[0], pair[1])
		}
	}

	rejected := [][2]ProfileStatus{
		// The immutability gate: nothing goes back from published.
		{StatusPublished, StatusDraft},
		{StatusPublished, StatusReview},
		{StatusPublished, StatusPublished},
		// Publication only happens out of review.
		{StatusDraft, StatusPublished},
		{StatusDraft, StatusArchived},
		{StatusDraft, StatusDraft},
		{StatusReview, StatusReview},
		// Archived is terminal; re-selecting an old revision means deriving one.
		{StatusArchived, StatusDraft},
		{StatusArchived, StatusReview},
		{StatusArchived, StatusPublished},
		{StatusArchived, StatusArchived},
		// Unknown spellings never transition.
		{ProfileStatus("pending"), StatusDraft},
		{StatusDraft, ProfileStatus("pending")},
		{ProfileStatus(""), StatusReview},
	}
	for _, pair := range rejected {
		if pair[0].CanTransition(pair[1]) {
			t.Errorf("%s -> %s allowed, want rejected", pair[0], pair[1])
		}
	}

	// NextStates is derived from the same table, so the UI cannot drift from it.
	if got := StatusDraft.NextStates(); len(got) != 1 || got[0] != StatusReview {
		t.Errorf("draft next = %v, want [review]", got)
	}
	if got := StatusPublished.NextStates(); len(got) != 1 || got[0] != StatusArchived {
		t.Errorf("published next = %v, want [archived]", got)
	}
	if got := StatusArchived.NextStates(); len(got) != 0 {
		t.Errorf("archived next = %v, want none", got)
	}
}

func TestProfilePublishAndImmutability(t *testing.T) {
	draft := Profile{ID: NewID(), EditionID: NewID(), RevisionNum: 1, Status: StatusDraft}
	if draft.CanPublish() {
		t.Error("a draft may not be published directly; docs/spec/03 requires review")
	}
	review := draft
	review.Status = StatusReview
	if !review.CanPublish() {
		t.Error("a revision in review must be publishable")
	}
	published := review
	published.Status = StatusPublished
	published.PublishedAt = nowStamp()
	published.ManifestDigest = strings.Repeat("e", 64)
	if err := published.Validate(); err != nil {
		t.Fatalf("published profile: %v", err)
	}
	if !published.IsImmutable() {
		t.Error("a published profile must report itself immutable")
	}
	if published.Status.CanTransition(StatusDraft) {
		t.Error("published reported a way back to draft")
	}
	archived := published
	archived.Status = StatusArchived
	if !archived.IsImmutable() {
		t.Error("an archived profile must stay immutable: its digest still identifies audio")
	}
}

func TestJobStateMachine(t *testing.T) {
	// docs/spec/02 "Transaction workflows": an interrupted `running` job becomes
	// recoverable, and the reconciler then resumes or fails it.
	if !JobRunning.CanTransition(JobRecoverable) {
		t.Error("running -> recoverable must be allowed")
	}
	if JobRecoverable.CanTransition(JobSucceeded) {
		t.Error("a recoverable job may not declare itself succeeded without running")
	}
	if !JobQueued.Claimable() {
		t.Error("queued jobs must be claimable")
	}
	for _, state := range []JobState{JobRunning, JobSucceeded, JobFailed, JobCancelled, JobRecoverable, JobRequiresInput} {
		if state.Claimable() {
			t.Errorf("%s must not be claimable by a worker", state)
		}
	}
	for _, state := range []JobState{JobSucceeded, JobFailed, JobCancelled} {
		if !state.Terminal() {
			t.Errorf("%s must report terminal", state)
		}
	}
	for _, state := range []JobState{JobQueued, JobRunning, JobRecoverable, JobRequiresInput} {
		if state.Terminal() {
			t.Errorf("%s must not report terminal", state)
		}
	}
	// A retry restarts a failed job rather than exceeding its attempt budget.
	if !JobFailed.CanTransition(JobQueued) {
		t.Error("failed -> queued (an explicit retry) must be allowed")
	}
	if JobSucceeded.CanTransition(JobQueued) {
		t.Error("a succeeded job must not be restartable; that would re-run a side effect")
	}
	if !JobRunning.CanTransition(JobRequiresInput) {
		t.Error("a worker must be able to park a job on a human answer (docs/spec/08 JOB_REQUIRES_INPUT)")
	}
	if JobState("pending").CanTransition(JobRunning) || JobRunning.CanTransition(JobState("pending")) {
		t.Error("an unknown spelling must never transition")
	}
}

func TestAttemptOutcomeRetryPolicy(t *testing.T) {
	// A deadline or a crash may be transient; a deterministic failure retried is
	// wasted provider budget (docs/spec/06 bounds retries deliberately).
	for _, o := range []AttemptOutcome{AttemptTimedOut, AttemptCrashed} {
		if !o.Retryable() {
			t.Errorf("%s should be retryable", o)
		}
	}
	for _, o := range []AttemptOutcome{AttemptFailed, AttemptCancelled, AttemptSucceeded, AttemptRequiresInput} {
		if o.Retryable() {
			t.Errorf("%s should not be retryable", o)
		}
	}
}

//
// Enum plumbing
//

func TestEnumParseRoundTrip(t *testing.T) {
	// Every registered vocabulary must parse its own spellings and reject
	// anything else, including the empty string and case variants.
	for _, name := range EnumNames() {
		enum, ok := LookupEnum(name)
		if !ok {
			t.Fatalf("EnumNames lists %s but LookupEnum refuses it", name)
		}
		if enum.Column == "" || len(enum.Values) == 0 {
			t.Fatalf("enum %s is incompletely registered: %+v", name, enum)
		}
		for _, value := range enum.Values {
			if !strings.Contains(value, "_") && strings.ToLower(value) != value {
				t.Errorf("enum %s value %q is not the wire spelling (snake_case)", name, value)
			}
			if got := strings.Contains(strings.Join(enum.Values, ","), value); !got {
				t.Errorf("enum %s does not list %q", name, value)
			}
		}
		for _, bad := range []string{"", "DRAFT", "draft ", " draft", "draftx", "0"} {
			if _, err := parseEnumValue(name, bad); err == nil {
				t.Errorf("enum %s parsed %q, want an error", name, bad)
			} else if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), enum.Column) {
				t.Errorf("enum %s error for %q = %v, want an ErrValidation naming %q", name, bad, err, enum.Column)
			}
		}
	}
}

// parseEnumValue exercises each exported Parse function by name, so a new enum
// without one fails the test instead of quietly having no parser.
func parseEnumValue(typeName, value string) (any, error) {
	switch typeName {
	case "ProfileStatus":
		return ParseProfileStatus(value)
	case "SourceType":
		return ParseSourceType(value)
	case "RightsStatus":
		return ParseRightsStatus(value)
	case "TextKind":
		return ParseTextKind(value)
	case "AttributionStatus":
		return ParseAttributionStatus(value)
	case "EdgeType":
		return ParseEdgeType(value)
	case "CharacterStatus":
		return ParseCharacterStatus(value)
	case "RegionType":
		return ParseRegionType(value)
	case "DetectMethod":
		return ParseDetectMethod(value)
	case "AssertionMethod":
		return ParseAssertionMethod(value)
	case "CorrectionKind":
		return ParseCorrectionKind(value)
	case "CorrectionTarget":
		return ParseCorrectionTarget(value)
	case "CorrectionStatus":
		return ParseCorrectionStatus(value)
	case "JobState":
		return ParseJobState(value)
	case "AttemptOutcome":
		return ParseAttemptOutcome(value)
	}
	return nil, errors.New("no parser for " + typeName)
}

func TestParseAcceptsEveryRegisteredValue(t *testing.T) {
	for _, name := range EnumNames() {
		enum, _ := LookupEnum(name)
		for _, value := range enum.Values {
			got, err := parseEnumValue(name, value)
			if err != nil {
				t.Errorf("Parse%s(%q): %v", name, value, err)
				continue
			}
			if fmt.Sprint(got) != value {
				t.Errorf("Parse%s(%q) = %v, want %q", name, value, got, value)
			}
			if name == "ProfileStatus" && ProfileStatus(value).String() != value {
				t.Errorf("ProfileStatus(%q).String() = %q", value, ProfileStatus(value).String())
			}
		}
	}
}

//
// JSON payload guard
//

func TestRequireJSONDepthBound(t *testing.T) {
	// A nesting bomb from a scraped page or a model response must be refused
	// before anything recurses over it.
	deep := raw(strings.Repeat(`[`, MaxJSONDepth+2) + strings.Repeat(`]`, MaxJSONDepth+2))
	err := RequireJSON("payload", deep)
	if err == nil {
		t.Fatalf("RequireJSON accepted %d levels of nesting", MaxJSONDepth+2)
	}
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), "levels deep") {
		t.Fatalf("error = %v, want a depth complaint", err)
	}

	// Just inside the bound must pass, or the guard would reject the markup ASTs
	// it exists to protect (docs/spec/02 "Normalization and markup").
	// One level of the count is the array itself.
	ok := raw(strings.Repeat(`[`, MaxJSONDepth-1) + `1` + strings.Repeat(`]`, MaxJSONDepth-1))
	if err := RequireJSON("payload", ok); err != nil {
		t.Errorf("RequireJSON refused a %d-level document: %v", MaxJSONDepth, err)
	}

	for _, bad := range []string{"", "not json", "{", `{"a": }`} {
		if err := RequireJSON("payload", raw(bad)); err == nil {
			t.Errorf("RequireJSON(%q) = nil, want an error", bad)
		}
	}
	// Brackets inside strings must not count as depth: an evidence excerpt from
	// a game script can hold arbitrary text.
	inString := raw(`{"note":"` + strings.Repeat("[", MaxJSONDepth+5) + `"}`)
	if err := RequireJSON("payload", inString); err != nil {
		t.Errorf("RequireJSON counted brackets inside a string: %v", err)
	}
	// Same for an escaped quote, which is where a naive scanner breaks.
	escaped := raw(`{"note":"a \" then ` + strings.Repeat("[", MaxJSONDepth+5) + strings.Repeat("]", MaxJSONDepth+5) + `"}`)
	if err := RequireJSON("payload", escaped); err != nil {
		t.Errorf("RequireJSON rejected a shallow document that only looks deep: %v", err)
	}
}
func TestEnumBehaviouralHelpers(t *testing.T) {
	// External content is untrusted input (docs/spec/14).
	for _, s := range []SourceType{SourceWebPage, SourceRepository} {
		if !s.IsExternal() {
			t.Errorf("%s must count as external", s)
		}
	}
	for _, s := range []SourceType{SourceUserFile, SourceManual, SourceGameExtract} {
		if s.IsExternal() {
			t.Errorf("%s must not count as external", s)
		}
	}
	// Rights gate what may be shipped (docs/spec/02 "Profile portability").
	if RightsUnknown.Publishable() {
		t.Error("rights 'unknown' must never be publishable")
	}
	if !RightsUserSupplied.Publishable() || !RightsLicensed.Publishable() {
		t.Error("user-supplied and licensed must be publishable")
	}
	// Menus are the player's own interface (docs/spec/05 speech classes).
	if TextMenu.DefaultSpeakable() {
		t.Error("menu text should default to not spoken")
	}
	for _, k := range []TextKind{TextDialogue, TextSign, TextItem, TextTutorial, TextSystem, TextOther} {
		if !k.DefaultSpeakable() {
			t.Errorf("%s should default to speakable", k)
		}
	}
	// A region's on-screen role implies the text class its readings carry
	// (docs/spec/04 "Visual profiles"); roles with no single class of their
	// own keep the runner's fallback hint rather than a guessed class.
	wantHints := map[RegionType]TextKind{
		RegionTextbox:      TextDialogue,
		RegionSign:         TextSign,
		RegionItem:         TextItem,
		RegionMenu:         TextMenu,
		RegionTutorial:     TextTutorial,
		RegionSubtitle:     "",
		RegionSpeakerLabel: "",
		RegionHUD:          "",
		RegionOther:        "",
	}
	for rt, want := range wantHints {
		got, ok := rt.TextKindHint()
		if want == "" {
			if ok {
				t.Errorf("%s: TextKindHint = %q, want no hint (the role names no class)", rt, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s: TextKindHint = %q (ok %v), want %q", rt, got, ok, want)
		}
	}

	// An inferred edge that claims certainty contradicts docs/spec/04.
	if EdgeInferred.MaxConfidence() >= 1 {
		t.Errorf("inferred edges may claim %v, want a value below certainty", EdgeInferred.MaxConfidence())
	}
	if EdgeNext.MaxConfidence() != 1 {
		t.Errorf("source-declared next edges should be allowed to claim certainty")
	}
	// Only a curator can produce a manual assertion (docs/spec/08 ACLs).
	if !AssertionManual.FromReviewer() || !AssertionManualCorrection.FromReviewer() {
		t.Error("manual assertion methods must report a reviewer origin")
	}
	for _, m := range []AssertionMethod{AssertionModel, AssertionScriptOrder, AssertionDumpMetadata,
		AssertionOnScreenLabel, AssertionVisualAnchor} {
		if m.FromReviewer() {
			t.Errorf("%s is automated and must not report a reviewer origin", m)
		}
	}
}
