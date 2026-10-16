package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Format writes the human report: an aligned STATUS  ID  SUMMARY table, grouped
// by Group, with the evidence and the fix under anything that is not OK, then a
// totals line. Layout carries no meaning beyond legibility — scripts must use
// --json.
func Format(w io.Writer, rep Report) error {
	statusWidth, idWidth := len("STATUS"), len("ID")
	for _, c := range rep.Checks {
		if n := len(c.Status); n > statusWidth {
			statusWidth = n
		}
		if n := len(c.ID); n > idWidth {
			idWidth = n
		}
	}
	indent := strings.Repeat(" ", statusWidth+2)

	group := ""
	firstGroup := true
	for _, c := range rep.Checks {
		if c.Group != group {
			if !firstGroup {
				if _, err := fmt.Fprintln(w); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "%s\n", c.Group); err != nil {
				return err
			}
			group, firstGroup = c.Group, false
		}
		if _, err := fmt.Fprintf(w, "%-*s  %-*s  %s\n", statusWidth, c.Status, idWidth, c.ID, c.Summary); err != nil {
			return err
		}
		if c.Status == StatusOK {
			continue
		}
		if detail := oneLine(c.Detail, 200); detail != "" {
			if _, err := fmt.Fprintf(w, "%s%s\n", indent, detail); err != nil {
				return err
			}
		}
		if remedy := oneLine(c.Remedy, 200); remedy != "" {
			if _, err := fmt.Fprintf(w, "%s→ %s\n", indent, remedy); err != nil {
				return err
			}
		}
	}

	t := rep.Totals
	_, err := fmt.Fprintf(w, "\nTotals: %d OK, %d DEGRADED, %d BLOCKED, %d NOT CONFIGURED, %d FAIL — %d checks, overall %s\n",
		t.OK, t.Degraded, t.Blocked, t.NotConfigured, t.Fail, t.Checks, rep.Status)
	return err
}

// FormatJSON writes the machine-readable report: the same Check fields, the
// totals, and the overall status, so CI can gate on `.status` and `.totals.fail`.
func FormatJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
