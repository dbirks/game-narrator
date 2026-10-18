# verbose-to-gotest-json.awk — translate a compiled go test binary's -test.v
# text output (the human form of what `go test -json` would have produced,
# minus the wrapper) into the `go test -json` event stream that
# scripts/gates/junit consumes.
#
# Only the fields the summariser reads are emitted: Action, Package, Test,
# Output, Elapsed. The package name is passed with -v pkg=...
#
# Input lines handled (the standard serial test binary format):
#
#   === RUN   TestName[/sub]
#   <indented output lines, any count>
#   --- PASS|FAIL|SKIP: TestName (N.NNNs)
#   PASS / FAIL / ok <pkg> ... (package trailer: ignored)
#
# A terminal line that arrives without a preceding RUN still records an
# event: a panic before the RUN line is a FAIL the gate must show.

function jesc(s) {
	gsub(/\\/, "\\\\", s)
	gsub(/"/, "\\\"", s)
	gsub(/\t/, "\\t", s)
	gsub(/\r/, " ", s)
	return s
}

function emit(action, elapsed) {
	if (test == "")
		return
	if (elapsed == "")
		printf("{\"Action\":\"%s\",\"Package\":\"%s\",\"Test\":\"%s\"}\n",
			action, jesc(pkg), jesc(test))
	else
		printf("{\"Action\":\"%s\",\"Package\":\"%s\",\"Test\":\"%s\",\"Elapsed\":%s}\n",
			action, jesc(pkg), jesc(test), elapsed)
}

/^=== RUN   / {
	test = substr($0, 11)  # "=== RUN" + three spaces: the name starts at column 11
	emit("run", "")
	next
}

/^=== (CONT|PAUSE|NAME) / { next }
/^(PASS|FAIL)([ \t]|$)/ { next }   # package trailer, not a test line
/^ok / { next }
/^exit status/ { next }

/^[ \t]*--- (PASS|FAIL|SKIP): / {
	# Subtest terminal lines are indented under the parent's terminal line in
	# the -test.v stream (the parent's line arrives first), so the rule must
	# accept leading whitespace; an indented line that fell through to the
	# output rule lost the subtest's terminal event and the summariser
	# reported "interrupted before a terminal action".
	# The name is everything after the "action: " prefix up to the trailing
	# " (N.NNNs)": taking a field ($3) would break names with spaces, while
	# the RUN rule copies the whole rest of the line, so the two forms must
	# stay identical.
	line = $0
	sub(/^[ \t]*--- [A-Z]+: /, "", line)
	elapsed = ""
	if (match(line, /\(([0-9.]+)s\)$/)) {
		elapsed = substr(line, RSTART + 1, RLENGTH - 2) + 0
		sub(/ \([0-9.]+s\)$/, "", line)  # the space before the paren is not part of the name
	}
	test = line
	action = $2
	sub(/:$/, "", action)
	emit(tolower(action), elapsed)
	test = ""
	next
}

{
	# Any other line is test output. Lines before the first RUN (harness
	# banner noise) are dropped: the summariser only records output under a
	# named test.
	if (test != "")
		printf("{\"Action\":\"output\",\"Package\":\"%s\",\"Test\":\"%s\",\"Output\":\"%s\\n\"}\n",
			jesc(pkg), jesc(test), jesc($0))
	next
}
