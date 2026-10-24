package matcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// FlagEditionKey is the Message.Flags key an importer may set to record which
// edition a line came from, so a multi-edition profile can score the edition
// component per message instead of per index. It is a convention, not a schema
// claim: nothing in internal/store or internal/games defines it, and an absent
// key means "unspecified", which scores as agreement rather than as evidence.
const FlagEditionKey = "edition"

// Index is an immutable retrieval and graph structure for one profile revision.
// Build produces it, Rank reads it, and nothing else mutates it, so concurrent
// Rank calls on one Index are safe without locking — which matters because the
// capture loop, the developer UI and a replay harness all ask about the same
// published profile at once (docs/spec/05 "immutable scene observations").
//
// The layout is deliberately flat: parallel int32/string slices addressed by a
// message's ordinal, plus posting lists of int32 ordinals. A 10k-message profile
// is a few hundred kilobytes of keys and a few megabytes of postings, not a
// graph of small structs (docs/spec/16 M1 "search 10k synthetic messages without
// memory blowup" on a 4 GB device).
type Index struct {
	opts     IndexOptions
	fold     *textnorm.Folder
	ids      []string // ordinal → message ID
	ords     map[string]int32
	keys     []string // ordinal → folded retrieval key
	keyLens  []int32  // ordinal → rune length of key
	scenes   []string
	kinds    []string
	editions []string // ordinal → edition label, "" when unspecified

	// exact maps a folded key to every message sharing it, ordinals ascending.
	// Ascending order is what makes the ambiguity count and the tie-break free.
	exact map[string][]int32
	// sortedKeys holds the distinct folded keys in byte order for prefix lookup.
	sortedKeys []string

	// Trigram inverted index. triCount is each message's distinct-trigram count
	// (the denominator of the Dice coefficient); postings maps a hashed trigram
	// to the ordinals containing it, ascending by construction.
	triCount []int32
	postings map[uint32][]int32

	// Token sets, in the same hashed form: message o's sorted unique token keys are
	// tokKeys[tokOff[o]:tokOff[o+1]]. Storing them at build time rather than
	// re-splitting stored text per candidate is what makes the token component
	// allocation-free on the query path, and it is a fraction of the trigram index's
	// size because there are ~16 words per line and ~60 trigrams.
	tokOff  []int32
	tokKeys []uint32

	// Outgoing adjacency in flat arrays: message o's edges are
	// outTo[outOff[o]:outOff[o+1]]. A map of slices per node would dominate the
	// heap for a graph that is mostly one successor per message.
	outOff  []int32
	outTo   []int32
	outKind []uint8
	outConf []float32

	// Display pages (ADR-0184): message o's pages are the flat rows
	// pageRow[pageRowOff[o]:pageRowOff[o+1]] — present only for messages the
	// caller indexed with two or more pages, which is where whole-text scoring
	// under-scores a single-box read. Each row holds the folded retrieval key
	// (built once, at index build — not per frame) and its own trigram count and
	// token set, in the same flat form as the whole-text columns, so a page
	// scores with the same character/token/Levenshtein blend a one-box message
	// would get.
	pageRowOff   []int32
	pageKeys     []string
	pageTriCount []int32
	pageTokOff   []int32
	pageTokKeys  []uint32

	stats Stats
}

// Stats reports index size and shape, so a profile publication can log it and a
// scale test can assert it (docs/spec/16 M2 "index build memory").
type Stats struct {
	Messages    int   // messages actually indexed
	Edges       int   // edges with both endpoints indexed
	Keys        int   // distinct retrieval keys
	Tokens      int   // token-set entries across all messages
	Trigrams    int   // distinct trigrams in the inverted index
	Postings    int   // total posting-list entries
	ApproxBytes int64 // approximation of live heap bytes held by the index

	// SkippedMessages and SkippedEdges count input rows Build refused to index.
	// They are reported rather than silently dropped so a partial import shows up
	// as a number in a trace instead of as an unexplained mismatch.
	SkippedMessages int
	SkippedEdges    int
}

