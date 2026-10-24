package pipeline

// ADR-0172 (issue #53): read a dialogue box wherever it is on screen. The
// profile's fixed regions (and the ADR-0137 top/middle/bottom bands) cover the
// placements a game is calibrated for; a box at any other height — top-of-
// screen boxes in other games, a panel shifted by a resolution change — is
// never read. This file is the frame-level panel finder that closes the gap:
// a cheap row-wise scan of a DOWNSAMPLED luma frame for horizontal bands that
// look like a dialogue panel. It is a pure function (no frame access, no
// runner state) so the scan rule is testable on synthetic frames and
// benchable on the host that ships it, and its candidates feed the existing
// presence → OCR → tracker → matcher path as one dynamic region
// (dispatch.go's panelFindTick) — the finder itself never touches OCR, the
// reducer or a model call.
//
// Cost discipline (the UNO Q is CPU-bound at ~7.5 fps): the scan runs on a
// small fixed-size luma buffer (default 160×120 = 19 200 bytes), one pass
// per row plus one pass over the winning band's pixels — microsecond-scale —
// and dispatch.go schedules it only when no configured region currently
// reports a present box (and at most every N frames), so the fixed-region
// path is bit-for-bit unchanged when it works.
//
// The rule is generic: nothing in the scan references a game's coordinates.
// A panel row is a row in which one polarity (pixels well below DarkLuma
// or well above BrightLuma — the dialogue-panel tone, either dark or
// bright) holds at least MinToneShare of the row's pixels — a share, not
// a contiguous run, because a translucent panel is broken into short runs
// by the content behind it while its share stays high. A panel is a run of
// such rows in one polarity, within the height bounds, whose band pixels
// carry TEXT-LIKE edge density: enough local
// contrast for letters, not so much that it is a busy scene. The density
// gate is what rejects the smooth vertical gradient (the sea): its rows are
// uniformly panel-toned but its pixels have no edges. All thresholds are
// PanelFindConfig with defaults; a profile disables the whole thing with
// Enabled=false.

import "image"

// PanelRect is one dialogue-panel candidate in unit-frame coordinates (the
// [0,1] rect spelling the profile regions use, vision.Rect). Score is the
// panel-ness the scan ranked on: the band's plain panel rows plus its height
// plus a small letters bonus, so a tall box outranks a short strip, a
// window and a letterbox of the same size.
type PanelRect struct {
	X, Y, W, H float64
	Score      float64
}

