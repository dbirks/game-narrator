import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { Root } from "./App";
import { LiveStatusProvider } from "./lib/liveStatus";
import { applyTheme, readStoredTheme } from "./lib/theme";
import "./index.css";
import "./styles/tokens.css";
import "./styles/play.css";

// The persisted theme (UX-09): applied before the first paint so a reload
// keeps the choice without a flash. The attribute selects a token set in
// src/styles/tokens.css — never an injected style (ADR-0011).
applyTheme(readStoredTheme() ?? "");

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

const container = document.getElementById("root");
if (!container) {
  throw new Error("missing #root element");
}

createRoot(container).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      {/* The one status subscription per page (plan.md Phase 0 item 5): the
          global status bar's feed (ADR-0100) lives at the app root. */}
      <LiveStatusProvider>
        <Root />
      </LiveStatusProvider>
    </QueryClientProvider>
  </StrictMode>,
);
