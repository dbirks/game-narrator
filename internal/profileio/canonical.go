package profileio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Canonicalization and the manifest digest.
//
// docs/spec/02 asks for "strong SHA-256 on raw data and canonical JSON
// manifests"; docs/spec/17 names the field `manifest_digest`; docs/spec/16
// requires an exported profile to be re-imported *and verified*. Those three
// statements only fit together if the digest survives a re-import, and a
// re-import mints new row ids by design (docs/spec/02 "Import resume", ADR-0015's
// snapshot rule). So the digest is taken over a *canonical projection* of the
// manifest:
//
//   - object keys sorted, no insignificant whitespace, no HTML escaping;
//   - `manifest_digest`, `profile_id` and the `publication` section removed:
//     they name a revision row and its lifecycle, which a re-import resets on
//     purpose (importing a manifest does not re-publish anything);
//   - every row id replaced by a positional alias derived from the row's
//     *content* rather than from the id in the database;
//   - each section's rows ordered by their own canonical bytes, so the result is
//     independent of database row order;
//   - a message's `pages` array ordered by its own `index`, because the ordinal
//     *is* the order and the database returns page rows sorted by it (see
//     orderPages);
//   - a source row's `uri` and `retrieved_at`, which describe *this* store's copy
//     of the bytes rather than their content (see volatileSourceFields).
//
// Alias assignment runs in dependency order (sources → characters → scenes →
// regions → messages → speaker_assertions), because a row's content includes the
// references it makes and those need stable labels before the row's own key means
// anything. Within a section, rows are sorted by their canonical bytes with
// references already aliased and then numbered (`msg#0`, `chr#1`, …). An id the
// document defines but has not aliased yet becomes `@unresolved`, so a raw
// database id never reaches the digest input at all.
//
// The residual limitation is stated rather than hidden: two rows that differ *only*
// by a forward reference (an id inside a JSON payload pointing at a section not
// yet refined) share a sort key and are ordered by their raw ids, so such a pair
// could be numbered differently after a re-import, and their digests would
// disagree. Manifests this package exports have no forward references — adapters
// put labels and offsets in payloads, not row ids — and the failure mode is
// conservative: a digest mismatch refuses the import rather than admitting
// different content. That is a canonical labelling, not full graph canonization,
// and it is recorded here because the difference is exactly what a reader
// comparing this to store.contentDigest (which hashes raw ids and therefore does
// *not* survive a re-import) will ask about.

// unresolvedRef stands in for a document id that has no alias yet.
const unresolvedRef = "@unresolved"

// aliasSections names the id-bearing sections in refinement order: a row may only
// reference rows defined by an earlier section. `message_edges` carries no id of
// its own (its identity is the (from, to, type) triple), so it is canonicalized
// but never aliased.
var aliasSections = []struct {
	name   string
	idKey  string
	prefix string
}{
	{"sources", "source_id", "src"},
	{"characters", "id", "chr"},
	{"scenes", "id", "scn"},
	{"regions", "id", "reg"},
	{"messages", "id", "msg"},
	{"speaker_assertions", "id", "asr"},
}

// sectionNames lists every array section of the document. The emitted order is
// content-derived; this list only says which sections exist and must be ordered.
var sectionNames = []string{"characters", "message_edges", "messages", "regions", "scenes", "speaker_assertions", "sources"}

// digestExcluded names the top-level fields the digest projection drops.
var digestExcluded = map[string]bool{"manifest_digest": true, "profile_id": true, "publication": true}

// volatileSourceFields are source columns a re-import legitimately re-derives: the
// asset address, which defaults to `asset://<sha256>` of the bytes the row already
// names, and the moment *this* database first read them. They describe the local
// copy, not the content — the same reasoning that keeps `profile_id` and the
// publication section out of the fingerprint — so a hand-authored manifest that
// states neither, and the export of the profile it imported, agree.
var volatileSourceFields = []string{"uri", "retrieved_at"}

// ContentDigest is the hex SHA-256 of the canonical projection: the manifest's
// content fingerprint, independent of the ids of the database that exported it.
func ContentDigest(m Manifest) string {
	_, proj := canonicalize(m)
	sum := sha256.Sum256(proj)
	return hex.EncodeToString(sum[:])
}

// Stamp sets m.Digest to its content fingerprint and returns it. Export stamps; a
// caller assembling a manifest by hand must stamp before writing one out.
func Stamp(m *Manifest) string {
	m.Digest = ContentDigest(*m)
	return m.Digest
}

// Verify reports whether the digest a document carries matches a recomputation.
// An absent digest is a mismatch, not a skip: a manifest that claims nothing
// cannot be evidence that two revisions agree.
func Verify(m Manifest) error {
	want := ContentDigest(m)
	if m.Digest != want {
		return fmt.Errorf("%w: document says %q, recomputed %q", ErrDigestMismatch, m.Digest, want)
	}
	return nil
}

