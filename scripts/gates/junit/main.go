// Command junit summarises a `go test -json` stream for the make gates.
//
// Usage:
//
//	junit OUT-XML [IN-JSON]        (IN-JSON defaults to stdin)
//
// It reads the stream line by line, prints one line per test —
// PASS / FAIL / SKIP / NOT RUN — and writes a JUnit-style XML summary to
// OUT-XML. A test skipped with a message starting "NOT RUN" (the gate
// convention for "capability not configured on this host"; see
// docs/dev-environment.md "Gates") is reported as NOT RUN and its message
// is printed as the setup instruction. Nothing here is new behaviour:
// the classification only reads what `go test -json` already recorded.
//
// Exit codes:
//
//	0  every test ran and passed
//	1  any test failed, or no test ran at all
//	2  nothing failed but at least one test was NOT RUN
package main

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// event is one `go test -json` line.
type event struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// testrec is the recorded state of one (possibly sub) test.
type testrec struct {
	pkg     string
	name    string
	status  string // final action: pass | fail | skip
	elapsed float64
	output  []string
	oleng   int
}

// outputCap bounds the recorded output per test (tail is kept).
const outputCap = 256 * 1024

func (r *testrec) addOutput(s string) {
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		if len(line) > outputCap {
			line = line[:outputCap] + "…"
		}
		r.output = append(r.output, line)
		r.oleng += len(line) + 1
		for r.oleng > outputCap && len(r.output) > 1 {
			r.oleng -= len(r.output[0]) + 1
			r.output = r.output[1:]
		}
	}
}

// notRunMessage returns the skip message if any recorded line carries the
// NOT RUN convention (after the usual file:line: prefix), else "".
func (r *testrec) notRunMessage() string {
	for _, line := range r.output {
		i := strings.Index(line, "NOT RUN")
		if i >= 0 {
			return strings.TrimSpace(line[i:])
		}
	}
	return ""
}

// skipMessage is a printable message for a plain skip: the first recorded
// line that is not go-test bookkeeping ("=== RUN", "--- SKIP", …).
func (r *testrec) skipMessage() string {
	for _, line := range r.output {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "===") || strings.HasPrefix(t, "---") {
			continue
		}
		return t
	}
	return ""
}

