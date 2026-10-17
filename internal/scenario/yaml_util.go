// yaml_util.go holds the scalar lexer and the typed ynode accessors that
// the loader in yaml.go consumes: quote handling, int/bool typing and the
// "key -> typed value" getters, each line-numbered for the same error style.

package scenario

import (
	"strconv"
	"strings"
)

// splitKey splits "key: value" (or "key:") into parts. The separator is a colon
// followed by a space or end of line, and the key is a plain identifier.
func splitKey(body string) (key, val string, ok bool) {
	i := strings.Index(body, ":")
	if i < 0 {
		return "", "", false
	}
	if i+1 < len(body) && body[i+1] != ' ' {
		return "", "", false
	}
	key = strings.TrimSpace(body[:i])
	val = strings.TrimSpace(body[i+1:])
	if !validYKey(key) {
		return "", "", false
	}
	return key, val, true
}

func validYKey(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// parseScalar types a plain scalar. Quoted values stay strings; true/false are
// booleans; integer and decimal literals are numbers; everything else, and the
// tokens null/~, is an empty-carrying string.
func parseScalar(raw string, line int) *ynode {
	n := &ynode{kind: yscalar, line: line, str: raw}
	switch {
	case len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"':
		n.str = raw[1 : len(raw)-1]
		return n
	case len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'':
		n.str = raw[1 : len(raw)-1]
		return n
	case raw == "true":
		n.bval, n.hasB = true, true
		return n
	case raw == "false":
		n.hasB = true
		return n
	case raw == "null" || raw == "~":
		n.str = ""
		return n
	}
	// Underscores are rejected outright: strconv would read "1_0" as the
	// integer 10, and this subset has no reason to accept a number with a
	// hole in it.
	if !strings.Contains(raw, "_") {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
			n.num, n.hasNum = v, true
			return n
		}
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !strings.ContainsAny(raw, " +-") {
			n.fnum, n.hasF = v, true
			return n
		}
	}
	return n
}

// --- typed getters -----------------------------------------------------------

// child returns the value stored under k in a mapping.
func (n *ynode) child(k string) (*ynode, bool) {
	if n == nil || n.kind != ymap {
		return nil, false
	}
	v, ok := n.vals[k]
	return v, ok
}

// scalarStr returns the string value of key k. ok is false when the key is
// missing or the value is not a scalar; line names where the mistake is.
func (n *ynode) scalarStr(k string) (val string, line int, ok bool) {
	v, ok := n.child(k)
	if !ok {
		return "", 0, false
	}
	if v.kind != yscalar {
		return "", v.line, false
	}
	return v.str, v.line, true
}

// scalarInt returns the integer value of key k. A quoted number is *not* an
// integer: the profile author typed a string, and the error says so.
func (n *ynode) scalarInt(k string) (val int, line int, ok bool) {
	v, ok := n.child(k)
	if !ok {
		return 0, 0, false
	}
	if v.kind != yscalar || !v.hasNum {
		return 0, v.line, false
	}
	return int(v.num), v.line, true
}

// scalarBool returns the boolean value of key k.
func (n *ynode) scalarBool(k string) (val bool, line int, ok bool) {
	v, ok := n.child(k)
	if !ok {
		return false, 0, false
	}
	if v.kind != yscalar || !v.hasB {
		return false, v.line, false
	}
	return v.bval, v.line, true
}

// listAt returns the list stored under key k of a mapping.
func (n *ynode) listAt(k string) ([]*ynode, int, bool) {
	v, ok := n.child(k)
	if !ok {
		return nil, 0, false
	}
	if v.kind != ylist {
		return nil, v.line, false
	}
	return v.items, v.line, true
}

// hasKey reports whether a mapping stores key k.
func (n *ynode) hasKey(k string) bool {
	_, ok := n.child(k)
	return ok
}
