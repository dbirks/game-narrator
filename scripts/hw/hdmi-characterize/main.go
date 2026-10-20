// Command hdmi-characterize measures, per frame, the properties a vision
// pipeline must survive on a real GameCube HDMI feed: the active-picture box
// (black-border detection), sharpness (Laplacian variance), luma range/levels
// (full vs studio range), interlace combing, and dim (fade) frames. It prints
// a per-frame CSV and a per-set summary, so an HDMI capture set can be
// compared against the Dolphin headless fixture frames used by the bench.
//
// Stdlib-only and deterministic: PNG decode + fixed-point luma, so the numbers
// are reproducible on the dev host and on the appliance. It reads images only;
// it never writes into a fixture directory (use -o to redirect output).
//
// Usage:
//
//	go run ./scripts/hw/hdmi-characterize \
//	  -dir <name>=<path> [-dir <name>=<path> ...] [-o out.csv]
//
// Example:
//
//	go run ./scripts/hw/hdmi-characterize \
//	  -dir hdmi-1080=$GAME_NARRATOR_FIXTURE_DIR/hdmi-v316b/1080-mjpeg/3fps \
//	  -dir hdmi-720=$GAME_NARRATOR_FIXTURE_DIR/hdmi-v316b/720-yuyv/3fps \
//	  -dir dolphin=artifacts/fixtures/windwaker-newgame/frames
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// barLuma and barCoverage mirror internal/vision (roi.go): a pixel is "bar"
// below luma 16 (the bottom of legal video range), and a scanned line is a bar
// when at least 98 % of its samples are bar.
const (
	barLuma     = 16
	barCoverage = 0.98
	scanStride  = 512 // samples per scanned line, same target as vision.ActiveArea
)

// Frame is one frame's measurements. Box coordinates are 1-based inclusive,
// matching how the manual and run notes quote them ("x 73..1830").
type Frame struct {
	Name     string
	W, H     int
	X0, Y0   int
	X1, Y1   int
	LapVar   float64
	LumaMin  int
	LumaMax  int
	LumaMean float64
	LumaStd  float64
	// SigLo/SigHi are the first/last histogram bin holding >= 1 % of the
	// pixels — the same discriminator internal/vision.normalizeLumaRange uses
	// to tell studio (16-235) content from full-range content.
	SigLo, SigHi       int
	FracBelow16        float64
	FracAbove235       float64
	FracAt0, FracAt255 float64
	Comb               float64
	Dim                bool
}

// RangeClass names the luma-range verdict for one frame, using the same
// 14/238 window internal/vision uses for its studio-range test.
func (f Frame) RangeClass() string {
	if f.SigLo < 14 || f.SigHi > 238 {
		return "full"
	}
	return "studio"
}

// set is one named directory of frames with its measurements.
type set struct {
	name   string
	paths  []string
	frames []Frame
}

func main() {
	var dirs []string
	out := flag.String("o", "", "write the per-frame CSV to this file (default: no CSV)")
	flag.Var(&dirList{v: &dirs}, "dir", "set name=path, e.g. hdmi-1080=/path/to/frames (repeatable)")
	flag.Parse()
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: hdmi-characterize -dir name=path [-dir name=path ...] [-o out.csv]")
		os.Exit(2)
	}

	var sets []set
	for _, d := range dirs {
		name, path, ok := strings.Cut(d, "=")
		if !ok || path == "" {
			fmt.Fprintf(os.Stderr, "bad -dir %q (want name=path)\n", d)
			os.Exit(2)
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading %s: %v\n", path, err)
			os.Exit(1)
		}
		var ps []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".png") {
				ps = append(ps, filepath.Join(path, e.Name()))
			}
		}
		sort.Strings(ps)
		s := set{name: name, paths: ps}
		for i, p := range ps {
			f, err := measure(p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", p, err)
				os.Exit(1)
			}
			if f.Name == "" {
				f.Name = fmt.Sprintf("f%04d", i+1)
			}
			s.frames = append(s.frames, f)
		}
		sets = append(sets, s)
	}

	if *out != "" {
		var b strings.Builder
		b.WriteString("set,frame,w,h,x0,y0,x1,y1,lap_var,luma_min,luma_max,luma_mean,luma_std,sig_lo,sig_hi,frac_below16,frac_above235,frac_at0,frac_at255,comb,range_class\n")
		for _, s := range sets {
			for _, f := range s.frames {
				fmt.Fprintf(&b, "%s,%s,%d,%d,%d,%d,%d,%d,%.1f,%d,%d,%.1f,%.1f,%d,%d,%.4f,%.4f,%.4f,%.4f,%.4f,%s\n",
					s.name, f.Name, f.W, f.H, f.X0, f.Y0, f.X1, f.Y1, f.LapVar,
					f.LumaMin, f.LumaMax, f.LumaMean, f.LumaStd, f.SigLo, f.SigHi,
					f.FracBelow16, f.FracAbove235, f.FracAt0, f.FracAt255, f.Comb, f.RangeClass())
			}
		}
		if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "writing %s: %v\n", *out, err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s\n", *out)
	}

	for _, s := range sets {
		printSummary(s)
	}
}