// Errors returned by Build, declared as sentinel values so a caller can branch on
// errors.Is for "the profile is corrupt" instead of matching prose; the message an
// individual row produced wraps the sentinel with the offending ID or index, which
// is what makes a partial import actionable (AGENTS.md "reproduce, then repair").
var (
	// ErrNoMessages means Build was given nothing to index. An empty index would
	// make every observation abstain, which is indistinguishable from a broken
	// profile, so it is rejected at build time instead.
	ErrNoMessages = errors.New("matcher: no messages to index")
	// ErrDuplicateID means two messages claim the same ID, so a committed
	// message could not be resolved back to one row.
	ErrDuplicateID = errors.New("matcher: duplicate message ID")
	// ErrEmptyID means a message has no ID at all.
	ErrEmptyID = errors.New("matcher: message with empty ID")
)

// Build indexes msgs and edges. It performs no OCR, no I/O and no model call, and
// returns an *Index that is safe to share. The input slices are not retained: keys
// are freshly folded, so a caller may reuse or mutate its buffers afterwards.
func Build(msgs []games.Message, edges []games.MessageEdge, opts IndexOptions) (*Index, error) {
	if len(msgs) == 0 {
		return nil, ErrNoMessages
	}
	idx := &Index{
		opts:     opts,
		fold:     textnorm.NewFolder(opts.foldOptions()),
		ords:     make(map[string]int32, len(msgs)),
		exact:    make(map[string][]int32, len(msgs)),
		postings: map[uint32][]int32{},
	}
	idx.ids = make([]string, 0, len(msgs))
	idx.keys = make([]string, 0, len(msgs))
	idx.keyLens = make([]int32, 0, len(msgs))
	idx.scenes = make([]string, 0, len(msgs))
	idx.kinds = make([]string, 0, len(msgs))
	idx.editions = make([]string, 0, len(msgs))
	idx.triCount = make([]int32, 0, len(msgs))

	var scratch, tokScratch, pageScratch []uint32
	for i := range msgs {
		m := &msgs[i]
		if m.ID == "" {
			return nil, fmt.Errorf("%w at index %d", ErrEmptyID, i)
		}
		if _, dup := idx.ords[m.ID]; dup {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateID, m.ID)
		}
		text := m.NormalizedText
		if text == "" {
			text = m.OriginalText
		}
		key := idx.fold.Normalize(truncateBounds(text, maxTextRunes))
		if key == "" {
			idx.stats.SkippedMessages++
			continue
		}
		idx.pageRowOff = append(idx.pageRowOff, int32(len(idx.pageKeys)))
		ord := int32(len(idx.ids))
		idx.ords[m.ID] = ord
		idx.ids = append(idx.ids, m.ID)
		idx.keys = append(idx.keys, key)
		idx.keyLens = append(idx.keyLens, int32(utf8.RuneCountInString(key)))
		idx.scenes = append(idx.scenes, m.SceneID)
		idx.kinds = append(idx.kinds, string(m.TextKind))
		idx.editions = append(idx.editions, messageEdition(m, opts.Edition))

		idx.exact[key] = append(idx.exact[key], ord)
		scratch = messageTrigrams(key, scratch)
		for _, h := range scratch {
			idx.postings[h] = append(idx.postings[h], ord)
		}
		idx.triCount = append(idx.triCount, int32(len(scratch)))

		tokScratch = tokenHashes(key, tokScratch)
		idx.tokOff = append(idx.tokOff, int32(len(idx.tokKeys)))
		idx.tokKeys = append(idx.tokKeys, tokScratch...)
		idx.stats.Tokens += len(tokScratch)

		// Display pages (ADR-0184): fold each page's retrieval key once here, at
		// build time — the per-frame path only scores against the stored keys.
		// Single-page and page-less messages keep no rows: their whole key is the
		// only thing to score, exactly as before.
		if pages := opts.Pages[m.ID]; len(pages) > 1 {
			for _, p := range pages {
				pkey := idx.fold.Normalize(truncateBounds(p, maxTextRunes))
				if pkey == "" {
					continue
				}
				pTri := messageTrigrams(pkey, pageScratch)
				pageScratch = pTri[:0]
				pTok := tokenHashes(pkey, pageScratch)
				pageScratch = pTok[:0]
				idx.pageTriCount = append(idx.pageTriCount, int32(len(pTri)))
				idx.pageTokOff = append(idx.pageTokOff, int32(len(idx.pageTokKeys)))
				idx.pageTokKeys = append(idx.pageTokKeys, pTok...)
				idx.pageKeys = append(idx.pageKeys, pkey)
			}
		}
	}
	idx.pageRowOff = append(idx.pageRowOff, int32(len(idx.pageKeys)))    // end sentinel
	idx.pageTokOff = append(idx.pageTokOff, int32(len(idx.pageTokKeys))) // end sentinel
	if len(idx.ids) == 0 {
		return nil, ErrNoMessages
	}

	idx.sortedKeys = make([]string, 0, len(idx.exact))
	for k := range idx.exact {
		idx.sortedKeys = append(idx.sortedKeys, k)
	}
	sort.Strings(idx.sortedKeys)

	idx.tokOff = append(idx.tokOff, int32(len(idx.tokKeys))) // end sentinel for the last message
	idx.stats.Messages = len(idx.ids)
	idx.stats.Keys = len(idx.sortedKeys)
	idx.stats.Trigrams = len(idx.postings)
	for _, list := range idx.postings {
		idx.stats.Postings += len(list)
	}

	idx.buildEdges(edges)
	idx.stats.ApproxBytes = idx.approxBytes()
	return idx, nil
}