// PanelFindConfig parameters the finder. Zero fields normalize to
// DefaultPanelFind; Enabled stays off unless the caller sets it — the finder
// is an opt-in runtime feature (ADR-0172), and a profile that does not want
// it pays exactly zero per-frame cost.
type PanelFindConfig struct {
	// Enabled turns the finder on. Off by default: the fixed-region path is
	// the primary mechanism and the finder only pays its (microsecond) cost
	// in frames where that path sees no box.
	Enabled bool
	// EveryNFrames is the scan cadence when the finder runs at all: at most
	// one scan per N captured frames (4 at the UNO Q's ~7.5 fps is ~500 ms —
	// a dialogue box stays on screen for seconds, so a missed frame costs
	// nothing).
	EveryNFrames int
	// SampleW/SampleH are the downsampled luma scan size (160×120 default:
	// small enough that the whole scan fits in the data path, large enough
	// that a real panel still spans a few dozen rows).
	SampleW int
	SampleH int
	// DarkLuma is the luma a pixel must be BELOW to be panel-toned dark;
	// BrightLuma the luma it must be ABOVE to be panel-toned bright (a
	// game's bright panels); zero means the knob is unset and normalizes to
	// the default, so a profile that wants "no bright panels" sets it to 1,
	// never a panel tone. Pixels between are scene, whatever the scene is.
	DarkLuma   uint8
	BrightLuma uint8
	// MinToneShare is the minimum share of a row's pixels that must be in ONE
	// panel tone (below DarkLuma for dark, above BrightLuma for bright) for
	// the row to count as a panel row in that polarity; the tone must
	// dominate the row (more than the opposite tone's share), so a busy row
	// whose dark and bright pixels merely balance out stays scene. Share,
	// not a contiguous run: a dialogue panel is often translucent, so the
	// scene and its characters show through and BREAK the panel tone into
	// short runs. 0.40 rather than 0.50 because a text-filled panel gives
	// up part of its tone to the letters riding it.
	MinToneShare float64
	// MinHeight/MaxHeight bound the panel run's height as shares of the
	// frame height (a box is a band, not the whole frame and not a hairline).
	MinHeight float64
	MaxHeight float64
	// EdgeDelta is the luma step between a pixel and its left neighbour
	// that counts as an edge; MinEdgeShare/MaxEdgeShare bound the band's
	// edge density (its pixels' edge share) — the text-like middle: a
	// smooth gradient (the sea) is below Min, a noisy scene above Max.
	EdgeDelta    uint8
	MinEdgeShare float64
	MaxEdgeShare float64
	// MaxRowGap is the most neutral (neither panel-tone) rows a band may
	// span at a time while still one panel: the letters' rows. A line of
	// text rides the panel in the opposite tone with anti-aliased, scene-
	// luma fringes, so the row gate sees the panel's tone INTERRUPTED
	// exactly where the letters are — a full-line box reads as two
	// half-bands and the upper one (panel only, no letters) fails
	// MinHeight while the lower fails the edge gate. Bounded so two
	// genuinely separate bands (two boxes, a box and a sky) never merge
	// across a scene gap. A strict contiguous rule is an arbitrarily
	// small positive value; zero is "unset" and normalizes to the default.
	MaxRowGap float64
	// MinTextRuns is the count of isolated short runs of the OPPOSITE tone
	// (at most MaxTextRunLen pixels long, letters riding the panel) that a
	// row needs to count as a letter row; MaxTextRunLen bounds the run
	// length. Letter rows and plain rows together feed the candidate's
	// Score — the signal that separates a dialogue box (both) from a
	// window (all letter rows) and a letterbox (no letter rows).
	MinTextRuns   int
	MaxTextRunLen int
	// MaxCandidates is the maximum number of candidate rects the scan
	// returns (3: two boxes and a false positive is a lot for one frame).
	MaxCandidates int
}

// DefaultPanelFind is the panel detector described above. The numbers are
// the scan rule's operating point, measured on synthetic frames
// (panelfind_test.go) and the private Dolphin frames the probe test
// (panelfind_probe_test.go) exercises when the operator opts in.
func DefaultPanelFind() PanelFindConfig {
	return PanelFindConfig{
		Enabled:       false,
		EveryNFrames:  4,
		SampleW:       160,
		SampleH:       120,
		DarkLuma:      90,
		BrightLuma:    180,
		MinToneShare:  0.40,
		MinHeight:     0.04,
		MaxHeight:     0.45,
		EdgeDelta:     60,
		MinEdgeShare:  0.005,
		MaxEdgeShare:  0.50,
		MaxRowGap:     0.06,
		MinTextRuns:   2,
		MaxTextRunLen: 12,
		MaxCandidates: 3,
	}
}

// normalize fills unset (zero/negative) knobs with the defaults; explicit
// values are kept. Enabled is copied through as-is: a zero config is a
// DISABLED finder, not a default one.
func (c PanelFindConfig) normalize() PanelFindConfig {
	d := DefaultPanelFind()
	if c.EveryNFrames <= 0 {
		c.EveryNFrames = d.EveryNFrames
	}
	if c.SampleW <= 0 {
		c.SampleW = d.SampleW
	}
	if c.SampleH <= 0 {
		c.SampleH = d.SampleH
	}
	if c.DarkLuma <= 0 {
		c.DarkLuma = d.DarkLuma
	}
	if c.BrightLuma <= 1 {
		c.BrightLuma = d.BrightLuma
	}
	if c.MinToneShare <= 0 {
		c.MinToneShare = d.MinToneShare
	}
	if c.MinHeight <= 0 {
		c.MinHeight = d.MinHeight
	}
	if c.MaxHeight <= 0 || c.MaxHeight <= c.MinHeight {
		c.MaxHeight = d.MaxHeight
	}
	if c.EdgeDelta == 0 {
		c.EdgeDelta = d.EdgeDelta
	}
	if c.MinEdgeShare <= 0 {
		c.MinEdgeShare = d.MinEdgeShare
	}
	if c.MaxEdgeShare <= 0 || c.MaxEdgeShare < c.MinEdgeShare {
		c.MaxEdgeShare = d.MaxEdgeShare
	}
	if c.MaxRowGap <= 0 {
		c.MaxRowGap = d.MaxRowGap
	}
	if c.MinTextRuns <= 0 {
		c.MinTextRuns = d.MinTextRuns
	}
	if c.MaxTextRunLen <= 0 {
		c.MaxTextRunLen = d.MaxTextRunLen
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = d.MaxCandidates
	}
	return c
}

