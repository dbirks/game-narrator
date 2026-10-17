// Package scenario loads the docs/spec/17 "E2E scenario YAML" and evaluates
// its assertions against one run's event log, the pipeline's self-report and
// the clip's ground truth.
//
// The scenario format is the small fixed shape docs/spec/17 documents, and the
// only YAML this project parses, so it gets a minimal parser instead of a YAML
// dependency (the milestone forbids new Go modules). The parser deliberately
// covers exactly what the format needs:
//
//	block maps, block lists of mappings, plain scalars, full-line and trailing
//	comments, space indentation.
//
// No flow style ({}, []), anchors, multi-line scalars or tabs. Anything else
// gets a line-numbered error naming what was expected, which is more useful to
// a profile author than a generic "mapping values are not allowed in this
// context".
package scenario

import (
	"fmt"
	"strings"
)

// ykind tags one node of the parsed tree.
type ykind int

const (
	ymap ykind = iota
	ylist
	yscalar
)

// ynode is one parsed node. A scalar node carries the raw string and the
// conversion that actually applied: a value written "03" is an int, one written
// "1500" (quoted) stays a string, and the loader reports the mismatch instead
// of silently re-typing it.
type ynode struct {
	kind ykind
	line int

	// scalar
	str    string // raw value, or the unquoted value for quoted scalars
	num    int64
	fnum   float64
	bval   bool
	hasNum bool
	hasF   bool
	hasB   bool

	// map, ordered so errors and reports are stable
	keys []string
	vals map[string]*ynode

	// list
	items []*ynode
}

// yline is one physical line after comment stripping, numbered from 1.
type yline struct {
	num    int
	indent int
	body   string
}

// splitYAML breaks src into parseable lines: comments and blanks dropped,
// indentation measured, tabs refused.
func splitYAML(src string) ([]yline, error) {
	var out []yline
	for i, s := range strings.Split(src, "\n") {
		lineNo := i + 1
		if strings.HasSuffix(s, "\r") {
			s = s[:len(s)-1]
		}
		trimmed := strings.TrimSpace(s)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		body, err := stripTrailingComment(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", lineNo, err)
		}
		if body == "" {
			continue
		}
		if strings.Contains(body, "\t") {
			return nil, fmt.Errorf("line %d: tabs are not supported in this YAML subset, use spaces", lineNo)
		}
		indent := 0
		for indent < len(body) && body[indent] == ' ' {
			indent++
		}
		out = append(out, yline{num: lineNo, indent: indent, body: strings.TrimLeft(body, " ")})
	}
	return out, nil
}

// stripTrailingComment cuts a trailing "# …" that is outside quotes and
// preceded by whitespace, the way YAML does. A '#' inside a value or at the
// start of a word is data.
func stripTrailingComment(s string) (string, error) {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				if quote == '"' && i > 0 && s[i-1] == '\\' {
					continue // escaped close quote; the pair keeps running
				}
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#':
			if i == 0 || s[i-1] == ' ' {
				return strings.TrimRight(s[:i], " "), nil
			}
		}
	}
	if quote != 0 {
		return "", fmt.Errorf("unterminated %c quote", quote)
	}
	return s, nil
}

// parseYAML parses a complete document; the top level must be a mapping.
func parseYAML(src string) (*ynode, error) {
	lines, err := splitYAML(src)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("scenario: empty document")
	}
	if lines[0].indent != 0 {
		return nil, fmt.Errorf("line %d: top-level keys must start at column 0 (bad indentation)", lines[0].num)
	}
	node, next, err := parseBlock(lines, 0, 0)
	if err != nil {
		return nil, err
	}
	if next != len(lines) {
		return nil, fmt.Errorf("line %d: unexpected content after the document ended", lines[next].num)
	}
	if node.kind != ymap {
		return nil, fmt.Errorf("line %d: the top level must be a mapping", node.line)
	}
	return node, nil
}

