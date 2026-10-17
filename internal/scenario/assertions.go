// parseAssertions turns the assertion list into validated structs: one
// pass over the fields of each item, so every error names its line and the
// field it came from.

package scenario

import "fmt"

func parseAssertions(items []*ynode) ([]Assertion, error) {
	out := make([]Assertion, 0, len(items))
	for i, item := range items {
		if item.kind != ymap {
			return nil, fmt.Errorf("line %d: assertion %d must be a mapping", item.line, i+1)
		}
		typ, line, ok := item.scalarStr("type")
		if !ok {
			return nil, fmt.Errorf("line %d: assertion %d: type is required (string)", item.line, i+1)
		}
		if !knownAssertionTypes[typ] {
			return nil, fmt.Errorf("line %d: unknown assertion type %q (known: message_detected, message_committed_count, speech_cancelled, no_duplicate_commit, no_stale_generation_applied, events_order, no_stale_speech, no_duplicate_speech, late_result_discarded, duplicate_result_discarded, no_voice_substitution, fault_count, committed_only, provider_degraded)", line, typ)
		}
		a := Assertion{Type: typ, Line: line}
		allowed := allowedFields[typ]
		for _, k := range item.keys {
			if k == "type" {
				continue
			}
			if !allowed[k] {
				l, _ := item.child(k)
				return nil, fmt.Errorf("line %d: unexpected field %q for assertion type %q", l.line, k, typ)
			}
			switch k {
			case "message_id", "reason":
				v, l, ok := item.scalarStr(k)
				if !ok || v == "" {
					return nil, fmt.Errorf("line %d: %s is required (non-empty string) for %q", l, k, typ)
				}
				if k == "message_id" {
					a.MessageID = v
				} else {
					a.Reason = v
				}
			case "max_delay_ms":
				v, l, ok := item.scalarInt(k)
				if !ok || v < 0 {
					return nil, fmt.Errorf("line %d: max_delay_ms must be a non-negative integer", l)
				}
				a.MaxDelayMS = v
			case "count":
				v, l, ok := item.scalarInt(k)
				if !ok || v < 0 {
					return nil, fmt.Errorf("line %d: count must be a non-negative integer", l)
				}
				a.Count = v
			case "names":
				v, l, ok := item.listAt(k)
				if !ok || len(v) == 0 {
					return nil, fmt.Errorf("line %d: names must be a non-empty list of event names", l)
				}
				for j, n := range v {
					if n.kind != yscalar || n.str == "" {
						return nil, fmt.Errorf("line %d: names[%d] must be a non-empty string", n.line, j)
					}
					a.Names = append(a.Names, n.str)
				}
			case "voice":
				v, l, ok := item.scalarStr(k)
				if !ok || v == "" {
					return nil, fmt.Errorf("line %d: voice is required (non-empty string)", l)
				}
				a.Voice = v
			case "kind":
				v, l, ok := item.scalarStr(k)
				if !ok || v == "" {
					return nil, fmt.Errorf("line %d: kind is required (non-empty string)", l)
				}
				a.Kind = v
			case "role":
				v, l, ok := item.scalarStr(k)
				if !ok || (v != "decision" && v != "tts") {
					return nil, fmt.Errorf("line %d: role must be decision or tts (got %q)", l, v)
				}
				a.Role = v
			case "recovered":
				v, l, ok := item.scalarBool(k)
				if !ok {
					return nil, fmt.Errorf("line %d: recovered must be a boolean", l)
				}
				a.Recovered = v
			}
		}
		// Required-per-type fields: a shape that silently omits evidence is a
		// different assertion than the one it looks like.
		switch typ {
		case "message_detected":
			if a.MessageID == "" {
				return nil, fmt.Errorf("line %d: message_id is required for message_detected", line)
			}
			if !item.hasKey("max_delay_ms") {
				return nil, fmt.Errorf("line %d: max_delay_ms is required for message_detected", line)
			}
		case "message_committed_count", "speech_cancelled":
			if !item.hasKey("count") {
				return nil, fmt.Errorf("line %d: count is required for %s", line, typ)
			}
		case "events_order":
			if len(a.Names) == 0 {
				return nil, fmt.Errorf("line %d: names is required for events_order", line)
			}
		case "no_stale_speech", "no_duplicate_speech", "late_result_discarded", "duplicate_result_discarded":
			if a.MessageID == "" {
				return nil, fmt.Errorf("line %d: message_id is required for %s", line, typ)
			}
		case "no_voice_substitution":
			if a.Voice == "" {
				return nil, fmt.Errorf("line %d: voice is required for %s", line, typ)
			}
		case "fault_count":
			if a.Kind == "" || !item.hasKey("count") {
				return nil, fmt.Errorf("line %d: kind and count are required for fault_count", line)
			}
		case "committed_only":
			if len(a.Names) == 0 {
				return nil, fmt.Errorf("line %d: names is required for %s", line, typ)
			}
		case "provider_degraded":
			if a.Role == "" {
				return nil, fmt.Errorf("line %d: role is required for %s", line, typ)
			}
		}
		out = append(out, a)
	}
	return out, nil
}