// rowTone is a panel row's polarity (the tone its panel run is mostly).
type rowTone int8

const (
	rowNone   rowTone = 0
	rowDark   rowTone = 1
	rowBright rowTone = -1
)

// panelRow is one scan row's verdict: its panel tone (none when the row is
// scene), the dominant tone's left/right extent in scan pixels, and the
// count of isolated short runs of the OPPOSITE tone riding the panel — the
// row's letters.
type panelRow struct {
	tone  rowTone
	left  int
	right int
	text  int
}

// FindPanels scans one downsampled luma frame (row-major, w×h, h rows of w
// bytes) for dialogue-panel bands and returns the candidates, best score
// first. It is pure: no image access, no config mutation — the whole scan
// rule isolated from the runner so the tests drive it on synthetic luma and
// the bench measures its true cost.
func FindPanels(luma []byte, w, h int, cfg PanelFindConfig) []PanelRect {
	if len(luma) < w*h || w <= 0 || h <= 0 {
		return nil
	}
	c := cfg.normalize()
	minTone := int(c.MinToneShare * float64(w))
	if minTone < 1 {
		minTone = 1
	}

	// Pass 1: measure every row's panel-tone SHARES. A row is a panel row
	// when one polarity holds at least MinToneShare of the row — SHARE, not
	// a contiguous run: a dialogue panel is often translucent, and the
	// content behind it (characters, the scene itself on dark frames)
	// breaks the tone into short runs while the share stays high. A row
	// whose dark and bright pixels merely balance out (a busy scene) is
	// below MinToneShare in each polarity and stays scene. The row's
	// left/right are the dominant tone's extreme pixels — the panel's true
	// edges, not a longest-run artifact. The row's letters are counted too
	// (isolated short runs of the opposite tone riding the panel): the
	// band's text gate below is what separates a box from a letterbox, a
	// dark scene strip and the sea — none of them carries text.
	rows := make([]panelRow, h)
	for y := 0; y < h; y++ {
		row := luma[y*w : (y+1)*w]
		dark, bright := 0, 0
		darkLeft, darkRight := -1, -1
		brightLeft, brightRight := -1, -1
		shortDark, shortBright := 0, 0
		sd, sb := 0, 0 // active dark/bright run lengths (0 = no run)
		for x, l := range row {
			isDark := l < c.DarkLuma
			isBright := l > c.BrightLuma
			switch {
			case isDark && sd > 0:
				sd++
			case isDark:
				if sb > 0 && sb <= c.MaxTextRunLen {
					shortBright++
				}
				sd, sb = 1, 0
			case isBright && sb > 0:
				sb++
			case isBright:
				if sd > 0 && sd <= c.MaxTextRunLen {
					shortDark++
				}
				sd, sb = 0, 1
			default:
				if sd > 0 && sd <= c.MaxTextRunLen {
					shortDark++
				}
				if sb > 0 && sb <= c.MaxTextRunLen {
					shortBright++
				}
				sd, sb = 0, 0
			}
			switch {
			case isDark:
				dark++
				if darkLeft < 0 {
					darkLeft = x
				}
				darkRight = x
			case isBright:
				bright++
				if brightLeft < 0 {
					brightLeft = x
				}
				brightRight = x
			}
		}
		if sd > 0 && sd <= c.MaxTextRunLen {
			shortDark++
		}
		if sb > 0 && sb <= c.MaxTextRunLen {
			shortBright++
		}
		switch {
		case dark >= minTone && dark > bright && darkLeft >= 0:
			rows[y] = panelRow{tone: rowDark, left: darkLeft, right: darkRight, text: shortBright}
		case bright >= minTone && bright > dark && brightLeft >= 0:
			rows[y] = panelRow{tone: rowBright, left: brightLeft, right: brightRight, text: shortDark}
		}
	}

	// Pass 2: collect runs of same-tone panel rows within the height
	// bounds, and score each. A run may span neutral rows (up to MaxRowGap
	// at a time): the letters' rows are where the panel's tone is
	// interrupted, and a full-line box without them is the exact frame a
	// finder must catch.
	minRows := int(float64(h) * c.MinHeight)
	maxRows := int(float64(h) * c.MaxHeight)
	if minRows < 1 {
		minRows = 1
	}
	maxGap := int(float64(h) * c.MaxRowGap)
	out := []PanelRect{}
	y := 0
	for y < h {
		if rows[y].tone == rowNone {
			y++
			continue
		}
		tone := rows[y].tone
		start := y
		gap := 0
	runEnd:
		for y < h {
			switch rows[y].tone {
			case tone:
				gap = 0
				y++
			case rowNone:
				if gap < maxGap {
					gap++
					y++
				} else {
					break runEnd
				}
			default: // the other polarity: a second band, not a gap.
				break runEnd
			}
		}
		end := y
		// Trim trailing neutral rows the run ended on: they are scene, not
		// panel (the gap that followed the last tone row was not a line of
		// letters, it was the rest of the picture).
		for end > start && rows[end-1].tone == rowNone {
			end--
		}
		// The run's height counts its PANEL rows; the letters' neutral rows
		// ride inside the band without inflating it (a one-line box is not
		// 20% taller for carrying the line).
		run := 0
		for yy := start; yy < end; yy++ {
			if rows[yy].tone == tone {
				run++
			}
		}
		if run < minRows {
			continue
		}
		if run > maxRows {
			// The bridged run is too tall: the gap that joined it was
			// scene, not a line of letters — two same-tone bands sharing
			// a border strip (a dark cabin wall above, the box below).
			// Split at the internal gaps and consider each contiguous
			// sub-band that fits the height bounds on its own.
			yy := start
			for yy < end {
				if rows[yy].tone != tone {
					yy++
					continue
				}
				s := yy
				e := yy
				for e < end && rows[e].tone == tone {
					e++
				}
				if e-s >= minRows && e-s <= maxRows {
					if p := panelCandidate(rows, s, e, luma, w, h, c); p.H > 0 {
						out = append(out, p)
					}
				}
				yy = e
			}
			continue
		}
		if p := panelCandidate(rows, start, end, luma, w, h, c); p.H > 0 {
			out = append(out, p)
		}
	}
	// Rank by panel-ness, best first (a wide band with letters beats a
	// narrower band): the caller's choice is the first element. Trim only
	// after the rank, so the kept set is the best, not the topmost.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Score > out[j-1].Score; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	if len(out) > c.MaxCandidates {
		out = out[:c.MaxCandidates]
	}
	return out
}

