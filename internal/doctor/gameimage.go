// The game.image check's identity layer: what image the local fixtures were
// recorded against, and whether the configured file is that image. The image
// is the user's private file (AGENTS.md rule 5): doctor never prints its
// path, and the only identity it may surface is the SHA-256 prefix of the
// value the fixture manifests record (the manifests live outside Git).
package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// gameImageCheckBudget bounds the game.image check. It may hash a full
// GameCube disc image (a Wind Waker CISO is ~1.1 GB) to compare it with the
// SHA-256 the fixture manifests record — the same identity check the bench
// gate performs (internal/bench). On a cold or slow disk that exceeds the
// default 15 s probe budget, so the check runs on its own budget; a probe
// that ignores even this deadline is still abandoned by the engine
// (checkGrace) and reported as FAIL, never left to hang the report.
const gameImageCheckBudget = 2 * time.Minute

// gameImageHashPrefixLen is the length of the SHA-256 prefix doctor prints.
// The full digest stays in the fixture manifests (AGENTS.md rule 5); a 12
// hex-char prefix (48 bits) is enough to verify the file at a terminal and
// matches the manifest-prefix convention the scenario HTML reports use
// (internal/scenario).
const gameImageHashPrefixLen = 12

// gameImageGates names the gates that consume the image, so a fresh host
// knows what the NOT CONFIGURED row will cost.
const gameImageGates = "the bench captured holdout (make bench, class b), make test-dolphin (real Dolphin tests and the committed scenario replay), the real-disc parse (import-disc)"

// imageExpectation is the identity evidence read from the local fixture
// manifests at run time: the distinct non-empty game_image_sha256 values
// they record, and how many manifests recorded one.
type imageExpectation struct {
	shas      []string
	manifests int
}

// fixtureImageExpectation scans the fixture root ($GAME_NARRATOR_FIXTURE_DIR,
// default the gitignored artifacts/fixtures — the root is supplied by the
// caller through the Options.FixtureRoot seam) for <id>/manifest.json files
// and collects the image SHA-256 they record. Unreadable or unparseable
// manifests are skipped rather than failing the check: the check degrades to
// size-only reporting instead of reporting a host fault the operator cannot
// act on.
func fixtureImageExpectation(root string) imageExpectation {
	var exp imageExpectation
	if root == "" {
		return exp
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", "manifest.json"))
	if err != nil {
		return exp
	}
	sort.Strings(matches)
	seen := map[string]bool{}
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		// Only the identity field is read; the rest of the manifest is the
		// dolphin package's schema and the check must not depend on its
		// evolution (and must not print any of it).
		var man struct {
			GameImageSHA256 string `json:"game_image_sha256"`
		}
		if err := json.Unmarshal(raw, &man); err != nil {
			continue
		}
		sha := strings.ToLower(strings.TrimSpace(man.GameImageSHA256))
		if sha == "" {
			continue
		}
		exp.manifests++
		if !seen[sha] {
			seen[sha] = true
			exp.shas = append(exp.shas, sha)
		}
	}
	return exp
}

// hashPrefix returns the printable prefix of a hex SHA-256.
func hashPrefix(sha string) string {
	if len(sha) <= gameImageHashPrefixLen {
		return sha
	}
	return sha[:gameImageHashPrefixLen]
}

// verifyText renders the "how to verify it" clause: the SHA-256 prefix the
// image must start with (read from the manifests at run time, present only
// when the manifests are), never the image path.
func (e imageExpectation) verifyText() string {
	if len(e.shas) == 0 {
		return "verify the image against the game_image_sha256 field of the fixture manifests under $GAME_NARRATOR_FIXTURE_DIR (none present here)"
	}
	prefixes := make([]string, len(e.shas))
	for i, s := range e.shas {
		prefixes[i] = hashPrefix(s)
	}
	noun := "fixture manifest"
	if e.manifests != 1 {
		noun += "s"
	}
	return fmt.Sprintf("verify the image: its SHA-256 must start with %s (read at run time from %d %s under $GAME_NARRATOR_FIXTURE_DIR)",
		strings.Join(prefixes, " or "), e.manifests, noun)
}

// sha256File hashes a whole file in 1 MiB chunks, checking the check's
// context before opening and between chunks so the engine's deadline is
// honoured even on a slow disk.
func sha256File(ctx context.Context, path string) (string, error) {
	if cerr := ctx.Err(); cerr != nil {
		return "", cerr
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, err := h.Write(buf[:n]); err != nil {
				return "", err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
		if cerr := ctx.Err(); cerr != nil {
			return "", cerr
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
