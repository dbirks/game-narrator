import { BrowserRouter, Navigate, Route, Routes, useParams } from "react-router";

import { AuthProvider, RequireAuth } from "./auth/auth-context";
import { Shell } from "./components/Shell";
import { CastPage } from "./pages/Cast";
import { CharactersPage } from "./pages/Characters";
import { DialoguePage } from "./pages/Dialogue";
import { DiagnosticsPage } from "./pages/Diagnostics";
import { DeveloperPage } from "./pages/developer/DeveloperPage";
import { DevKitPage } from "./pages/DevKit";
import { GameOverviewPage } from "./pages/GameOverview";
import { GraphPage } from "./pages/Graph";
import { LibraryPage } from "./pages/Library";
import { LoginPage } from "./pages/Login";
import { NewGamePage } from "./pages/NewGame";
import { NotFoundPage } from "./pages/NotFound";
import { PlayPage } from "./pages/Play";
import { PublishPage } from "./pages/Publish";
import { ReplayClipsPage } from "./pages/ReplayClips";
import { RegionsPage } from "./pages/Regions";
import { SetupPage } from "./pages/Setup";
import { SettingsPage } from "./pages/Settings";
import { TestsPage } from "./pages/Tests";
import { VoicesPage } from "./pages/Voices";

/**
 * The ⌘K character-row seam (ADR-0098): /games/:id/cast is the design's Cast
 * destination; the merged Cast page (plan item 5) replaces this redirect with
 * the real route, and the old /characters and /voices paths become its
 * redirects — the seam moves, the link never 404s.
 */
function CastSeamRedirect() {
  const { id } = useParams();
  return <Navigate to={`/games/${id}/characters`} replace />;
}

/**
 * Routes (docs/spec/09 "Routes and page requirements"). The milestone 1 slice
 * built the first-run, sign-in and catalogue pages; the milestones since added
 * the session runtime, developer inspector, voice, onboarding, character and
 * publication-review pages. The nav shows only the global destinations;
 * per-game pages (overview, dialogue, characters, voices, publish) are reached
 * from the revision rows of the overview, which is how their `?profile=` target
 * gets named.
 *
 * `/diagnostics` stays outside the guard on purpose: readiness and build info are
 * public routes in the scope table (ADR-0016 §3) because `doctor` and a stalled
 * appliance must be inspectable before an account exists — and an appliance that
 * cannot be inspected is the one you are locked out of.
 */
export function App() {
  return (
    <Shell>
      <Routes>
        <Route path="/" element={<Navigate to="/library" replace />} />
        {/* Plum & Peach IA seams (ADR-0093 §4): the rename routes. The
            redirects keep the design's status-bar nav ("traces") live in
            Phase 0; Phase 1 moves the pages and keeps these redirects. */}
        <Route path="/traces" element={<Navigate to="/developer" replace />} />
        <Route path="/health" element={<Navigate to="/diagnostics" replace />} />
        {/* The ⌘K palette's character rows target the Cast page (ADR-0098's
            IA: /games/:id/cast). The merged Cast page lands with plan item 5;
            until then the seam keeps every cast link live, the same way
            /traces and /health keep the renamed nav live in Phase 0. */}
        <Route path="/games/:id/cast" element={<CastSeamRedirect />} />
        {/* The kit gallery (plan.md Phase 0 item 4): developer role only;
            the page gates the role, the guard gates the session. */}
        <Route
          path="/dev/kit"
          element={
            <RequireAuth>
              <DevKitPage />
            </RequireAuth>
          }
        />
        <Route path="/setup" element={<SetupPage />} />
        <Route path="/login" element={<LoginPage />} />
        <Route path="/diagnostics" element={<DiagnosticsPage />} />
        {/* /settings is admin-gated by the API (403 otherwise); the guard here
            is the unauthenticated case only (ADR-0047). */}
        <Route
          path="/settings"
          element={
            <RequireAuth>
              <SettingsPage />
            </RequireAuth>
          }
        />
        <Route
          path="/tests"
          element={
            <RequireAuth>
              <TestsPage />
            </RequireAuth>
          }
        />
        {/* /replay-clips is developer-scoped by the API (403 otherwise); the
            guard here is the unauthenticated case (ADR-0073). */}
        <Route
          path="/replay-clips"
          element={
            <RequireAuth>
              <ReplayClipsPage />
            </RequireAuth>
          }
        />
        <Route
          path="/developer"
          element={
            <RequireAuth>
              <DeveloperPage />
            </RequireAuth>
          }
        />
        <Route
          path="/library"
          element={
            <RequireAuth>
              <LibraryPage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/new"
          element={
            <RequireAuth>
              <NewGamePage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/:id/overview"
          element={
            <RequireAuth>
              <GameOverviewPage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/:id/dialogue"
          element={
            <RequireAuth>
              <DialoguePage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/:id/cast"
          element={
            <RequireAuth>
              <CastPage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/:id/voices"
          element={
            <RequireAuth>
              <VoicesPage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/:id/characters"
          element={
            <RequireAuth>
              <CharactersPage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/:id/graph"
          element={
            <RequireAuth>
              <GraphPage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/:id/regions"
          element={
            <RequireAuth>
              <RegionsPage />
            </RequireAuth>
          }
        />
        <Route
          path="/games/:id/publish"
          element={
            <RequireAuth>
              <PublishPage />
            </RequireAuth>
          }
        />
        <Route
          path="/play"
          element={
            <RequireAuth>
              <PlayPage />
            </RequireAuth>
          }
        />
        <Route path="*" element={<NotFoundPage />} />
      </Routes>
    </Shell>
  );
}

export function Root() {
  return (
    <BrowserRouter>
      <AuthProvider>
        <App />
      </AuthProvider>
    </BrowserRouter>
  );
}
