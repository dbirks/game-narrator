import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// The Go API target is configurable so Playwright can point the dev server at
// a throwaway port (web/playwright.config.ts). Production embedding of
// web/dist into the Go binary lands in a later milestone (docs/spec/09).
const apiTarget = process.env.GAME_NARRATOR_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    // The OpenAPI contract lives in internal/api/openapi/ next to the Go code
    // that owns it (ADR-0017); the client's contract-parity test imports it.
    fs: { allow: [".."] },
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
