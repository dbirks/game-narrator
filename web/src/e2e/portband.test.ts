import { existsSync } from "node:fs";
import { createServer, type Server } from "node:net";
import { afterAll, describe, expect, it } from "vitest";

import {
  bandFree,
  bandLockDir,
  candidateBases,
  releaseBand,
  resolvePortBase,
  runPorts,
  type PortBaseResolution,
} from "../../e2e/support/portband";

/**
 * The port-band selection behind GN_E2E_PORT_BASE (ADR-0094, issue #43):
 * several Playwright runs in the same checkout resolve their base at config
 * load, and a run must never take a band another run already claimed — with
 * a fixed default base two concurrent runs bound the same ports, and the
 * second run's webServer health check was answered by the first run's
 * appliance (its tests then asserted against a claimed foreign appliance).
 *
 * Each "run" here is a resolvePortBase call with the environment value the
 * run would have at config load (unset, for the default). The claim is a
 * real mkdir lock under os.tmpdir() keyed by the band, so two resolutions in
 * this process exercise the same arbitration two processes would: the probe
 * says "nobody answers", the lock breaks the tie.
 */
const claimed: string[] = [];
function remember(run: PortBaseResolution): void {
  if (run.lock !== null) claimed.push(run.lock);
}

afterAll(() => {
  // Leave no claims behind: the next real Playwright run must see every band
  // as unclaimed (a leftover claim is only reclaimable after this process
  // exits, and the next run may start before that).
  for (const lock of claimed) releaseBand(lock);
});

describe("resolvePortBase (issue #43)", () => {
  it("two runs with GN_E2E_PORT_BASE unset reserve disjoint port bands", async () => {
    const run1 = await resolvePortBase(undefined);
    const run2 = await resolvePortBase(undefined);
    remember(run1);
    remember(run2);
    expect(
      run1.base,
      "two concurrent runs must not resolve to the same base — the second " +
        "run's servers would fail to bind (or be killed by Playwright) and its " +
        "health check would be answered by the first run's appliance",
    ).not.toBe(run2.base);
    expect(run1.lock, "the unset path must claim its band").not.toBeNull();
    expect(run2.lock, "the unset path must claim its band").not.toBeNull();
    // Every port the run would bind is free on both bases.
    await expect(bandFree(run1.base)).resolves.toBe(true);
    await expect(bandFree(run2.base)).resolves.toBe(true);
    // No shared port between the two runs. The dev-server port offset is
    // negative (5173 - 18090), so base arithmetic does not prove
    // disjointness — compare the real port sets.
    const run1Ports = new Set(runPorts(run1.base));
    for (const port of runPorts(run2.base)) {
      expect(run1Ports.has(port), `port ${port} would be shared between the runs`).toBe(false);
    }
    // The claim is on disk: a foreign process cannot take either band.
    expect(existsSync(bandLockDir(run1.base))).toBe(true);
    expect(existsSync(bandLockDir(run2.base))).toBe(true);
  });

  it("a third run skips every band the first two runs claimed", async () => {
    const run1 = await resolvePortBase(undefined);
    const run2 = await resolvePortBase(undefined);
    const run3 = await resolvePortBase(undefined);
    remember(run1);
    remember(run2);
    remember(run3);
    expect(new Set([run1.base, run2.base, run3.base]).size).toBe(3);
  });

  it("a released claim becomes claimable again", async () => {
    const first = await resolvePortBase(undefined);
    remember(first);
    expect(existsSync(bandLockDir(first.base))).toBe(true);
    releaseBand(first.lock as string);
    expect(existsSync(bandLockDir(first.base))).toBe(false);
    // The freed band is probe-free and unclaimed again, so the next run
    // re-claims it (candidate order makes this deterministic).
    const second = await resolvePortBase(undefined);
    remember(second);
    expect(second.base).toBe(first.base);
  });

  it("an explicit base is used as-is: operator choice, never probed or claimed", async () => {
    const run = await resolvePortBase("19000");
    expect(run.base).toBe(19000);
    expect(run.lock).toBeNull();
    expect(existsSync(bandLockDir(19000))).toBe(false);
  });

  it("invalid bases fail loudly instead of binding", async () => {
    await expect(resolvePortBase("900")).rejects.toThrow(/not a valid port/);
    await expect(resolvePortBase("not-a-port")).rejects.toThrow(/not a valid port/);
  });

  it("when every candidate band is taken the run fails loudly instead of picking a colliding base", async () => {
    // Hold one port per candidate band: each bandFree probe then fails and
    // the resolution must give up with the operator-facing diagnostic.
    const holders: Server[] = [];
    for (const base of candidateBases()) {
      const server = createServer(() => {});
      const listening = await new Promise<boolean>((resolve) => {
        server.once("error", () => resolve(false));
        server.listen(base, "127.0.0.1", () => resolve(true));
      });
      // An already-held port needs no holder of ours: the probe fails on it
      // either way, which is the point of the test.
      if (listening) holders.push(server);
    }
    try {
      await expect(resolvePortBase(undefined)).rejects.toThrow(/none of the candidate bands/);
    } finally {
      await Promise.all(
        holders.map(
          (server) =>
            new Promise<void>((resolve) => {
              server.close(() => resolve());
            }),
        ),
      );
    }
  });
});