// Canonical returns the digest projection itself — canonical JSON with ids
// aliased and the lifecycle fields removed. The portability test compares two
// revisions by comparing exactly this, instead of writing its own canonicalizer
// and checking a copy of the algorithm.
func Canonical(m Manifest) []byte {
	_, proj := canonicalize(m)
	return proj
}

// Encode renders the whole manifest — real ids, publication section, digest — as
// canonical JSON in canonical section order. This is the byte form of a
// `.gnprofile.json` file: canonical bytes make the file itself hashable and make
// two exports diff as content differences rather than as key reorderings.
func Encode(m Manifest) ([]byte, error) {
	doc, _ := canonicalize(m)
	return canonical(doc), nil
}

// canonicalize returns the document with every section in canonical order (with
// its real ids, for Encode) and the digest projection (ids aliased, lifecycle
// fields dropped). Both orders are the same, because both are computed from the
// aliased bytes.
func canonicalize(m Manifest) (doc map[string]any, projection []byte) {
	tree, err := tree(m)
	if err != nil {
		// Only possible if a field type cannot marshal, which Manifest's cannot.
		// Surfaced as a panic because it is a programming error, and returning a
		// wrong digest for content we could not read would be worse.
		panic("profileio: cannot encode manifest: " + err.Error())
	}
	m = m.normalized()
	doc, ok := tree.(map[string]any)
	if !ok {
		panic("profileio: manifest did not decode to a JSON object")
	}

	known := knownIDs(doc)
	aliases := refineAliases(doc, known)

	// Page order is canonicalized before anything else reads a message's bytes,
	// so the listing order of a document cannot leak into a row's own sort key.
	orderPages(doc, known, aliases)

	for _, name := range sectionNames {
		if doc[name] == nil {
			// An empty section is [] on the wire: 0002_content.sql's JSON columns
			// and docs/spec/17 both use [] rather than null, and a null would make
			// "no characters" and "characters not considered" the same document.
			doc[name] = []any{}
		}
		doc[name] = orderRows(doc[name], known, aliases)
	}

	proj := make(map[string]any, 2+len(sectionNames))
	proj["schema_version"] = m.SchemaVersion
	if ed, ok := doc["edition"]; ok {
		proj["edition"] = ed
	}
	for _, name := range sectionNames {
		proj[name] = doc[name]
	}
	for field := range digestExcluded {
		delete(proj, field)
	}
	// substitute deep-copies, so the volatile columns are removed from the
	// projection alone — the exported document keeps stating where it read them.
	projected, _ := substitute(proj, known, aliases).(map[string]any)
	dropFromRows(projected["sources"], volatileSourceFields)
	projection = canonical(projected)
	return doc, projection
}

// dropFromRows removes named keys from every object row of a section in the
// digest projection.
func dropFromRows(section any, fields []string) {
	for _, row := range rows(section) {
		for _, field := range fields {
			delete(row, field)
		}
	}
}

// knownIDs collects every id the document defines, mapped to its section prefix.
// Substitution needs the set, not the prefixes; the prefixes are kept for
// diagnostics and for reading the code without guessing.
func knownIDs(doc map[string]any) map[string]string {
	out := map[string]string{}
	for _, sec := range aliasSections {
		for _, row := range rows(doc[sec.name]) {
			if id, _ := row[sec.idKey].(string); id != "" {
				out[id] = sec.prefix
			}
		}
	}
	return out
}

// refineAliases assigns a content-derived alias to every row id, in the order the
// sections may depend on one another.
func refineAliases(doc map[string]any, known map[string]string) map[string]string {
	aliases := map[string]string{}
	for _, sec := range aliasSections {
		list := rows(doc[sec.name])
		type keyed struct {
			key string
			row map[string]any
		}
		items := make([]keyed, 0, len(list))
		for _, row := range list {
			// The row's own id is not yet aliased, so it canonicalizes to
			// @unresolved and cannot influence the alias it is being given.
			items = append(items, keyed{key: string(canonical(substitute(row, known, aliases))), row: row})
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
		for i, it := range items {
			if id, _ := it.row[sec.idKey].(string); id != "" {
				aliases[id] = fmt.Sprintf("%s#%d", sec.prefix, i)
			}
		}
	}
	return aliases
}

// orderRows returns the section's rows sorted by their aliased canonical bytes.
// Keys are computed once per row: an export of a 10 000-message revision sorts on
// precomputed strings rather than re-encoding rows inside the comparator.
func orderRows(value any, known, aliases map[string]string) any {
	list, ok := value.([]any)
	if !ok {
		return value
	}
	type keyed struct {
		key   string
		value any
	}
	items := make([]keyed, 0, len(list))
	for _, row := range list {
		items = append(items, keyed{key: string(canonical(substitute(row, known, aliases))), value: row})
	}
	// Ties are content-identical rows, so which one comes first cannot change the
	// aliased bytes that decide the digest.
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.value)
	}
	return out
}