// panelCandidate measures one tone run's rows [start,end) into a candidate:
// the run's horizontal extent (the band's panel edges, trimmed of outlier
// rows) must span MinToneShare of the frame, and the band's pixels must
// carry text-like edge density — enough for letters, not so much for a busy
// scene. A failing gate returns a zero rect.
func panelCandidate(rows []panelRow, start, end int, luma []byte, w, h int, c PanelFindConfig) PanelRect {
	lefts, rights := []int{}, []int{}
	for y := start; y < end; y++ {
		// Only the panel rows contribute to the band's extent: the
		// letters' neutral rows carry no measured panel edge, and their
		// zero values would drag the percentile toward the frame corner.
		if rows[y].tone == rowNone {
			continue
		}
		lefts = append(lefts, rows[y].left)
		rights = append(rights, rows[y].right)
	}
	if len(lefts) == 0 {
		return PanelRect{}
	}
	left := percentile(lefts, len(lefts)/4)      // 25th pctl of the left edge
	right := percentile(rights, 3*len(rights)/4) // 75th pctl of the right edge
	if right < left {
		return PanelRect{}
	}
	widthShare := float64(right-left+1) / float64(w)
	if widthShare < c.MinToneShare {
		return PanelRect{}
	}

	// Edge density over the band's extent: the letter gate. A gradient
	// (sea) has no horizontal edges and fails MinEdgeShare; a noisy scene
	// fails MaxEdgeShare — its rows were never wide-run enough to start a
	// run, but a busy band inside a uniformly toned region is the shape
	// this gate exists for.
	rowsN := end - start
	edges, total := 0, 0
	for y := start; y < end; y++ {
		row := luma[y*w : (y+1)*w]
		for x := left + 1; x <= right; x++ {
			total++
			if d := int(row[x]) - int(row[x-1]); d >= int(c.EdgeDelta) || d <= -int(c.EdgeDelta) {
				edges++
			}
		}
	}
	if total == 0 {
		return PanelRect{}
	}
	density := float64(edges) / float64(total)
	if density < c.MinEdgeShare || density > c.MaxEdgeShare {
		return PanelRect{}
	}

	// Ranking signal. A dialogue box is a TALL band made of PLAIN panel
	// rows (the fill) plus a line or two of text. That is what the score
	// rewards, in order: plainRows (the box carries dozens of fill rows;
	// a window is all mullions so plain=0, a letterbox a handful, a scene
	// strip a few), then height (a box is a tall band; a strip is short),
	// then a small bonus for having SOME letters (separates a live box
	// from a letterbox of the same size). A box whose text is too thin to
	// register on a given frame still keeps its plain + height — it does
	// not fall to the strip just because one line of dialogue is faint.
	textRows, plainRows := 0, 0
	for y := start; y < end; y++ {
		if rows[y].tone == rowNone {
			continue
		}
		if rows[y].text >= c.MinTextRuns {
			textRows++
		} else {
			plainRows++
		}
	}
	bandH := float64(rowsN) / float64(h)
	textBonus := 0.0
	if textRows > 0 {
		textBonus = 0.5
	}
	return PanelRect{
		X:     float64(left) / float64(w),
		Y:     float64(start) / float64(h),
		W:     widthShare,
		H:     bandH,
		Score: float64(plainRows) + bandH + textBonus + density,
	}
}

