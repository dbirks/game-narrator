package dolphin

// arrow.go is the olive talk-arrow's pixel-only detection (ADR-0166's
// approach step): the measured color predicate, the densest-window stats and
// the qualifying-cluster list over one frame. No vision model — the arrow's
// own pixels are the signal, so the approach step that consumes it is
// deterministic and synthetic-frame-testable.

import (
	"image"
	"math"
	"sort"
)

// arrowMinPx is the talk-arrow's densest-window pixel floor (640x480):
// the arrow measures ~400 dense olive px in the 32 px window on the
// restored-deck evidence (the 24 px proximity clustering measured 445 px
// in a 33x25 bbox at (0.60, 0.34)), while the only other olive object on
// the deck — the bottom-right "A SPEAK" prompt UI (115 px at (0.86, 0.90))
// — is excluded by the arrow's cy <= 0.66 clause. Scene greens and the
// deck's pure-yellow sunlit patches (measured rgb 250,235,16: r8 >= 225)
// do not clear the olive predicate, so a qualifying window is the arrow.
const arrowMinPx = 150

// isOliveArrowPixel is the talk arrow's measured color predicate. The
// restored-deck evidence (artifacts/runs/dolphin-savestate-2026-10-20-
// 085709/restored-deck.png) measured the arrow at (0.60, 0.34) as a 445 px
// cluster, 33x25 bbox, densest rgb (162-166, 177-179, 81-84): an
// OLIVE-GOLD fill with green at or above red — not the saturated green the
// first drive calibrated on (that predicate found no qualifying window on
// the deck, which is why the first runs never saw the arrow). The clauses:
// g >= r-10 (g at or above r), g >= 140 (the arrow is bright), g-b >= 50
// (the yellow family — b far below g), 130 <= r < 225 (the deck's
// pure-yellow sunlit patches measure rgb 250,235,16 — r >= 225 keeps them
// out; scene greens and the sea are darker or less yellow). Calibrated by
// densest-blob clustering over the measured frame: the arrow is the only
// qualifying object above the count floor; its sole competitor is the
// bottom-right prompt UI (115 px at (0.86, 0.90)), which the window's
// cy <= 0.66 clause excludes.
func isOliveArrowPixel(r8, g8, b8 int) bool {
	return g8 >= r8-10 && g8 >= 140 && g8-b8 >= 50 && r8 >= 130 && r8 < 225
}

// arrowStats is one frame's olive talk-arrow measurement (the arrow is the
// only qualifying olive object on the deck scene; the sunlit patches and
// the sea do not clear the predicate).
type arrowStats struct {
	count int     // the densest 32 px window's olive pixels
	cx    float64 // centroid (frame fraction, 0..1)
	cy    float64
	ok    bool // the densest window met the arrow's density/compactness/fill/upper-mid tests
}

// oliveArrowStats finds the interaction arrow: the densest 32 px window of
// olive (isOliveArrowPixel — the arrow is a solid fill; the deck's sunlit
// yellow is r >= 225 and the scene greens are not the yellow family, so no
// window of them reaches the arrow's density). A candidate window is the
// arrow when it is dense (>=150 px), compact (the window's olive bounding
// box <=40 px on a side), solid (the count fills at least 30% of its
// bounding box — the arrow is a fill, scattered sunlit pixels are not),
// and upper-mid (the arrow floats over Aryll's head at cy ~0.34 — the only
// other qualifying blob on the deck, the bottom-right "A SPEAK" prompt UI
// at (0.86, 0.90), sits below the cy <= 0.66 clause). Calibrated on the
// captured restored-deck evidence (the arrow is ~400 dense olive px at
// (0.60, 0.34); the deck itself clears no qualifying window).
func oliveArrowStats(img image.Image) arrowStats {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	win, step := 32, 8
	var best arrowStats
	for wy := 0; wy+win <= h; wy += step {
		for wx := 0; wx+win <= w; wx += step {
			n, sx, sy := 0, 0, 0
			minx, miny, maxx, maxy := 1<<30, 1<<30, -1, -1
			for y := wy; y < wy+win; y++ {
				for x := wx; x < wx+win; x++ {
					r, g, bl, _ := img.At(x, y).RGBA()
					g8, r8, b8 := int(g>>8), int(r>>8), int(bl>>8)
					if isOliveArrowPixel(r8, g8, b8) {
						n++
						sx += x
						sy += y
						if x < minx {
							minx = x
						}
						if x > maxx {
							maxx = x
						}
						if y < miny {
							miny = y
						}
						if y > maxy {
							maxy = y
						}
					}
				}
			}
			if n <= 0 {
				continue
			}
			bw, bh := maxx-minx+1, maxy-miny+1
			cy := float64(sy) / float64(n) / float64(h)
			if n >= arrowMinPx && bw <= 40 && bh <= 40 && float64(n) >= 0.3*float64(bw*bh) && cy <= 0.66 {
				c := arrowStats{count: n, cx: float64(sx) / float64(n) / float64(w), cy: cy, ok: true}
				if c.count > best.count {
					best = c
				}
			}
		}
	}
	return best
}

// oliveArrowClusters returns every qualifying cluster (the same criteria
// as oliveArrowStats) deduplicated by proximity (windows overlapping the
// same blob are one cluster), densest first. The drive tracks one cluster
// across samples (continuity): the deck can carry more than one qualifying
// blob (the arrow plus a sunlit prop), and the densest per frame would flip
// between them while the arrow itself is the steady one.
func oliveArrowClusters(img image.Image) []arrowStats {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	win, step := 32, 8
	type cand struct {
		n, sx, sy              int
		minx, miny, maxx, maxy int
	}
	var cands []cand
	for wy := 0; wy+win <= h; wy += step {
		for wx := 0; wx+win <= w; wx += step {
			c := cand{minx: 1 << 30, miny: 1 << 30, maxx: -1, maxy: -1}
			for y := wy; y < wy+win; y++ {
				for x := wx; x < wx+win; x++ {
					r, g, bl, _ := img.At(x, y).RGBA()
					g8, r8, b8 := int(g>>8), int(r>>8), int(bl>>8)
					if isOliveArrowPixel(r8, g8, b8) {
						c.n++
						c.sx += x
						c.sy += y
						if x < c.minx {
							c.minx = x
						}
						if x > c.maxx {
							c.maxx = x
						}
						if y < c.miny {
							c.miny = y
						}
						if y > c.maxy {
							c.maxy = y
						}
					}
				}
			}
			if c.n <= 0 {
				continue
			}
			bw, bh := c.maxx-c.minx+1, c.maxy-c.miny+1
			cy := float64(c.sy) / float64(c.n) / float64(h)
			if c.n >= arrowMinPx && bw <= 40 && bh <= 40 && float64(c.n) >= 0.3*float64(bw*bh) && cy <= 0.66 {
				cands = append(cands, c)
			}
		}
	}
	// Deduplicate: a window overlapping an already-kept cluster's centre is
	// the same blob.
	var out []arrowStats
	for _, c := range cands {
		cx, cy := float64(c.sx)/float64(c.n)/float64(w), float64(c.sy)/float64(c.n)/float64(h)
		dupe := false
		for _, k := range out {
			if math.Abs(k.cx-cx)+math.Abs(k.cy-cy) < 0.08 {
				dupe = true
				break
			}
		}
		if !dupe {
			out = append(out, arrowStats{count: c.n, cx: cx, cy: cy, ok: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}
