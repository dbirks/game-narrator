package store

// The status-bar reads of docs/design/plum-peach "Global chrome": the small
// indexed SELECTs GET /api/v1/status needs to name what the box is doing
// right now. Three rules shape this file, because the caller runs this at up
// to two snapshots a second and the answer sits on the operator's face:
//
//   - Every read is one statement (a join at most), so a status poll costs a
//     prepared statement, not a scan of the content tables.
//   - A missing row is an empty answer, not an error: a run that has no
//     profile yet is pre-profile, and the bar names the plan's title instead
//     of failing the whole snapshot.
//   - Nothing here is a probe. The heavy work (doctor, OCR, provider calls)
//     happens elsewhere and its cached answer is what the status reads; these
//     methods count rows.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// activeAgentStates are the run states that mean "the setup run is happening
// now" (docs/spec/03's closed set minus the terminal states): a queued run
// is work the box will do, a blocked one is work waiting on a human, a
// review one is work waiting on an answer — all of them live.
var activeAgentStates = []string{AgentStateQueued, AgentStateRunning, AgentStateBlocked, AgentStateReview}

// activeAgentRunCap bounds the list: the bar needs the current work, and a
// box with more than eight live setup runs has a queue problem the bar is
// not the place to debug. The bound keeps the statement's answer shape from
// growing with the store.
const activeAgentRunCap = 8

// ListActiveAgentRuns returns the runs that are not over, newest first, each
// with the same fields GetAgentRun loads (plan included: a pre-profile run's
// plan is the only place its game title lives). A table the migration has
// not applied is an empty list, like every other read against an
// un-migrated shape: the snapshot degrades to "no setup run", which is what
// is true of a box without the agent log.
func (a *AgentStore) ListActiveAgentRuns(ctx context.Context) ([]AgentRun, error) {
	const op = "list active agent runs"
	in := strings.Repeat("?,", len(activeAgentStates)-1) + "?"
	args := make([]any, len(activeAgentStates)+1)
	for i, st := range activeAgentStates {
		args[i] = st
	}
	args[len(activeAgentStates)] = activeAgentRunCap

	var out []AgentRun
	err := a.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, op,
			`SELECT id, profile_id, state, plan_json, budget_json, prompt_version,
				model, stop_reason, created_at, updated_at
			 FROM agent_runs WHERE state IN (`+in+`)
			 ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
		if err != nil {
			return mapReadError(op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				r       AgentRun
				profile sql.NullString
				created string
				updated string
			)
			if err := rows.Scan(&r.ID, &profile, &r.State, &r.PlanJSON, &r.BudgetJSON,
				&r.PromptVersion, &r.Model, &r.StopReason, &created, &updated); err != nil {
				return mapReadError(op, err)
			}
			if profile.Valid {
				r.ProfileID = profile.String
			}
			r.CreatedAt, err = parseStamp(created)
			if err != nil {
				return mapReadError(op, err)
			}
			r.UpdatedAt, err = parseStamp(updated)
			if err != nil {
				return mapReadError(op, err)
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// GameTitleForProfile walks profile → edition → game and returns the game's
// title: the name the status bar wants beside the session and the setup run
// ("Setting up The Wind Waker", "● live · …"). A profile with no edition
// row, or an edition whose game was deleted, answers "" — the bar then
// falls back to the run's plan title or the bare id, and a nameless game is
// still a game the box is working on.
func (s *Store) GameTitleForProfile(ctx context.Context, profileID string) (string, error) {
	const op = "game title for profile"
	var title string
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, op,
			`SELECT g.title
			 FROM profiles p
			 JOIN editions e ON e.id = p.edition_id
			 JOIN games g ON g.id = e.game_id
			 WHERE p.id = ?`, profileID).Scan(&title)
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", mapReadError(op, err)
	}
	return title, nil
}

// ProfileCharacterVoiceCounts is the shared "9 of 12 voiced" pair behind the
// Library read, the status bar's setup-run progress and the Cast page header
// (ADR 0134): one definition everywhere, so the tile chip and the bar cannot
// disagree about how much of a game has a voice.
//
// The denominator is the profile's **active speaking characters**: rows whose
// status is not retired (a retired row never counts, with or without lines)
// AND that hold at least one assigned line (a roster slot nobody speaks is
// not a speaker). The numerator counts the subset of those that carries at
// least one voice revision in a state other than retired — a draft or
// auditioning take still counts, the line has a voice whether the curator
// approved it or the auto-cast default did; retired is the only state that
// means "no voice". The voice side joins on the character's **name**:
// voice_profiles.character is the voice surface's identity (UNIQUE
// (character, revision), migration 0009), not a characters.id — joining the
// name column to the id column matched nothing and read every profile's
// voiced count as 0. Two COUNT statements on indexed columns (the EXISTS
// rides messages_profile_speaker_idx, the voice side
// idx_voice_profiles_character), one read transaction.
func (s *Store) ProfileCharacterVoiceCounts(ctx context.Context, profileID string) (characters, voiced int, err error) {
	const op = "profile character voice counts"
	speaking := "EXISTS (SELECT 1 FROM messages m WHERE m.profile_id = c.profile_id AND m.speaker_id = c.id)"
	err = s.inRead(ctx, func(ctx context.Context, q queryable) error {
		if err := q.queryRow(ctx, op,
			`SELECT COUNT(*) FROM characters c
			 WHERE c.profile_id = ? AND c.status <> 'retired' AND `+speaking, profileID,
		).Scan(&characters); err != nil {
			return mapReadError(op, err)
		}
		return q.queryRow(ctx, op,
			`SELECT COUNT(DISTINCT c.id) FROM characters c
			 JOIN voice_profiles vp ON (vp.character = c.name OR vp.character = c.id)
			 WHERE c.profile_id = ? AND c.status <> 'retired' AND vp.state <> 'retired' AND `+speaking, profileID,
		).Scan(&voiced)
	})
	if err != nil {
		return 0, 0, err
	}
	return characters, voiced, nil
}

// AgentPlanTitle pulls the plan's title out of a run's plan_json, for the
// pre-profile half of a setup run's name. A plan is JSON the agent loop
// wrote (validateJSONOrEmpty's guard means it is JSON), and a missing or
// unparseable title is "" rather than an error: the name is display data.
func AgentPlanTitle(planJSON []byte) string {
	var p struct {
		Title string `json:"title"`
	}
	if len(planJSON) == 0 {
		return ""
	}
	if err := json.Unmarshal(planJSON, &p); err != nil {
		return ""
	}
	return strings.TrimSpace(p.Title)
}