// buildEdges lays the adjacency lists out flat. Edges whose endpoints are not in
// the index are counted and ignored: a profile whose message import is partial
// still ranks, it just has fewer graph hints, and failing the whole build over one
// dangling transition would make the matcher the strictest thing in the pipeline.
func (x *Index) buildEdges(edges []games.MessageEdge) {
	type edge struct {
		from int32
		to   int32
		kind uint8
		conf float32
	}
	resolved := make([]edge, 0, len(edges))
	for _, e := range edges {
		from, okFrom := x.ords[e.FromID]
		to, okTo := x.ords[e.ToID]
		if !okFrom || !okTo {
			x.stats.SkippedEdges++
			continue
		}
		// An unknown edge type keeps its place in the graph but scores no prior
		// (baseEdgePrior returns 0): absence of knowledge is not evidence, and
		// guessing a provenance level for it would be worse than having none.
		resolved = append(resolved, edge{
			from: from, to: to,
			kind: uint8(edgeKindIndex(games.EdgeType(e.EdgeType))),
			conf: float32(unit(e.Confidence)),
		})
	}
	// Ordinal order, stable, so a message with two edges keeps the order the
	// profile stored them in and traversal never depends on map iteration.
	sort.SliceStable(resolved, func(i, j int) bool { return resolved[i].from < resolved[j].from })

	x.outOff = make([]int32, len(x.ids)+1)
	x.outTo = make([]int32, len(resolved))
	x.outKind = make([]uint8, len(resolved))
	x.outConf = make([]float32, len(resolved))
	for i, e := range resolved {
		x.outTo[i] = e.to
		x.outKind[i] = e.kind
		x.outConf[i] = e.conf
		x.outOff[int(e.from)+1]++
		x.stats.Edges++
	}
	for o := range x.ids {
		x.outOff[o+1] += x.outOff[o]
	}
}

// neighbors visits message o's outgoing edges in a deterministic order (input
// order within a message, which sort.SliceStable above preserved).
func (x *Index) neighbors(o int32, visit func(to int32, kind games.EdgeType, conf float64)) {
	if len(x.outOff) == 0 || int(o) >= len(x.outOff)-1 {
		return
	}
	for p := x.outOff[o]; p < x.outOff[o+1]; p++ {
		visit(x.outTo[p], edgeKindAt(x.outKind[p]), float64(x.outConf[p]))
	}
}

