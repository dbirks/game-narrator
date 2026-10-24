package doctor

// The cast voice-gender check (ADR-0171): for every character that has an
// assigned voice, the reference's median F0 (the ADR-0162 YIN estimator)
// must read the character's expected voice class — a female character must
// not carry a male voice and vice versa. Read-only: it reports mismatches;
// the remedy is the `cast audit --fix` command, not a doctor action.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/cast"
	"github.com/dbirks/game-narrator/internal/store"
)

// castVoiceCheckMaxRows bounds the detail string: the doctor table is read
// by a human, so a long mismatch list keeps its first rows and counts the
// rest.
const castVoiceCheckMaxRows = 5

func (r *runner) checkCastVoiceGender(ctx context.Context) Check {
	if strings.TrimSpace(r.opts.DataDir) == "" {
		return Check{Status: StatusNotConfigured, Summary: "no data directory is configured"}
	}
	st, err := store.Open(ctx, store.DBPath(r.opts.DataDir))
	if err != nil {
		return Check{
			Status:  StatusFail,
			Summary: "cannot open the catalogue to audit the cast",
			Detail:  err.Error(),
			Remedy:  "run game-narrator migrate --data-dir " + r.opts.DataDir,
		}
	}
	defer func() { _ = st.Close() }()
	assetStore, err := assets.NewLocal(filepath.Join(r.opts.DataDir, "assets"))
	if err != nil {
		return Check{Status: StatusFail, Summary: "cannot open the asset store to audit the cast", Detail: err.Error()}
	}
	aud := cast.NewAuditor(st, assetStore)

	var (
		profileCount int
		mismatches   []cast.Row
	)
	cursor := ""
	for {
		gamePage, next, err := st.ListGames(ctx, cursor, store.MaxListLimit)
		if err != nil {
			return Check{Status: StatusFail, Summary: "cannot list the catalogue", Detail: err.Error()}
		}
		for _, g := range gamePage {
			editions, err := st.ListEditions(ctx, g.ID)
			if err != nil {
				return Check{Status: StatusFail, Summary: "cannot list editions", Detail: err.Error()}
			}
			for _, e := range editions {
				profiles, err := st.ListProfiles(ctx, e.ID)
				if err != nil {
					return Check{Status: StatusFail, Summary: "cannot list profiles", Detail: err.Error()}
				}
				for _, p := range profiles {
					profileCount++
					rows, err := aud.Profile(ctx, p.ID)
					if err != nil {
						return Check{Status: StatusFail, Summary: "the cast audit failed", Detail: err.Error(),
							Remedy: "run game-narrator doctor --json and the cast audit on the affected profile"}
					}
					mismatches = append(mismatches, cast.Mismatches(rows)...)
				}
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if profileCount == 0 {
		return Check{Status: StatusNotConfigured, Summary: "no profiles imported, so no cast to audit"}
	}
	if len(mismatches) == 0 {
		return Check{
			Status:  StatusOK,
			Summary: fmt.Sprintf("every assigned voice reads its character's expected voice class (%d profile(s))", profileCount),
		}
	}
	var b strings.Builder
	for i, m := range mismatches {
		if i >= castVoiceCheckMaxRows {
			fmt.Fprintf(&b, " and %d more", len(mismatches)-castVoiceCheckMaxRows)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s expects %s but reads %.1f Hz (%s)", m.Character, m.Expected, m.MeasuredF0Hz, m.Measured)
	}
	return Check{
		Status:  StatusDegraded,
		Summary: fmt.Sprintf("%d assigned voice(s) read the wrong voice class", len(mismatches)),
		Detail:  b.String(),
		Remedy:  "run game-narrator cast audit --profile <id> --fix --apply, then review the pending takes",
	}
}
