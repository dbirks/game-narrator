import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

// The vitest config keeps globals off, so Testing Library's auto-cleanup
// (which registers against a global afterEach) never runs on its own.
afterEach(() => cleanup());
