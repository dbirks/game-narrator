package api

// The /library screen's per-game read (docs/design/plum-peach 2a, ADR-0093):
// one viewer GET that answers the two questions the screen asks of every
// game tile — "which revision would ▶ run?" and "how big is it, and how
// much of it has a voice?".
//
// The answer shape is the design's list columns made into data (cover art is
// the client's placeholder; state chip, lines, cast, voiced and the start
// action all come from this one read):
//
//   - profile is the library's actionable revision: the edition's published
//     revision when any edition has one (the newest publication wins — the
//     revision a session would pin is the one the family last pressed
//     ▶ on), otherwise the edition's newest revision (a draft the onboarding
//     run is still filling), otherwise absent;
//   - lines/characters/voiced are that revision's counts (an indexed COUNT
//     each; the screen never pages the dialogue to learn its length).
//
// A game with nothing catalogued answers 200 with the empty view — the tile
// shows its "no profile yet" chip from that, the same way every other list
// read answers an honest empty rather than a 404 for "exists but bare".

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// gameLibraryDTO is the wire shape of GET /api/v1/games/{id}/library.
type gameLibraryDTO struct {
	GameID string `json:"game_id"`
	// The actionable revision's identity; all absent together when the
	// game has no profile (an edition with no revisions is not an error —
	// it is the shelf's "add a game" next step).
	ProfileID     string `json:"profile_id,omitempty"`
	EditionID     string `json:"edition_id,omitempty"`
	ProfileStatus string `json:"profile_status,omitempty"`
	Revision      int    `json:"revision,omitempty"`
	PublishedAt   string `json:"published_at,omitempty"`
	// The revision's counts (zero when it has no profile).
	Lines      int `json:"lines"`
	Characters int `json:"characters"`
	Voiced     int `json:"voiced"`
}

// handleGameLibrary serves GET /api/v1/games/{id}/library.
func (s *Server) handleGameLibrary(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	g, ok := s.resolveGame(w, r, r.PathValue("id"))
	if !ok {
		return
	}

	view, err := s.gameLibraryView(r.Context(), g)
	if err != nil {
		s.storeProblem(w, r, err, "game library view", "game")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// libraryPick is one edition's candidate revision for the game's actionable
// slot.
type libraryPick struct {
	editionID string
	profile   games.Profile
	published bool
}

// gameLibraryView assembles one game's library view from the catalogue:
// editions in their stable (ULID) order, each edition's published revision
// first and its newest revision second, then the counts of the revision the
// pick landed on.
func (s *Server) gameLibraryView(ctx context.Context, g games.Game) (gameLibraryDTO, error) {
	view := gameLibraryDTO{GameID: g.ID}

	editions, err := s.store.ListEditions(ctx, g.ID)
	if err != nil {
		return view, err
	}

	// The pick: the newest publication (a session pins exactly this), then
	// the newest draft (a setup in progress names its progress on it).
	var chosen *libraryPick
	for _, e := range editions {
		p, _, perr := s.store.PublishedProfile(ctx, e.ID)
		if perr != nil && !errors.Is(perr, store.ErrNotFound) {
			return view, perr // a store fault is not "not published"
		}
		if perr == nil {
			candidate := libraryPick{editionID: e.ID, profile: p, published: true}
			if chosen == nil || newerLibraryPick(chosen, &candidate) {
				chosen = &candidate
			}
			continue
		}
		// Not published: the edition's newest revision stands in.
		profiles, err := s.store.ListProfiles(ctx, e.ID)
		if err != nil {
			return view, err
		}
		if len(profiles) == 0 {
			continue
		}
		newest := profiles[len(profiles)-1]
		candidate := libraryPick{editionID: e.ID, profile: newest, published: false}
		if chosen == nil || (candidate.published && !chosen.published) {
			chosen = &candidate
		}
	}

	if chosen == nil {
		return view, nil // no profile at all: the honest zero view
	}
	view.ProfileID = chosen.profile.ID
	view.EditionID = chosen.editionID
	view.ProfileStatus = string(chosen.profile.Status)
	view.Revision = chosen.profile.RevisionNum
	view.PublishedAt = chosen.profile.PublishedAt

	lines, err := s.store.ProfileMessageCount(ctx, chosen.profile.ID)
	if err != nil {
		return view, err
	}
	characters, voiced, err := s.store.ProfileCharacterVoiceCounts(ctx, chosen.profile.ID)
	if err != nil {
		return view, err
	}
	view.Lines, view.Characters, view.Voiced = lines, characters, voiced
	return view, nil
}

// newerLibraryPick reports whether b outranks a for the actionable slot:
// a publication always beats a draft, the later published_at wins between
// publications, and a ULID edition-id tie-break keeps the answer
// deterministic when two editions publish at the same instant.
func newerLibraryPick(a, b *libraryPick) bool {
	if a.published != b.published {
		return b.published
	}
	if a.profile.PublishedAt != b.profile.PublishedAt {
		at, errA := time.Parse(time.RFC3339, a.profile.PublishedAt)
		bt, errB := time.Parse(time.RFC3339, b.profile.PublishedAt)
		if errA == nil && errB == nil {
			return bt.After(at)
		}
	}
	return b.editionID > a.editionID
}