type dirList struct{ v *[]string }

func (d *dirList) String() string { return strings.Join(*d.v, " ") }
func (d *dirList) Set(s string) error {
	*d.v = append(*d.v, s)
	return nil
}

func printSummary(s set) {
	n := len(s.frames)
	if n == 0 {
		fmt.Printf("== %s: no frames ==\n", s.name)
		return
	}
	x0min, x0max := s.frames[0].X0, s.frames[0].X0
	y0min, y0max := s.frames[0].Y0, s.frames[0].Y0
	x1min, x1max := s.frames[0].X1, s.frames[0].X1
	y1min, y1max := s.frames[0].Y1, s.frames[0].Y1
	w, h := s.frames[0].W, s.frames[0].H
	laps := make([]float64, n)
	combMax, combSum := 0.0, 0.0
	lumaMin, lumaMax := 255, 0
	means := make([]float64, n)
	full, studio := 0, 0
	for i, f := range s.frames {
		laps[i] = f.LapVar
		combMax = math.Max(combMax, f.Comb)
		combSum += f.Comb
		lumaMin = min(lumaMin, f.LumaMin)
		lumaMax = max(lumaMax, f.LumaMax)
		means[i] = f.LumaMean
		switch f.RangeClass() {
		case "full":
			full++
		default:
			studio++
		}
		x0min, x0max = min(x0min, f.X0), max(x0max, f.X0)
		y0min, y0max = min(y0min, f.Y0), max(y0max, f.Y0)
		x1min, x1max = min(x1min, f.X1), max(x1max, f.X1)
		y1min, y1max = min(y1min, f.Y1), max(y1max, f.Y1)
	}
	sort.Float64s(laps)
	med := percentile(means, 0.5)
	var dim []int
	for i, f := range s.frames {
		if f.LumaMean < math.Max(10, 0.75*med) {
			dim = append(dim, i+1)
			s.frames[i].Dim = true
		}
	}
	fmt.Printf("== %s (%d frames, %dx%d) ==\n", s.name, n, w, h)
	if w > 0 && h > 0 {
		avgBoxW := avgFrac(s, func(f Frame) float64 { return float64(f.X1-f.X0+1) / float64(f.W) })
		avgBoxH := avgFrac(s, func(f Frame) float64 { return float64(f.Y1-f.Y0+1) / float64(f.H) })
		fmt.Printf("  box frac  w %.3f  h %.3f (mean per-frame box size)\n", avgBoxW, avgBoxH)
	}
	fmt.Printf("  box edge  x0 %d..%d  x1 %d..%d  y0 %d..%d  y1 %d..%d (across frames)\n",
		x0min, x0max, x1min, x1max, y0min, y0max, y1min, y1max)
	fmt.Printf("  sharp   laplacian var  p10 %.0f  p50 %.0f  p90 %.0f\n",
		percentile(laps, 0.10), percentile(laps, 0.50), percentile(laps, 0.90))
	fmt.Printf("  luma    min %d  max %d   (across frames)\n", lumaMin, lumaMax)
	fmt.Printf("  range   full %d / studio %d frames\n", full, studio)
	fmt.Printf("  comb    max %.3f  mean %.3f (1.0 = strong interlace; 0 = none)\n", combMax, combSum/float64(n))
	if len(dim) > 0 {
		fmt.Printf("  dim     %d frames (mean luma < %.0f): %v\n", len(dim), med*0.75, dim)
	} else {
		fmt.Printf("  dim     none (median mean luma %.0f)\n", med)
	}
}

