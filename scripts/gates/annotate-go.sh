#!/bin/sh
# annotate-go.sh JSON-FILE [LABEL]
#
# Emits GitHub Actions workflow-command annotations (public via
# GET /repos/<owner>/<repo>/check-runs/<id>/annotations, no auth needed)
# for a `go test -json` stream file:
#
#   - one  ::error file=<go package path>,title=<TestName>::<message>  per
#     failing test, capped at 20 annotations. <message> puts the lines that
#     carry the failure first — the output lines naming the failure
#     (Error / Fatal / want / got / expected / panic:) and the test's last
#     output line (the t.Fatal message the "--- FAIL" bookkeeping line
#     stands for) — then a truncated tail of the remaining output, so the
#     assertion survives GitHub's annotation size limit. Bookkeeping
#     "=== RUN" / "--- FAIL" lines and blank lines are excluded (the same
#     convention as the scripts/gates/junit summariser);
#   - a package that fails at package level without any test-level events
#     (a build failure) becomes one
#     ::error file=<pkg>,title=build failed::  annotation carrying the
#     compiler output;
#   - one  ::notice  with the PASS / FAIL / SKIP totals (plus a NOT RUN
#     count when present — the docs/dev-environment.md gate convention).
#
# Always exits 0: annotation is best-effort. The gate's exit code comes
# from `go test` itself (see .github/workflows/ci.yml).
set -u

json=${1:-}
label=${2:-"go test"}

if [ -z "$json" ] || [ ! -f "$json" ]; then
	echo "::notice::$label: no go test -json stream to annotate (test step did not run?)"
	exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
	echo "::notice::$label: jq not available, skipping annotations"
	exit 0
fi

# Parses the whole `go test -json` stream (one JSON object per line) and
# prints the final workflow-command lines. It mirrors the classification of
# scripts/gates/junit: a test is FAIL when its last action is fail, or it
# never reached a terminal action (interrupted); a SKIP whose output
# carries the "NOT RUN" convention is counted as NOT RUN in the totals.
exec jq -R -s -r --arg label "$label" '
	def esc_data: gsub("%"; "%25") | gsub("\r"; "%0D") | gsub("\n"; "%0A");
	def esc_prop: esc_data | gsub(":"; "%3A") | gsub(","; "%2C");

	# Trimmed, non-blank, non-bookkeeping output lines ("=== RUN" /
	# "--- FAIL" style lines are go-test bookkeeping, not failure output).
	def clean:
		[ .[]
		  | split("\n")[]
		  | gsub("^[ \t]+"; "") | gsub("[ \t]+$"; "")
		  | select(. != "")
		  | select((startswith("===") or startswith("---")) | not)
		];

	# The failure-carrying lines of the output of one test: the lines that
	# name the failure (Error / Fatal / want / got / expected / panic:) and
	# the last line (the t.Fatal reason). They lead the annotation message
	# so the assertion survives the GitHub annotation size limit — the last
	# line is always kept, never dropped by the cap — and a truncated tail
	# of the remaining output follows for context.
	def failmessage:
		. as $out
		| if ($out | length) == 0 then "(no failure output recorded)"
		  else
			([ $out[]
			   | select(test("Error") or test("Fatal") or test("want")
					  or test("got") or test("expected") or test("panic:")) ]
			   + $out[-1:])
			| reduce .[] as $x ([]; if any(.[]; . == $x) then . else . + [$x] end)
			| . as $all
			| ($all[0:4] + $all[-1:])
			| reduce .[] as $x ([]; if any(.[]; . == $x) then . else . + [$x] end)
			| . as $head
			| [ $out[] | select(. as $x | ($head | index($x)) | not) ] as $rest
			| ($head
			   + (if ($rest | length) > 3 then ["…"] else [] end)
			   + ($rest | .[-3:]))
			| join("\n")
		  end;

	split("\n")
	| map(select(length > 0) | fromjson?)
	| . as $evs
	| [ $evs[] | select(.Test != null) ]
	| group_by([.Package, .Test])
	| map(
			{ pkg: (.[0].Package),
			  test: (.[0].Test),
			  terminal: ([ .[].Action | select(. == "pass" or . == "fail" or . == "skip") ] | last),
			  out: ([ .[] | .Output // empty ] | clean)
			}
		) as $tests
	| ( [ $tests[] | select(.terminal == "skip")
		    | select([ .out[] | select(test("NOT RUN")) ] | any) ] | length
	  ) as $notrun
	| { pass: ([$tests[] | select(.terminal == "pass")] | length),
		fail: ([$tests[] | select(.terminal == "fail" or .terminal == null)] | length),
		skip: ([$tests[] | select(.terminal == "skip")] | length)
	} as $t
	| ( # build failures: package-level fail with no test-level events.
		[ $evs[] | select(.Test == null and .Action == "fail") | .Package ]
		| unique as $failedpkgs
		| [ $failedpkgs[]
		    | . as $p
		    | select([ $evs[] | select(.Test != null and .Package == $p) ] | length == 0)
		    | { file: $p,
			title: "build failed",
			message: ([ $evs[] | select(.Action == "build-output") | .Output // empty ]
				| clean | .[0:3]
				| if length == 0 then "(no build output recorded)"
					 else join("\n") end)
			}
		  ]
	  ) as $buildfails
	| ( "::notice::\($label): \($t.pass) PASS, \($t.fail) FAIL, \($t.skip) SKIP"
			+ (if $notrun > 0 then " (\($notrun) NOT RUN)" else "" end)
		  ),
	  ( [ ($tests[] | select(.terminal == "fail" or .terminal == null)
			| { file: .pkg,
				title: .test,
				message: (.out | failmessage)
			}),
		  $buildfails[]
		]
		| .[0:20]
		| map("::error file=\(.file | esc_prop),title=\(.title | esc_prop)::\(.message | esc_data)")
		| .[]
	  )
' "$json"