// percentile selects the k-th smallest value (0-indexed) by repeated
// min-extraction on a copy: panel runs hold at most a few dozen rows, so the
// O(k²) is microseconds — and the scan stays allocation-light on the way
// that matters.
func percentile(a []int, k int) int {
	if k < 0 {
		k = 0
	}
	if k >= len(a) {
		k = len(a) - 1
	}
	v := make([]int, len(a))
	copy(v, a)
	for i := 0; i < k; i++ {
		mi := 0
		for j := 1; j < len(v); j++ {
			if v[j] < v[mi] {
				mi = j
			}
		}
		v[mi] = v[len(v)-1]
		v = v[:len(v)-1]
	}
	return v[0]
}

// FindPanelsImage is the image-facing wrapper: downsample the frame to the
// config's scan size and run the pure FindPanels. It is the entry harnesses
// and probe tests use; the pipeline's own hot path calls panelLuma and
// FindPanels through the same two steps, so a rect found this way is the
// rect the dynamic region crops.
func FindPanelsImage(img image.Image, cfg PanelFindConfig) []PanelRect {
	c := cfg.normalize()
	return FindPanels(panelLuma(img, c.SampleW, c.SampleH), c.SampleW, c.SampleH, c)
}

// panelLuma downsamples an arbitrary frame image to w×h luma bytes
// (row-major, 0–255) by box-averaging its samples over each destination
// pixel's source cell. It is the finder's only frame touch — the rest of
// the scan is the pure FindPanels on these bytes — and it runs at most
// every EveryNFrames on frames with no configured box, which is the cost
// budget ADR-0172 accounts for. The common frame types are walked through
// their pixel arrays directly; anything else falls back to the (rare,
// harness-odd) At() path.
func panelLuma(img image.Image, w, h int) []byte {
	out := make([]byte, w*h)
	switch src := img.(type) {
	case *image.RGBA:
		return panelLumaRGBA(src.Pix, src.Stride, 4, src.Bounds(), out, w, h)
	case *image.NRGBA:
		return panelLumaRGBA(src.Pix, src.Stride, 4, src.Bounds(), out, w, h)
	case *image.RGBA64:
		pix := make([]uint8, len(src.Pix)/2)
		for i, j := 0, 0; i < len(src.Pix); i, j = i+2, j+2 {
			pix[j] = uint8(src.Pix[i])
		}
		return panelLumaRGBA(pix, src.Stride/2, 4, src.Bounds(), out, w, h)
	case *image.Gray:
		pix := src.Pix
		return panelLumaLuma(pix, src.Stride, src.Bounds(), out, w, h)
	default:
		return panelLumaAt(img, out, w, h)
	}
}