func (r *testrec) tail(n int) string {
	s := strings.Join(r.output, "\n")
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// JUnit-style XML (a consumer-friendly subset; NOT RUN is a <skipped>
// element whose message starts "NOT RUN:").
type failure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}
type skipped struct {
	Message string `xml:"message,attr"`
}
type testcase struct {
	XMLName xml.Name `xml:"testcase"`
	Name    string   `xml:"name,attr"`
	Class   string   `xml:"classname,attr"`
	Time    float64  `xml:"time,attr"`
	Failure *failure `xml:"failure,omitempty"`
	Skipped *skipped `xml:"skipped,omitempty"`
}
type testsuite struct {
	XMLName   xml.Name   `xml:"testsuite"`
	Name      string     `xml:"name,attr"`
	Tests     int        `xml:"tests,attr"`
	Failures  int        `xml:"failures,attr"`
	Errors    int        `xml:"errors,attr"`
	Skipped   int        `xml:"skipped,attr"`
	Testcases []testcase `xml:"testcase"`
}
type testsuites struct {
	XMLName   xml.Name    `xml:"testsuites"`
	Name      string      `xml:"name,attr"`
	Timestamp string      `xml:"timestamp,attr"`
	Tests     int         `xml:"tests,attr"`
	Failures  int         `xml:"failures,attr"`
	Errors    int         `xml:"errors,attr"`
	Skipped   int         `xml:"skipped,attr"`
	Suites    []testsuite `xml:"testsuite"`
}

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: junit OUT-XML [IN-JSON]")
		os.Exit(1)
	}
	outPath := os.Args[1]
	var in *os.File
	if len(os.Args) == 3 {
		f, err := os.Open(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "junit: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		in = f
	} else {
		in = os.Stdin
	}

	tests := map[string]*testrec{}
	var order []string
	add := func(pkg, name string) *testrec {
		key := pkg + "\x00" + name
		r, ok := tests[key]
		if !ok {
			r = &testrec{pkg: pkg, name: name}
			tests[key] = r
			order = append(order, key)
		}
		return r
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Test == "" {
			continue // package-level event
		}
		r := add(ev.Package, ev.Test)
		switch ev.Action {
		case "run":
			r.status = "run"
		case "output":
			r.addOutput(ev.Output)
		case "pass", "fail", "skip":
			r.status = ev.Action
			if ev.Elapsed > 0 {
				r.elapsed = ev.Elapsed
			}
			if ev.Output != "" {
				r.addOutput(ev.Output)
			}
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "junit: reading input: %v\n", err)
		os.Exit(1)
	}

	pkgs := map[string]bool{}
	var (
		pass, fail, skip, notrun int
	)
	for _, key := range order {
		r := tests[key]
		switch r.status {
		case "pass":
			pass++
			fmt.Printf("PASS  %s (%s)\n", r.name, r.pkg)
		case "fail":
			fail++
			fmt.Printf("FAIL  %s (%s)\n", r.name, r.pkg)
		case "skip":
			if msg := r.notRunMessage(); msg != "" {
				notrun++
				fmt.Printf("NOT RUN %s (%s): %s\n", r.name, r.pkg, msg)
			} else {
				skip++
				m := r.skipMessage()
				fmt.Printf("SKIP  %s (%s): %s\n", r.name, r.pkg, firstNonEmpty(m, "(no message recorded)"))
			}
		default: // "run": interrupted before a terminal action
			fail++
			fmt.Printf("FAIL  %s (%s): interrupted before a terminal action\n", r.name, r.pkg)
		}
		pkgs[r.pkg] = true
	}

	fmt.Printf("junit: %d PASS, %d FAIL, %d SKIP, %d NOT RUN\n", pass, fail, skip, notrun)

	// Per-package suites, package order stable.
	var pkgNames []string
	for p := range pkgs {
		pkgNames = append(pkgNames, p)
	}
	sort.Strings(pkgNames)
	root := testsuites{Name: "go test", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	for _, p := range pkgNames {
		var ts testsuite
		ts.Name = p
		for _, key := range order {
			r := tests[key]
			if r.pkg != p {
				continue
			}
			tc := testcase{Name: r.name, Class: r.pkg, Time: r.elapsed}
			switch r.status {
			case "pass":
			case "fail":
				tc.Failure = &failure{Message: "test failed", Body: r.tail(4096)}
			case "skip":
				msg := r.notRunMessage()
				if msg == "" {
					msg = "skipped: " + firstNonEmpty(r.skipMessage(), "no message recorded")
				}
				tc.Skipped = &skipped{Message: msg}
				ts.Skipped++
			default:
				tc.Failure = &failure{Message: "interrupted before a terminal action", Body: r.tail(4096)}
			}
			if tc.Failure != nil {
				ts.Failures++
			}
			ts.Tests++
			ts.Testcases = append(ts.Testcases, tc)
		}
		root.Suites = append(root.Suites, ts)
		root.Tests += ts.Tests
		root.Failures += ts.Failures
		root.Skipped += ts.Skipped
	}

	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "junit: creating %s: %v\n", outPath, err)
		os.Exit(1)
	}
	defer f.Close()
	enc := xml.NewEncoder(f)
	enc.Indent("", "  ")
	if err := enc.Encode(root); err != nil {
		fmt.Fprintf(os.Stderr, "junit: encoding %s: %v\n", outPath, err)
		os.Exit(1)
	}
	if err := enc.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "junit: flushing %s: %v\n", outPath, err)
		os.Exit(1)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "junit: closing %s: %v\n", outPath, err)
		os.Exit(1)
	}

	if fail > 0 || len(order) == 0 {
		os.Exit(1)
	}
	if notrun > 0 {
		os.Exit(2)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
