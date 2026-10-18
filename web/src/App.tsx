import { BrowserRouter, Navigate, Route, Routes } from "react-router";

import { AuthProvider, RequireAuth } from "./auth/auth-context";
import { AppNav } from "./components/AppNav";
import { CharactersPage } from "./pages/Characters";
import { DialoguePage } from "./pages/Dialogue";
import { DiagnosticsPage } from "./pages/Diagnostics";
import { DeveloperPage } from "./pages/developer/DeveloperPage";
import { GameOverviewPage } from "./pages/GameOverview";
import { GraphPage } from "./pages/Graph";
import { LibraryPage } from "./pages/Library";
import { LoginPage } from "./pages/Login";
import { NewGamePage } from "./pages/NewGame";
import { NotFoundPage } from "./pages/NotFound";
import { PlayPage } from "./pages/Play";
import { PublishPage } from "./pages/Publish";
import { RegionsPage } from "./pages/Regions";
import { SetupPage } from "./pages/Setup";
import { VoicesPage } from "./pages/Voices";

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
    <>
      <AppNav />
      <Routes>
        <Route path="/" element={<Navigate to="/library" replace />} />
        <Route path="/setup" element={<SetupPage />} />
        <Route path="/login" element={<LoginPage />} />
        <Route path="/diagnostics" element={<DiagnosticsPage />} />
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
    </>
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