// panelLumaRGBA is the fast walk over a tightly packed 8-bit RGB(A) buffer.
// bpp is the bytes-per-pixel (3 for RGB, 4 for RGBA/NRGBA); the alpha byte,
// when present, is simply never read.
func panelLumaRGBA(pix []uint8, stride, bpp int, b image.Rectangle, out []byte, w, h int) []byte {
	if b.Empty() {
		return out
	}
	cw := float64(b.Dx()) / float64(w)
	ch := float64(b.Dy()) / float64(h)
	if cw <= 0 {
		cw = 1
	}
	if ch <= 0 {
		ch = 1
	}
	for dy := 0; dy < h; dy++ {
		y0 := b.Min.Y + int(float64(dy)*ch)
		y1 := y0 + int(ch)
		if y1 > b.Max.Y {
			y1 = b.Max.Y
		}
		for dx := 0; dx < w; dx++ {
			x0 := b.Min.X + int(float64(dx)*cw)
			x1 := x0 + int(cw)
			if x1 > b.Max.X {
				x1 = b.Max.X
			}
			sum, n := 0, 0
			for y := y0; y < y1; y++ {
				row := pix[y*stride+b.Min.X*bpp : y*stride+(b.Min.X+b.Dx())*bpp]
				for x := x0; x < x1; x++ {
					o := (x - b.Min.X) * bpp
					r8 := int(row[o])
					g8 := int(row[o+1])
					b8 := int(row[o+2])
					sum += (299*r8 + 587*g8 + 114*b8) / 1000
					n++
				}
			}
			if n == 0 {
				continue
			}
			l := sum / n
			if l > 255 {
				l = 255
			}
			out[dy*w+dx] = byte(l)
		}
	}
	return out
}

// panelLumaLuma is the fast walk over a 1-plane luma buffer.
func panelLumaLuma(pix []uint8, stride int, b image.Rectangle, out []byte, w, h int) []byte {
	if b.Empty() {
		return out
	}
	cw := float64(b.Dx()) / float64(w)
	ch := float64(b.Dy()) / float64(h)
	if cw <= 0 {
		cw = 1
	}
	if ch <= 0 {
		ch = 1
	}
	for dy := 0; dy < h; dy++ {
		y0 := b.Min.Y + int(float64(dy)*ch)
		y1 := y0 + int(ch)
		if y1 > b.Max.Y {
			y1 = b.Max.Y
		}
		for dx := 0; dx < w; dx++ {
			x0 := b.Min.X + int(float64(dx)*cw)
			x1 := x0 + int(cw)
			if x1 > b.Max.X {
				x1 = b.Max.X
			}
			sum, n := 0, 0
			for y := y0; y < y1; y++ {
				row := pix[y*stride+b.Min.X : y*stride+b.Min.X+b.Dx()]
				for x := x0; x < x1; x++ {
					sum += int(row[x])
					n++
				}
			}
			if n == 0 {
				continue
			}
			out[dy*w+dx] = byte(sum / n)
		}
	}
	return out
}

// panelLumaAt is the general (slow) path: one At() per source pixel. Only a
// harness oddity reaches it — the capture and replay paths hand the pipeline
// a *image.RGBA.
func panelLumaAt(img image.Image, out []byte, w, h int) []byte {
	b := img.Bounds()
	if b.Empty() {
		return out
	}
	cw := float64(b.Dx()) / float64(w)
	ch := float64(b.Dy()) / float64(h)
	if cw <= 0 {
		cw = 1
	}
	if ch <= 0 {
		ch = 1
	}
	for dy := 0; dy < h; dy++ {
		y0 := b.Min.Y + int(float64(dy)*ch)
		y1 := y0 + int(ch)
		if y1 > b.Max.Y {
			y1 = b.Max.Y
		}
		for dx := 0; dx < w; dx++ {
			x0 := b.Min.X + int(float64(dx)*cw)
			x1 := x0 + int(cw)
			if x1 > b.Max.X {
				x1 = b.Max.X
			}
			sum, n := 0, 0
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					r, g, bb, _ := img.At(x, y).RGBA()
					// 16-bit RGBA → luma (BT.601), back to 8 bits.
					r8 := int(r >> 8)
					g8 := int(g >> 8)
					b8 := int(bb >> 8)
					sum += (299*r8 + 587*g8 + 114*b8) / 1000
					n++
				}
			}
			if n == 0 {
				continue
			}
			l := sum / n
			if l > 255 {
				l = 255
			}
			out[dy*w+dx] = byte(l)
		}
	}
	return out
}
