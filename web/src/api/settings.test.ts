import { describe, expect, it } from "vitest";

import { scenarioReportUrl, scenarioRunRest } from "./settings";

// The scenario id carries its directory (`synthetic/dialogue-advance-001`),
// which is why the run route's template is `{rest...}` and the encoding is
// per-segment: one whole-id encodeURIComponent would turn the separator into
// %2F and the server would route the request to the wrong wildcard.
describe("scenarioRunRest", () => {
  it("encodes each segment and keeps the separators", () => {
    expect(scenarioRunRest("synthetic/dialogue-advance-001")).toBe(
      "synthetic/dialogue-advance-001",
    );
  });

  it("encodes the unsafe characters inside a segment", () => {
    expect(scenarioRunRest("a b/c%1")).toBe("a%20b/c%251");
  });
});

describe("scenarioReportUrl", () => {
  it("builds the report route's absolute URL", () => {
    expect(scenarioReportUrl("deadbeef")).toBe(
      "http://localhost:3000/api/v1/dev/tests/reports/deadbeef",
    );
  });
});
