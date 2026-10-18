#!/bin/sh
# annotate-vitest.sh JSON-FILE [ROOT-TO-STRIP]
#
# Emits GitHub Actions workflow-command annotations (public via
# GET /repos/<owner>/<repo>/check-runs/<id>/annotations, no auth needed)
# for a vitest JSON report (vitest >= 5: `--reporter=json --outputFile.json=...`):
#
#   - one  ::error file=<test file>,title=<full test name>::<message>  per
#     failing test, where <message> is the first 3 lines of the test's
#     recorded failure message (assertion error / thrown error), capped
#     at 20 annotations; test-file paths are made relative to
#     ROOT-TO-STRIP (e.g. the GITHUB_WORKSPACE in CI) so the annotation
#     links to the file;
#   - one  ::notice  with the PASS / FAIL / SKIP (pending) totals.
#
# Always exits 0: annotation is best-effort. The gate's exit code comes
# from vitest itself (see .github/workflows/ci.yml).
set -u

json=${1:-}
root=${2:-}

if [ -z "$json" ] || [ ! -f "$json" ]; then
	echo "::notice::web tests: no vitest JSON report to annotate (test step did not run?)"
	exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
	echo "::notice::web tests: jq not available, skipping annotations"
	exit 0
fi

exec jq -R -s -r --arg root "$root" '
	def esc_data: gsub("%"; "%25") | gsub("\r"; "%0D") | gsub("\n"; "%0A");
	def esc_prop: esc_data | gsub(":"; "%3A") | gsub(","; "%2C");
	def rel: if $root != "" and startswith($root + "/") then (ltrimstr($root + "/")) else . end;

	# The vitest JSON report is a single JSON document (possibly with a
	# trailing newline); fromjson? tolerates anything else.
	[ fromjson? ]
	| .[0] // {}
	| ( "::notice::web tests: \((.numPassedTests // 0) | tostring) PASS, \((.numFailedTests // 0) | tostring) FAIL, \((.numPendingTests // 0) | tostring) SKIP"
			+ (if ((.numTodoTests // 0) > 0) then " (\((.numTodoTests // 0)) TODO)" else "" end)
		  ),
	  ( [ .testResults[]?
		    | .name as $file
		    | (.assertionResults // [])[]
		    | select(.status == "failed")
		    | { file: ($file | rel),
			title: (.fullName // .title // "test"),
			message: ([ (.failureMessages // [])[] | split("\n")[] ]
				| map(gsub("^[ \t]+"; "") | gsub("[ \t]+$"; ""))
				| map(select(. != ""))
				| .[0:3]
				| if length == 0 then "(no failure output recorded)"
					 else join("\n") end)
			}
		  ]
		| .[0:20]
		| map("::error file=\(.file | esc_prop),title=\(.title | esc_prop)::\(.message | esc_data)")
		| .[]
	  )
' "$json"