// measure decodes one PNG and computes all per-frame metrics on luma.
func measure(path string) (Frame, error) {
	f, err := os.Open(path)
	if err != nil {
		return Frame{}, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return Frame{}, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	luma := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			luma[y*w+x] = lumaOf(img.At(x+b.Min.X, y+b.Min.Y))
		}
	}
	fr := Frame{Name: baseNoExt(path), W: w, H: h}

	// Luma histogram: min/max, mean, std, 1 % levels, edge fractions.
	var bins [256]int
	sum, sumSq := 0.0, 0.0
	for _, v := range luma {
		bins[v]++
		sum += float64(v)
		sumSq += float64(v) * float64(v)
	}
	total := w * h
	fr.LumaMean = sum / float64(total)
	fr.LumaStd = math.Sqrt(sumSq/float64(total) - fr.LumaMean*fr.LumaMean)
	for v := 0; v < 256; v++ {
		if bins[v] > 0 {
			fr.LumaMin = v
			break
		}
	}
	for v := 255; v >= 0; v-- {
		if bins[v] > 0 {
			fr.LumaMax = v
			break
		}
	}
	fr.SigLo, fr.SigHi = 0, 255
	for v := 0; v < 256; v++ {
		if bins[v]*100 >= total {
			fr.SigLo = v
			break
		}
	}
	for v := 255; v >= 0; v-- {
		if bins[v]*100 >= total {
			fr.SigHi = v
			break
		}
	}
	fr.FracBelow16 = fracBelow(bins, 16, total)
	fr.FracAbove235 = fracAbove(bins, 235, total)
	fr.FracAt0 = float64(bins[0]) / float64(total)
	fr.FracAt255 = float64(bins[255]) / float64(total)

	// Sharpness: variance of the 4-neighbour Laplacian over interior pixels.
	nl := 0.0
	css, cssq := 0.0, 0.0
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			i := y*w + x
			l := 4*float64(luma[i]) - float64(luma[i-w]) - float64(luma[i+w]) -
				float64(luma[i-1]) - float64(luma[i+1])
			css += l
			cssq += l * l
			nl++
		}
	}
	mean := css / nl
	fr.LapVar = cssq/nl - mean*mean

	// Combing heuristic: for a progressive picture the same-parity row pair
	// (y, y+2) is at least as different as the adjacent pair (y, y+1); a
	// fine interlaced pattern inverts that. Score = share of pixels where the
	// same-parity pair is clearly closer (<= 8 luma) than the adjacent pair.
	cnt, tot := 0, 0
	for y := 0; y+2 < h; y++ {
		rowA, rowB, rowC := luma[y*w:(y+1)*w], luma[(y+1)*w:(y+2)*w], luma[(y+2)*w:(y+3)*w]
		for x := 0; x < w; x++ {
			d1 := int(rowB[x]) - int(rowA[x])
			if d1 < 0 {
				d1 = -d1
			}
			d2 := int(rowC[x]) - int(rowA[x])
			if d2 < 0 {
				d2 = -d2
			}
			tot++
			if d2 <= 8 && d2 < d1 {
				cnt++
			}
		}
	}
	fr.Comb = float64(cnt) / float64(tot)

	// Active-picture box: scan the outermost rows/columns for black bars.
	cx, cy := stride(w), stride(h)
	fr.X0, fr.Y0, fr.X1, fr.Y1 = 1, 1, w, h
	for x := 0; x < w/2; x += cx {
		if !blackLine(luma, w, h, x, true, cx, cy) {
			break
		}
		fr.X0 = x + cx
	}
	for x := w - 1; x > w/2; x -= cx {
		if !blackLine(luma, w, h, x, true, cx, cy) {
			break
		}
		fr.X1 = x
	}
	for y := 0; y < h/2; y += cy {
		if !blackLine(luma, w, h, y, false, cx, cy) {
			break
		}
		fr.Y0 = y + cy
	}
	for y := h - 1; y > h/2; y -= cy {
		if !blackLine(luma, w, h, y, false, cx, cy) {
			break
		}
		fr.Y1 = y
	}
	if fr.X1 <= fr.X0 || fr.Y1 <= fr.Y0 {
		fr.X0, fr.Y0, fr.X1, fr.Y1 = 1, 1, w, h
	}
	return fr, nil
}

// blackLine reports whether the scanned line at column (vertical=true) or row
// position is >= barCoverage of "bar" luma, sampling with the given stride.
func blackLine(luma []uint8, w, h, pos int, vertical bool, sx, sy int) bool {
	seen, black := 0, 0
	if vertical {
		for y := 0; y < h; y += sy {
			seen++
			if luma[y*w+pos] < barLuma {
				black++
			}
		}
		return seen > 0 && float64(black)/float64(seen) >= barCoverage
	}
	for x := 0; x < w; x += sx {
		seen++
		if luma[pos*w+x] < barLuma {
			black++
		}
	}
	return seen > 0 && float64(black)/float64(seen) >= barCoverage
}

func fracBelow(bins [256]int, upTo, total int) float64 {
	t := 0
	for v := 0; v < upTo; v++ {
		t += bins[v]
	}
	return float64(t) / float64(total)
}

func fracAbove(bins [256]int, from, total int) float64 {
	t := 0
	for v := from; v < 256; v++ {
		t += bins[v]
	}
	return float64(t) / float64(total)
}

func stride(n int) int {
	s := n / scanStride
	if s < 1 {
		return 1
	}
	return s
}

// lumaOf weights 8-bit channels to luma with BT.601 in 16.16 fixed point —
// the same coefficients internal/vision.ToGray uses (0.299/0.587/0.114 scaled
// by 65536, which sum to 65536 exactly).
func lumaOf(c color.Color) uint8 {
	r, g, b, a := c.RGBA()
	if a == 0 {
		return 0
	}
	if a != 0xffff {
		// Un-premultiply. r,g,b <= 0xffff so r*0xffff fits in a uint32; clamp
		// the division result back to 16 bits.
		u := func(v uint32) uint32 {
			if v > 0xffff {
				return 0xffff
			}
			return v
		}
		r = u(r * 0xffff / a)
		g = u(g * 0xffff / a)
		b = u(b * 0xffff / a)
	}
	y := (19595*r + 38470*g + 7471*b + 32768) >> 16
	if y += 128; y > 0xffff {
		return 255
	}
	return uint8(y >> 8)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1)*p + 0.5)
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func avgFrac(s set, f func(Frame) float64) float64 {
	if len(s.frames) == 0 {
		return 0
	}
	var sum float64
	for _, fr := range s.frames {
		sum += f(fr)
	}
	return sum / float64(len(s.frames))
}

func baseNoExt(p string) string {
	b := filepath.Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

var _ = image.NewGray
