package windwaker

// Dialogue graph from FLW1/FLI1.
//
// docs/spec/04 asks the reference adapter to emit "next" edges "if present" in
// the message sections. The requirement this file enforces is the *negative*
// one: edges come only from a structure that is reliably decoded, with the
// evidence recorded on each edge — never from an adjacent-entry heuristic, a
// guess about flow, or an undocumented layout the bmg package keeps as opaque
// raw bytes (ADR-0029: unknown sections are retained verbatim, "never
// interpreted").
//
// The Wind Waker BMG on the user's disc contains no FLW1 or FLI1 section at
// all, so for this game the graph is: no edges, and the coverage note
// records why ("graph: not decoded (FLW1 raw)"), which is the documented,
// non-guessing outcome the import writes to sources.coverage_notes.
//
// GraphNote is the exact note string; the import command appends it verbatim
// so a future revision that decodes FLW1 changes the stored evidence, not a
// scattered string.

import (
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/bmg"
)

// GraphNote is recorded in sources.coverage_notes whenever the adapter emits
// no edges because the flow sections were absent or undecodable.
const GraphNote = "graph: not decoded (FLW1 raw)"

// flowEdges decodes "next" edges from FLW1/FLI1 when that can be done
// reliably. As of this revision it never guesses: it returns nil (and the
// coverage note) when the sections are absent, and — because no FLW1 decoder
// has been structurally validated yet — also when they are present. A future
// revision validates an FLW1 layout against the user's own files before this
// function may return edges; until then every edge would be an invention.
//
// The signature returns the full edge set plus a decoded flag so the caller
// can record evidence accurately: decoded=true would mean every returned
// edge carries edgeType=next and evidence "flw1".
func flowEdges(f *bmg.File) (edges []games.MessageEdge, decoded bool) {
	hasFlow := false
	for i := range f.Sections {
		switch f.Sections[i].Magic {
		case "FLW1", "FLI1":
			hasFlow = true
		}
	}
	if !hasFlow {
		return nil, false
	}
	// FLW1/FLI1 are present but their layout is not decoded in this revision
	// (the bmg package retains them as opaque raw per ADR-0029). Returning
	// edges on an unvalidated layout would violate the "never guess" rule;
	// the honest result is the same as when the sections are absent.
	return nil, false
}