// messageEdition reads a per-message edition label from Flags, falling back to the
// index-level label. The JSON is parsed once at build time, never per query.
func messageEdition(m *games.Message, indexEdition string) string {
	edition := indexEdition
	if len(m.Flags) > 2 {
		var flags map[string]any
		if err := json.Unmarshal(m.Flags, &flags); err == nil {
			if s, ok := flags[FlagEditionKey].(string); ok && s != "" {
				edition = s
			}
		}
	}
	return edition
}

// approxBytes adds up the sizes the structures actually hold. It is an
// approximation on purpose: Go's allocator rounds, and a map's bucket overhead is
// not public. It is precise enough to catch a structural mistake (a posting list
// storing strings instead of int32s would show up immediately) and to satisfy the
// memory gate in docs/spec/16 M2.
func (x *Index) approxBytes() int64 {
	var b int64
	b += int64(len(x.ids)) * 16
	for _, s := range x.keys {
		b += int64(len(s)) + 16
	}
	for _, s := range x.ids {
		b += int64(len(s))
	}
	for _, s := range x.scenes {
		b += int64(len(s)) + 16
	}
	for _, s := range x.kinds {
		b += int64(len(s)) + 16
	}
	for _, s := range x.editions {
		b += int64(len(s)) + 16
	}
	b += int64(len(x.keyLens)) * 4
	b += int64(len(x.triCount)) * 4
	b += int64(len(x.tokOff))*4 + int64(cap(x.tokKeys))*4
	b += int64(len(x.outTo))*4 + int64(len(x.outKind)) + int64(len(x.outConf))*4 +
		int64(len(x.outOff))*4
	// Map entries: key plus slice header.
	b += int64(len(x.exact)) * (48 + 24)
	for k, v := range x.exact {
		b += int64(len(k)) + int64(cap(v))*4
	}
	b += int64(len(x.postings)) * (24 + 24)
	b += int64(x.stats.Postings) * 4
	for _, s := range x.sortedKeys {
		b += int64(len(s)) + 16
	}
	return b
}

// Stats reports index size. The returned value is a copy of immutable data.
func (x *Index) Stats() Stats { return x.stats }

// tokensOf returns message o's sorted unique token keys.
// pageTokensOf returns the stored token set of page row pi (the flat index of
// a row in pageKeys, not a message ordinal): one box's retrieval tokens, kept
// at build time for the same allocation-free reason tokensOf keeps the
// whole-text tokens.
func (x *Index) pageTokensOf(pi int) []uint32 {
	return x.pageTokKeys[x.pageTokOff[pi]:x.pageTokOff[pi+1]]
}

func (x *Index) tokensOf(o int32) []uint32 {
	lo, hi := x.tokOff[o], x.tokOff[o+1]
	return x.tokKeys[lo:hi]
}

// Options reports the options the index was built with.
func (x *Index) Options() IndexOptions { return x.opts }

// Fold normalizes text the way this index stores its keys. It is exported because
// a caller that wants to compare a corrected transcript against an observation —
// the developer UI's "why did it not match" view — must use this index's fold and
// not hand-roll a second one (textnorm's package doc makes the same point about
// half-folded comparisons).
func (x *Index) Fold(s string) string { return x.fold.Normalize(truncateBounds(s, maxTextRunes)) }

// Lookup returns the IDs whose folded key equals text exactly, in ordinal order.
func (x *Index) Lookup(text string) []string {
	ords := x.exact[x.fold.Normalize(truncateBounds(text, maxTextRunes))]
	out := make([]string, 0, len(ords))
	for _, o := range ords {
		out = append(out, x.ids[o])
	}
	return out
}

// MessageCount reports indexed messages.
func (x *Index) MessageCount() int { return len(x.ids) }

// KindFor reports the stored text class (dialogue, sign, item, …) of one
// indexed message, so a caller — the session's commitment policy — can decide
// what the operator elected to hear without re-reading the profile. A message
// the index never saw (never indexed, an empty id, the session's "unknown"
// sentinel) reports the zero kind, which the caller reads as "no class
// known": the class filter must never veto a line it cannot name.
func (x *Index) KindFor(messageID string) (k games.TextKind, ok bool) {
	o, found := x.ords[messageID]
	if !found {
		return "", false
	}
	return games.TextKind(x.kinds[o]), true
}

