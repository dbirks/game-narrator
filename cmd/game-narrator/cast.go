package main

// The cast audit command (ADR-0171): `game-narrator cast audit --profile <id>`
// measures every assigned voice's reference with the YIN estimator and
// compares the measured voice class with the character's expected class.
// `--fix` redesigns each MISMATCH through the voice-design path with a brief
// that states the expected gender/age explicitly; it is dry-run by default
// (`--apply` executes) and leaves the new takes PENDING approval unless
// `--approve` is passed (voice selection is reviewable, AGENTS.md rule 6).
//
// Logs and output carry the character's own profile name, the class labels
// and Hz numbers — never game text (AGENTS.md rule 5).

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/cast"
)

const castUsageText = `usage: game-narrator cast <subcommand> [flags]

subcommands:
  audit   measure each assigned voice's reference pitch against the character's expected voice class

audit flags:
  --profile ID   the profile to audit (required)
  --json         print the result as JSON
  --fix          redesign the MISMATCH voices (dry-run unless --apply)
  --apply        with --fix: execute the redesigns (default is dry-run)
  --approve      with --fix --apply: approve the fixed takes instead of leaving them pending
  --only NAMES   with --fix: comma-separated character names to fix (default: every MISMATCH)
  --data-dir DIR durable data directory

exit codes: 0 all rows OK or UNKNOWN; 1 mismatches remain; 2 bad usage.
`

func runCast(args []string, log *slog.Logger) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, castUsageText)
		return 2, fmt.Errorf("%w: cast requires a subcommand", errUsage)
	}
	switch args[0] {
	case "audit":
		return runCastAudit(args[1:], log)
	case "help", "-h", "--help":
		fmt.Print(castUsageText)
		return 0, nil
	}
	fmt.Fprintf(os.Stderr, "game-narrator cast: unknown subcommand %q\n\n%s", args[0], castUsageText)
	return 2, fmt.Errorf("%w: unknown subcommand %q", errUsage, args[0])
}

// castAuditReport is the --json shape (and the API's response body).
type castAuditReport struct {
	Profile string `json:"profile"`
	Rows    []struct {
		Character    string  `json:"character"`
		VoiceID      string  `json:"voice_id,omitempty"`
		Expected     string  `json:"expected"`
		MeasuredF0Hz float64 `json:"measured_f0_hz"`
		Measured     string  `json:"measured"`
		Verdict      string  `json:"verdict"`
	} `json:"rows"`
}

func runCastAudit(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("cast audit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, castUsageText) }
	profileID := fs.String("profile", "", "the profile to audit (required)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fix := fs.Bool("fix", false, "redesign the MISMATCH voices (dry-run unless --apply)")
	apply := fs.Bool("apply", false, "execute the --fix redesigns (default is dry-run)")
	approve := fs.Bool("approve", false, "approve the fixed takes instead of leaving them pending")
	only := fs.String("only", "", "with --fix: comma-separated character names to fix (default: every MISMATCH); the expected class comes from the profile data, so fix only rows whose expectation is right")
	dataDir := fs.String("data-dir", "", "durable data directory; overrides "+bootstrap.EnvDataDir)
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return 2, fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}
	if *profileID == "" {
		return 2, fmt.Errorf("%w: --profile is required", errUsage)
	}

	cfg, err := bootstrap.LoadConfig("", *dataDir, os.LookupEnv)
	if err != nil {
		return 2, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openDataStore(ctx, cfg.DataDir, log)
	if err != nil {
		return 1, err
	}
	defer func() { _ = st.Close() }()
	assetStore, err := assets.NewLocal(AssetDir(cfg.DataDir), assets.WithMaxSize(maxAssetBlobBytes))
	if err != nil {
		return 1, fmt.Errorf("opening the asset store: %w", err)
	}

	aud := cast.NewAuditor(st, assetStore)
	rows, err := aud.Profile(ctx, *profileID)
	if err != nil {
		return 1, err
	}
	mismatches := cast.Mismatches(rows)
	toFix, err := cast.SelectFix(mismatches, *only)
	if err != nil {
		return 2, fmt.Errorf("%w: %v", errUsage, err)
	}

	if *fix && len(toFix) > 0 {
		if !*apply {
			for _, m := range toFix {
				fmt.Printf("dry-run: %s (voice %s): expected %s, measured %.1f Hz (%s) — would redesign\n",
					m.Character, m.VoiceID, m.Expected, m.MeasuredF0Hz, m.Measured)
			}
			fmt.Printf("re-run with --apply to redesign %d mismatched voice(s)\n", len(toFix))
			return 1, nil
		}
		svc, closer, err := newVoiceWiring(st, assetStore, log, nil)
		if err != nil {
			return 1, err
		}
		defer closer()
		if svc == nil {
			return 1, fmt.Errorf("cast fix: the voice design endpoints are not configured (see the serve log's missing-role list)")
		}
		fixer := &cast.Fixer{Aud: aud, Svc: svc}
		for _, m := range toFix {
			res, err := fixer.FixCharacter(ctx, *profileID, m.Character, m.Expected, m.MeasuredF0Hz, *approve)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fix %s: %v\n", m.Character, err)
				return 1, err
			}
			state := "pending review"
			if res.Approved {
				state = "approved"
			}
			fmt.Printf("%s: %s -> %s (voice %s, %d take(s); %.1f Hz -> %.1f Hz; %s)\n",
				m.Character, m.Expected, res.Verdict, res.NewVoiceID, res.Attempts,
				res.BeforeF0Hz, res.NewF0Hz, state)
		}
	}

	if *asJSON {
		rep := castAuditReport{Profile: *profileID}
		for _, r := range rows {
			rep.Rows = append(rep.Rows, struct {
				Character    string  `json:"character"`
				VoiceID      string  `json:"voice_id,omitempty"`
				Expected     string  `json:"expected"`
				MeasuredF0Hz float64 `json:"measured_f0_hz"`
				Measured     string  `json:"measured"`
				Verdict      string  `json:"verdict"`
			}{r.Character, r.VoiceID, r.Expected, r.MeasuredF0Hz, r.Measured, r.Verdict})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return 1, err
		}
		return exitForMismatches(mismatches), nil
	}

	w := tabwriter.NewWriter(stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(w, "CHARACTER\tVOICE\tEXPECTED\tMEASURED (Hz)\tCLASS\tVERDICT")
	sorted := append([]cast.Row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Character < sorted[j].Character })
	for _, r := range sorted {
		fmt.Fprintf(w, "%s\t%s\t%s\t%.1f\t%s\t%s\n",
			r.Character, r.VoiceID, r.Expected, r.MeasuredF0Hz, r.Measured, r.Verdict)
	}
	if err := w.Flush(); err != nil {
		return 1, err
	}
	return exitForMismatches(mismatches), nil
}

// exitForMismatches is the script contract: 0 when nothing is wrong (OK or
// UNKNOWN rows), 1 when mismatches remain after any fix run.
func exitForMismatches(m []cast.Row) int {
	if len(m) > 0 {
		return 1
	}
	return 0
}
