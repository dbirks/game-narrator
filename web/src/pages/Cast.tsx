import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";

import { catalogApi, type Game } from "../api/client";
import { useAuth } from "../auth/auth-context";
import { ErrorState, Loading } from "../components/states";
import { DitherCover, NameTab } from "../components/kit";

/**
 * `/games/:id/cast` — the cast page's Phase 1 seam (ADR-0093 §4: Cast merges
 * the characters and voices routes; item 5 of the Phase 1 plan owns the full
 * review surface). What this seam must already carry:
 *
 * - the landing the setup board's completion hands off to (ADR-0093 open
 *   question (a), provisional answer): the prominent **Start narrating** is
 *   the way from "setup finished" to the narration;
 * - the review affordance that exists today — the per-character voice
 *   workflow at `/games/:id/voices` — so a user who lands here can do the
 *   thing the CTA promises, one character at a time, until the merged
 *   surface lands.
 *
 * Placeholder copy only (AGENTS.md rule 5); all quoted text is invented.
 */
export function CastPage() {
  const { id = "" } = useParams<{ id: string }>();
  const { atLeast } = useAuth();
  const canWrite = atLeast("curator");

  const game = useQuery({
    queryKey: ["cast", id, "game"],
    queryFn: ({ signal }) => catalogApi.getGame(id, signal),
    enabled: id !== "",
  });

  if (game.isPending)
    return (
      <main>
        <Loading label="Loading the cast…" />
      </main>
    );
  if (game.isError) {
    return (
      <main>
        <ErrorState
          error={game.error}
          action="Cannot load the game"
          onRetry={() => void game.refetch()}
        />
      </main>
    );
  }
  const g = game.data as Game;

  return (
    <main className="cast-page">
      <header className="page-header cast-header">
        <h1>
          Cast — <span>{g.title}</span>
        </h1>
        <div className="cast-actions">
          <Link to="/library" data-testid="cast-back" className="link-button">
            Library
          </Link>
        </div>
      </header>

      {/* The landing's contract (ADR-0093 (a)): the prominent start. */}
      <div className="cast-cta" data-testid="cast-cta">
        <p className="muted">
          Setup finished: the voices below are reviewed one at a time, and the narration starts from
          here.
        </p>
        <Link to="/play" className="kit-pill kit-pill-coral" data-testid="cast-start-narrating">
          Start narrating
        </Link>
      </div>

      <section className="cast-review" data-testid="cast-review">
        <h2>Review the voices</h2>
        <p className="muted">
          Placeholder: the merged cast review — one approved take per character, regenerate with
          one-step undo — lands on this page. Until then, each character's voice is reviewed on the
          voices page.
        </p>
        {canWrite ? (
          <Link
            to={`/games/${g.id}/voices`}
            className="kit-pill kit-pill-outline"
            data-testid="cast-review-voices"
          >
            Open the voice review
          </Link>
        ) : (
          <p className="muted" data-testid="cast-review-readonly">
            Voice review needs the curator role.
          </p>
        )}
      </section>

      <section className="cast-roster" data-testid="cast-roster">
        <h2>Roster</h2>
        <EmptyStatePlaceholder />
      </section>
    </main>
  );
}

/** The roster's placeholder: the merged surface renders the characters with
 *  their current take; the seam shows the honest stand-in (a dithered cover
 *  per the design's placeholder rule). */
function EmptyStatePlaceholder() {
  return (
    <div className="kit-empty" data-testid="cast-roster-empty">
      <DitherCover label="Roster placeholder" testid="cast-roster-cover" className="devkit-cover" />
      <NameTab>placeholder line for the speaking character</NameTab>
      <p className="muted">The roster lands here with the merged cast surface.</p>
    </div>
  );
}