// edgeKindIndex maps an edge type onto a small dense code so the adjacency array
// stores one byte per edge instead of a string.
func edgeKindIndex(t games.EdgeType) int {
	for i, known := range edgeTypes {
		if known == t {
			return i
		}
	}
	return len(edgeTypes) // unknown
}

func edgeKindAt(code uint8) games.EdgeType {
	if int(code) >= len(edgeTypes) {
		return games.EdgeType("")
	}
	return edgeTypes[code]
}

// edgeTypes is ordered strongest-first, which is what makes the byte code
// double as a rank. docs/spec/04 "Dialogue graph" establishes the order:
// explicit next metadata is high provenance, branch is a real alternative, a
// repeat is the same line becoming current again, contextual depends on a
// condition the runtime did not evaluate, and inferred is a guess that "must never
// outrank contrary OCR text".
var edgeTypes = []games.EdgeType{
	games.EdgeNext,
	games.EdgeBranch,
	games.EdgeRepeat,
	games.EdgeContextual,
	games.EdgeInferred,
}

// edgePriors is the base prior per edge kind at the matching index above. Unknown
// kinds score 0: absence of knowledge is not evidence.
var edgePriors = []float64{1.0, 0.7, 0.5, 0.35, 0.2}

func baseEdgePrior(t games.EdgeType) float64 {
	i := edgeKindIndex(t)
	if i >= len(edgePriors) {
		return 0
	}
	return edgePriors[i]
}

// Successor is one outgoing graph edge from a message, weighted exactly the
// way the ranker's graph expansion (expandGraph) weights it at recency
// position 0: baseEdgePrior(kind) * confidence. That is the predictive
// tracker's own "likely next" arithmetic, so prewarming (docs/spec/07
// "Prewarm top weighted successor messages") and the tracker agree on which
// lines deserve to be prepared.
type Successor struct {
	// MessageID is the successor message.
	MessageID string
	// Weight is in (0,1]: the edge prior the ranker applies to the edge.
	Weight float64
}

// Successors reports messageID's successors, best weighted first (weight
// descending, message id ascending on ties, so the order is deterministic).
// Several edge types from one message to the same target keep the highest
// weight — the ranker's own edge-prior rule (its recency decay does not
// apply: the caller asks about the just-confirmed line, position 0).
// Edges whose kind the index does not know carry no prior and are skipped:
// absence of knowledge is not evidence. An unknown message, or one whose
// outgoing edges all carry no prior, returns nil. limit <= 0 means no cap.
func (x *Index) Successors(messageID string, limit int) []Successor {
	o, ok := x.ords[messageID]
	if !ok || limit == 0 {
		return nil
	}
	best := make(map[int32]float64)
	for p := x.outOff[o]; p < x.outOff[o+1]; p++ {
		prior := baseEdgePrior(edgeKindAt(x.outKind[p])) * float64(x.outConf[p])
		if prior <= 0 {
			continue
		}
		if to := x.outTo[p]; prior > best[to] {
			best[to] = prior
		}
	}
	out := make([]Successor, 0, len(best))
	for to, w := range best {
		out = append(out, Successor{MessageID: x.ids[to], Weight: w})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		return out[i].MessageID < out[j].MessageID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// prefixRange returns the index span of sortedKeys whose key starts with prefix.
// The scan is bounded by maxKeys, so a two-character prefix on a corpus where
// every line starts with "the " costs a fixed amount instead of touching 10k keys.
func (x *Index) prefixRange(prefix string, maxKeys int) (ords []int32, examined int, truncated bool) {
	if prefix == "" {
		return nil, 0, true
	}
	start := sort.SearchStrings(x.sortedKeys, prefix)
	for i := start; i < len(x.sortedKeys); i++ {
		if examined == maxKeys {
			return ords, examined, true
		}
		if !strings.HasPrefix(x.sortedKeys[i], prefix) {
			break
		}
		examined++
		ords = append(ords, x.exact[x.sortedKeys[i]]...)
	}
	return ords, examined, false
}
