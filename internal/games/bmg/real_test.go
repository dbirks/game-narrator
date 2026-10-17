package bmg

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/gcdisc"
)

// TestRealBMG parses the actual BMG from the user-supplied game image.
// It logs structural statistics only (magics, sizes, counts, numeric
// tag-pair histogram) — never message text, offsets hex, or raw bytes —
// and asserts the losslessness property for every message.
func TestRealBMG(t *testing.T) {
	path := os.Getenv("GAME_NARRATOR_GAME_IMAGE_PATH")
	if path == "" {
		t.Skip("NOT RUN: GAME_NARRATOR_GAME_IMAGE_PATH not set")
	}
	d, err := gcdisc.Open(path)
	if err != nil {
		t.Fatalf("open disc: %v", err)
	}
	defer d.Close()
	dfs, err := d.FS()
	if err != nil {
		t.Fatalf("disc FS: %v", err)
	}
	raw, err := fs.ReadFile(dfs, "res/Msg/bmgres.arc")
	if err != nil {
		t.Fatalf("read bmgres.arc: %v", err)
	}
	arc, err := gcdisc.OpenRARC(raw)
	if err != nil {
		t.Fatalf("OpenRARC(bmgres.arc): %v", err)
	}
	var bmgPath string
	err = fs.WalkDir(arc, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".bmg") && bmgPath == "" {
			bmgPath = p
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk RARC: %v", err)
	}
	if bmgPath == "" {
		t.Fatal("no .bmg entry found in bmgres.arc")
	}
	data, err := fs.ReadFile(arc, bmgPath)
	if err != nil {
		t.Fatalf("read %s: %v", bmgPath, err)
	}

	file, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Losslessness: every message must reconstruct exactly.
	for _, m := range file.Messages {
		if !bytes.Equal(m.Reconstruct(), m.Raw) {
			t.Fatalf("Reconstruct() != Raw for message %d (index-based)", m.Index)
		}
	}

	// Entry size from the INF1 body (u16 at body offset 2).
	var entrySize int
	if s := findSection(file.Sections, "INF1"); s != nil {
		entrySize = int(beU16(s.Raw[10:12]))
	}

	// Stats only: encoding byte, sections, message/tag counts.
	secs := make([]string, len(file.Sections))
	for i, s := range file.Sections {
		secs[i] = fmt.Sprintf("%s:%d", s.Magic, s.Size)
	}
	t.Logf("encoding=%d", file.Encoding)
	t.Logf("sections: %s", strings.Join(secs, " "))
	t.Logf("messages=%d entry_size=%d", len(file.Messages), entrySize)

	hist := map[[2]int]int{}
	var taggedMsgs, tagCount, invalidCount int
	for _, m := range file.Messages {
		hasTag := false
		for _, tk := range m.Tokens {
			switch tk.Kind {
			case TokTag:
				tagCount++
				hasTag = true
				hist[[2]int{tk.Group, tk.Type}]++
			case TokInvalid:
				invalidCount++
			}
		}
		if hasTag {
			taggedMsgs++
		}
	}
	type pair struct {
		g, c, n int
	}
	ranked := make([]pair, 0, len(hist))
	for k, n := range hist {
		ranked = append(ranked, pair{k[0], k[1], n})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].n != ranked[j].n {
			return ranked[i].n > ranked[j].n
		}
		if ranked[i].g != ranked[j].g {
			return ranked[i].g < ranked[j].g
		}
		return ranked[i].c < ranked[j].c
	})
	if len(ranked) > 10 {
		ranked = ranked[:10]
	}
	top := make([]string, len(ranked))
	for i, p := range ranked {
		top[i] = fmt.Sprintf("%d:%d=%d", p.g, p.c, p.n)
	}
	t.Logf("tagged_messages=%d total_tags=%d invalid_tokens=%d", taggedMsgs, tagCount, invalidCount)
	t.Logf("tag_pairs_top10: %s", strings.Join(top, " "))
}