// parseBlock parses the block whose level lines sit at indentation ind.
func parseBlock(lines []yline, pos, ind int) (*ynode, int, error) {
	ln := lines[pos]
	if ln.indent != ind {
		return nil, 0, fmt.Errorf("line %d: bad indentation (want %d spaces, got %d)", ln.num, ind, ln.indent)
	}
	if isListDash(ln.body) {
		return parseList(lines, pos, ind)
	}
	return parseMap(lines, pos, ind)
}

func isListDash(body string) bool { return body == "-" || strings.HasPrefix(body, "- ") }

// parseMap parses mapping keys at indentation ind. It stops at the first line
// with less indentation, which hands control back to the parent level.
func parseMap(lines []yline, pos, ind int) (*ynode, int, error) {
	node := &ynode{kind: ymap, line: lines[pos].num, vals: map[string]*ynode{}}
	for pos < len(lines) {
		ln := lines[pos]
		switch {
		case ln.indent < ind:
			return node, pos, nil
		case ln.indent > ind:
			return nil, 0, fmt.Errorf("line %d: bad indentation (extra spaces before a key)", ln.num)
		}
		if isListDash(ln.body) {
			return nil, 0, fmt.Errorf("line %d: list item where a mapping key is expected", ln.num)
		}
		key, val, ok := splitKey(ln.body)
		if !ok {
			return nil, 0, fmt.Errorf("line %d: expected 'key: value', got %q", ln.num, ln.body)
		}
		if _, dup := node.vals[key]; dup {
			return nil, 0, fmt.Errorf("line %d: duplicate key %q", ln.num, key)
		}
		pos++
		var child *ynode
		if val != "" {
			child = parseScalar(val, ln.num)
		} else {
			if pos >= len(lines) {
				return nil, 0, fmt.Errorf("line %d: key %q has no value", ln.num, key)
			}
			if lines[pos].indent <= ind {
				return nil, 0, fmt.Errorf("line %d: key %q has no value and no nested block", ln.num, key)
			}
			var err error
			child, pos, err = parseBlock(lines, pos, lines[pos].indent)
			if err != nil {
				return nil, 0, err
			}
		}
		node.keys = append(node.keys, key)
		node.vals[key] = child
	}
	return node, pos, nil
}

// parseList parses '- ' items at indentation ind. Each item is a mapping whose
// first key shares the dash's line: the dash line is rewritten without the dash
// and parsed as a mapping at indent+2, the column its follow-up keys align to.
func parseList(lines []yline, pos, ind int) (*ynode, int, error) {
	node := &ynode{kind: ylist, line: lines[pos].num}
	for pos < len(lines) {
		ln := lines[pos]
		switch {
		case ln.indent < ind:
			return node, pos, nil
		case ln.indent > ind:
			return nil, 0, fmt.Errorf("line %d: bad indentation (list items must align)", ln.num)
		}
		if !isListDash(ln.body) {
			return node, pos, nil // a key at this indent ends the list
		}
		rest := strings.TrimSpace(ln.body[1:])
		if rest == "" {
			pos++
			if pos >= len(lines) || lines[pos].indent <= ind {
				return nil, 0, fmt.Errorf("line %d: empty list item", ln.num)
			}
			child, np, err := parseBlock(lines, pos, lines[pos].indent)
			if err != nil {
				return nil, 0, err
			}
			node.items = append(node.items, child)
			pos = np
			continue
		}
		if _, _, ok := splitKey(rest); !ok {
			// A scalar list item (names: [frame.observed, …]). Not a mapping
			// start, and the format's only use of the shape is a list of event
			// names.
			node.items = append(node.items, parseScalar(rest, ln.num))
			pos++
			continue
		}
		virtual := make([]yline, 0, len(lines)-pos)
		virtual = append(virtual, yline{num: ln.num, indent: ind + 2, body: rest})
		virtual = append(virtual, lines[pos+1:]...)
		child, consumed, err := parseMap(virtual, 0, ind+2)
		if err != nil {
			return nil, 0, err
		}
		node.items = append(node.items, child)
		pos = pos + consumed // the dash line plus the keys it introduced
	}
	return node, pos, nil
}
