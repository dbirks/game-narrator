package cast

import (
	"fmt"
	"strings"
)

// SelectFix narrows the MISMATCH rows a --fix run redesigns to the
// comma-separated character names in only (empty: every mismatch). The
// expected class comes from the profile's text, which can be wrong, so the
// operator names the rows whose expectation is right. A name that is not a
// mismatch row is an error, not a silent no-op.
func SelectFix(mismatches []Row, only string) ([]Row, error) {
	if strings.TrimSpace(only) == "" {
		return mismatches, nil
	}
	want := map[string]bool{}
	for _, n := range strings.Split(only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	var out []Row
	for _, m := range mismatches {
		if want[m.Character] {
			out = append(out, m)
			delete(want, m.Character)
		}
	}
	for n := range want {
		return nil, fmt.Errorf("--only %q is not a MISMATCH row of this profile", n)
	}
	return out, nil
}