// orderPages re-sorts every message's `pages` array by its ordinal. Two documents
// that list the same pages in a different sequence denote the same content —
// docs/spec/02 stores pages as rows carrying their own ordinal, and the store
// returns them sorted — so a fingerprint that read listing order as content would
// disagree with itself after a re-import of a hand-authored document, which is
// precisely what the projection exists to survive. Ties (a document with two pages
// claiming one ordinal) fall back to the rows' aliased canonical bytes.
func orderPages(doc map[string]any, known, aliases map[string]string) {
	for _, msg := range rows(doc["messages"]) {
		pages, ok := msg["pages"].([]any)
		if !ok || len(pages) < 2 {
			continue
		}
		type keyed struct {
			ordinal float64
			key     string
			value   any
		}
		items := make([]keyed, 0, len(pages))
		for _, page := range pages {
			it := keyed{value: page}
			if row, ok := page.(map[string]any); ok {
				if n, ok := row["index"].(json.Number); ok {
					if v, err := n.Float64(); err == nil {
						it.ordinal = v
					}
				}
				it.key = string(canonical(substitute(row, known, aliases)))
			}
			items = append(items, it)
		}
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].ordinal != items[j].ordinal {
				return items[i].ordinal < items[j].ordinal
			}
			return items[i].key < items[j].key
		})
		out := make([]any, 0, len(items))
		for _, it := range items {
			out = append(out, it.value)
		}
		msg["pages"] = out
	}
}

// substitute deep-copies v, replacing every string equal to one of the document's
// row ids with its alias, or @unresolved when it has no alias yet. It is applied
// to whole subtrees rather than to the foreign-key columns alone because that is
// the rule store.remapRefs applies on the way in: an id inside a markup AST or a
// flags object is an id (docs/spec/02 keeps those payloads Go-interpreted).
func substitute(v any, known map[string]string, aliases map[string]string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = substitute(val, known, aliases)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			out = append(out, substitute(val, known, aliases))
		}
		return out
	case string:
		if _, ok := known[t]; ok {
			if alias, ok := aliases[t]; ok {
				return alias
			}
			return unresolvedRef
		}
		return t
	default:
		return v
	}
}

// tree decodes a manifest into generic JSON, keeping numbers exactly as written
// (json.Number) so canonical bytes never depend on float reformatting.
func tree(m Manifest) (any, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return out, nil
}

func rows(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

//
// ── canonical JSON emission ─────────────────────────────────────────────────
//

// canonical renders v as canonical JSON: object keys in byte order, array order
// preserved, no whitespace, no HTML escaping, numbers exactly as decoded.
func canonical(v any) []byte {
	var buf bytes.Buffer
	writeCanonical(&buf, v)
	return buf.Bytes()
}

func writeCanonical(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeCanonicalString(buf, t)
	case json.Number:
		buf.WriteString(t.String())
	case float64:
		// Only reachable from a tree built without UseNumber.
		raw, err := json.Marshal(t)
		if err != nil {
			buf.WriteString("null")
			return
		}
		buf.Write(raw)
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, item)
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			writeCanonical(buf, t[k])
		}
		buf.WriteByte('}')
	default:
		// A shape neither decoder produces. Marshal it rather than panic: a
		// future field type must not take the process down over a digest.
		raw, err := json.Marshal(t)
		if err != nil {
			buf.WriteString("null")
			return
		}
		buf.Write(raw)
	}
}

// writeCanonicalString escapes a string the way encoding/json does with HTML
// escaping turned off, so `<`, `>` and `&` in game text survive as themselves and
// the bytes do not depend on an encoder default.
func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		// Bytes >= 0x80 are part of a UTF-8 rune and are written through:
		// canonical JSON is UTF-8, and escaping non-ASCII would make the same
		// text hash differently depending on an encoder option.
		if b >= 0x80 || safeSet[b] {
			continue
		}
		if start < i {
			buf.WriteString(s[start:i])
		}
		switch b {
		case '\\', '"':
			buf.WriteByte('\\')
			buf.WriteByte(b)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			const hexDigits = "0123456789abcdef"
			buf.WriteString(`\u00`)
			buf.WriteByte(hexDigits[b>>4])
			buf.WriteByte(hexDigits[b&0xF])
		}
		start = i + 1
	}
	if start < len(s) {
		buf.WriteString(s[start:])
	}
	buf.WriteByte('"')
}

const asciiLimit = 0x80

// safeSet marks the ASCII bytes that need no escaping, following encoding/json's
// table with HTML escaping disabled.
var safeSet = func() [asciiLimit]bool {
	var set [asciiLimit]bool
	for i := range set {
		switch {
		case i == 0x22, i == 0x5c:
		case i < 0x20:
		default:
			set[i] = true
		}
	}
	return set
}()
