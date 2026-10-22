// Package runtime owns the session runtime: one live pipeline run at a time,
// its state, its events and the developer-trace evidence behind them
// (docs/spec/01 "Key invariants", docs/spec/08 "Example session workflow",
// docs/spec/13 "Logging/privacy", ADR-0024).
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/vision"
)

// Developer-trace evidence budget (docs/spec/13 "Developer trace sample mode
// may store frame crops/audio for debugging under time/size quotas and
// explicit toggle"): a per-session byte quota with oldest-first eviction, and
// a per-sample time gate so a 30 fps live capture cannot burn the quota on
// one screen change.
const (
	// DevTraceFrameWidth caps the stored full-frame preview width: ≤640 px
	// wide keeps one JPEG in the tens-of-KB range, which is what a 64 MiB
	// session quota is sized to hold for minutes, not seconds.
	DevTraceFrameWidth = 640
	// devTraceJPEGQuality is the full-frame JPEG quality. A preview for a
	// human eyeballing the OCR input does not need lossless.
	devTraceJPEGQuality = 80
	// DefaultDevTraceQuota is the per-session byte budget for stored frames
	// and crops; the oldest samples are evicted first.
	DefaultDevTraceQuota = 64 << 20
	// devTraceMaxBlobBytes caps one stored sample so a single pathological
	// frame cannot eat the whole session quota by itself.
	devTraceMaxBlobBytes = 8 << 20
	// devTraceSampleInterval is the minimum gap, in source time, between two
	// stored samples: at most one sample per second of captured time
	// (ADR-0115 §5). The interval is measured on the frame's own capture clock,
	// so the encode cost — the per-frame CPU the board cannot spare — is a rate
	// of the capture, not of the host: a changed picture still gets its sample
	// within a second, a stable picture costs one encode per second at most.
	devTraceSampleInterval = time.Second
	// DevTracePinnedQuotaFraction is the per-session pinned-evidence byte
	// budget as a fraction of the rolling DevTraceQuota (issue #46): the frame
	// and crop of a read that committed or that the read-through spoke are held
	// out of the rolling idle eviction and live under this budget instead, so
	// pinning stays bounded the same way the rolling quota is. A session that
	// never pins holds nothing here; a session that pins spends at most this
	// share of the quota before its oldest pinned evidence is evicted first.
	DevTracePinnedQuotaFraction float64 = 0.25
	// devTracePinContextReads bounds the reads pinned alongside a committing
	// or read-through-spoken read: the committing read itself plus this many
	// retained reads before it, so the context a commit was made from survives
	// the same idle sweep (issue #46).
	devTracePinContextReads = 3
)

// devFrameSample is the evidence of one captured frame: the stored full-frame
// JPEG and, per region, the stored raw ROI crop PNG the OCR stage reads.
// text.observed references these by sha256 (frame_asset, crop_asset).
type devFrameSample struct {
	ts    int64  // the frame's CaptureTimestampNS (source clock)
	frame string // sha256 of the downscaled full-frame JPEG
	crops map[string]string
	// basis is the crop window the session's crop options mapped the regions
	// through when this sample was taken: the tracker's accepted window once
	// one has settled, the whole frame otherwise (fractions of the frame, so
	// the developer inspector's overlay math needs no frame dimensions).
	basis *cropBasis
}

// cropBasis is one sample's crop basis (ADR-0082 amendment 1): where the
// active picture sits in the frame, in fractions of the frame, and whether
// the tracker had settled a window at the sampling instant. The developer
// page draws the region overlay through this box and says so in words — a
// mis-settled window is visible, not guessed at.
type cropBasis struct {
	X, Y, W, H float64
	Settled    bool
}

// devHeldAsset is one blob this session's dev-trace store keeps, for the
// oldest-first eviction.
type devHeldAsset struct {
	sha  string
	size int64
}

// devTraceRecorder stores the per-session frame and crop evidence in its own
// content-addressed store (ADR-0014's Local backend, private 0700 layout)
// under the session's dev-trace namespace, enforces the byte quota and answers
// "which frame does this capture timestamp belong to" for the text.observed
// enrichment.
//
// Privacy (docs/spec/13): a Recorder is only ever constructed when the session
// was started with dev_trace by a caller holding the developer role, and only
// its events and assets are ever read back through the developer-only routes.
// Without it the pipeline stores and references no images at all.
type devTraceRecorder struct {
	store *assets.Local
	quota int64
	// crop is the session's crop options (the tracked active window, ADR-0082
	// amendment 1), set once at session start before any frame flows, so the
	// stored crops are the crops the pipeline reads and the samples can name
	// the window they were cut through.
	crop vision.CropOptions

	mu       sync.Mutex
	order    []devHeldAsset // oldest first
	retained map[string]bool
	types    map[string]string // sha → media type, for the inspector's Content-Type
	total    int64

	// Pinned evidence (issue #46): the frame and crop of a read that committed or
	// that the read-through spoke are held out of the rolling eviction and live
	// under the session's pinned budget instead. `pinned` names the retained
	// blobs the operator asked to keep; `pinnedOrder` is the same set in
	// oldest-pinned-first order; `pinnedTotal` books the pinned bytes against
	// `pinnedQuota`. The rolling sweep skips anything in `pinned`, and only evicts
	// pinned evidence when the pinned budget itself overflows, oldest pinned
	// first — the mirror image of the rolling oldest-first sweep.
	pinned      map[string]bool
	pinnedOrder []devHeldAsset
	pinnedTotal int64
	pinnedQuota int64

	lastSampleAtNS int64 // the instant of the last stored sample, on its own clock
	lastFromSource bool  // that instant was the frame's source clock, not the wall
	haveSample     bool
	samples        []devFrameSample
}

// NewDevTraceRecorder prepares the session's private dev-trace store under
// root (one directory per session) with a byte quota. quota <= 0 takes the
// documented 64 MiB default.
func NewDevTraceRecorder(root string, quota int64) (*devTraceRecorder, error) {
	if quota <= 0 {
		quota = DefaultDevTraceQuota
	}
	st, err := assets.NewLocal(root, assets.WithMaxSize(devTraceMaxBlobBytes))
	if err != nil {
		return nil, fmt.Errorf("devtrace: open store: %w", err)
	}
	return &devTraceRecorder{
		store:       st,
		quota:       quota,
		pinnedQuota: int64(float64(quota) * DevTracePinnedQuotaFraction),
		retained:    map[string]bool{},
		types:       map[string]string{},
		pinned:      map[string]bool{},
	}, nil
}

// SetCropOptions hands the recorder the session's crop options so its stored
// crops describe the same crop basis the pipeline reads (the tracked
// active-picture window, ADR-0082 amendment 1). It is called once when the
// session starts, before the source's pump delivers a frame, so the field
// needs no lock of its own; reading the tracker's window concurrently with
// the frame stage's Update is safe because Tracker.Box is mutex-guarded.
func (r *devTraceRecorder) SetCropOptions(o vision.CropOptions) { r.crop = o }

// recordFrame stores the sampled evidence for one frame and returns the
// sample, or nil when the sampling gate skipped the frame. A store failure is
// returned, not swallowed: the session keeps running untraced rather than
// dropping frames over a debug feature (docs/spec/13 degraded modes).
func (r *devTraceRecorder) recordFrame(ctx context.Context, f capture.Frame, regions []games.Region) (*devFrameSample, error) {
	if r == nil {
		return nil, nil
	}
	// The sampling gate (ADR-0115 §5): at most one encoded sample per second
	// of source time, whether or not the picture changed. The interval is on
	// the frame's own capture clock, the wall clock when the frame names none
	// (a source-less stream is rate-limited on wall time the way the old gate
	// did). An interval is only meaningful within one clock domain: a frame
	// whose instant comes from a different clock than the last sample's is
	// stored, because comparing the two would be comparing source time against
	// wall time, and the gate's fail-open direction is a stored sample (the
	// byte quota still bounds the total).
	//
	// The gate the picture's hash used to key on re-encoded the full-frame
	// JPEG and every crop on every frame of an analog feed — where the hash
	// changes every frame — which is the per-frame CPU cost this gate bounds:
	// the encode, not the store, is what the board could not spare.
	sampleAt := f.CaptureTimestampNS
	fromSource := sampleAt > 0
	if !fromSource {
		sampleAt = time.Now().UnixNano()
	}
	r.mu.Lock()
	if r.haveSample && r.lastFromSource == fromSource &&
		sampleAt-r.lastSampleAtNS < int64(devTraceSampleInterval) {
		r.mu.Unlock()
		return nil, nil
	}
	r.mu.Unlock()
	img := f.Image
	if img == nil || img.Bounds().Empty() {
		return nil, nil
	}

	frameSHA, err := r.storeBytes(ctx, encodeJPEG(downscaleToWidth(img, DevTraceFrameWidth), devTraceJPEGQuality), "image/jpeg")
	if err != nil {
		return nil, err
	}
	crops := map[string]string{}
	for _, reg := range regions {
		// The session's own crop options: the stored evidence must be the crop
		// the OCR stage read, cut through the same window basis it will stamp
		// on the event (ADR-0082 amendment 1).
		crop := vision.CropNormalized(img, vision.RectOf(reg), r.crop)
		if crop == nil || crop.Bounds().Empty() {
			continue
		}
		sha, err := r.storeBytes(ctx, encodePNG(crop), "image/png")
		if err != nil {
			return nil, err
		}
		crops[reg.ID] = sha
	}
	basis := cropBasis{W: 1, H: 1} // the whole frame, until the tracker says otherwise
	b := img.Bounds()
	if tr := r.crop.Tracker; tr != nil {
		if a, ok := tr.Box(); ok && b.Dx() > 0 && b.Dy() > 0 {
			basis = cropBasis{
				X:       float64(a.Min.X-b.Min.X) / float64(b.Dx()),
				Y:       float64(a.Min.Y-b.Min.Y) / float64(b.Dy()),
				W:       float64(a.Dx()) / float64(b.Dx()),
				H:       float64(a.Dy()) / float64(b.Dy()),
				Settled: true,
			}
		}
	}
	s := devFrameSample{ts: f.CaptureTimestampNS, frame: frameSHA, crops: crops, basis: &basis}
	r.mu.Lock()
	r.samples = append(r.samples, s)
	r.lastSampleAtNS = sampleAt
	r.lastFromSource = fromSource
	r.haveSample = true
	r.mu.Unlock()
	return &s, nil
}

// lookup answers the text.observed enrichment: for the observation's capture
// timestamp (source clock) it returns the nearest stored sample at or before
// it — the closest frame the recorder actually kept — and nil when nothing was
// stored yet. The sample is the evidence, so an exact frame is preferred, but
// the sampling gate (rate limiting) makes "nearest kept sample" the honest
// fallback rather than a fabricated match (ADR-0024).
func (r *devTraceRecorder) lookup(ts int64) (*devFrameSample, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.samples) == 0 {
		return nil, false
	}
	var best *devFrameSample
	for i := range r.samples {
		s := &r.samples[i]
		if s.ts > ts {
			break
		}
		best = s
	}
	if best == nil {
		best = &r.samples[len(r.samples)-1]
	}
	return best, true
}

// asset streams one stored blob and returns its media type: the read path
// behind GET /api/v1/dev/assets/{sha256}. The recorder's ledger, not the
// store's, names the type: PutStaged was called without one, and a sampled
// frame is a JPEG while an ROI crop is a PNG — the inspector must not have to
// sniff content to display what it was told about.
func (r *devTraceRecorder) asset(ctx context.Context, sha string) (io.ReadCloser, assets.Info, error) {
	rc, info, err := r.store.Get(ctx, sha)
	if err != nil {
		return nil, assets.Info{}, err
	}
	r.mu.Lock()
	if mt, ok := r.types[sha]; ok {
		info.MediaType = mt
	}
	r.mu.Unlock()
	return rc, info, nil
}

// totalBytes reports how much of the quota the recorder currently holds.
func (r *devTraceRecorder) totalBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// sampleCount reports how many frames were sampled, for the stop log line.
func (r *devTraceRecorder) sampleCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.samples)
}

// storeBytes commits one encoded sample to the session's store and books it
// against the quota, evicting the oldest retained blobs until the total fits.
// Eviction runs through the store's own reachability walk (ADR-0014), so the
// on-disk tree and the in-memory ledger can never drift apart.
func (r *devTraceRecorder) storeBytes(ctx context.Context, data []byte, mediaType string) (string, error) {
	if len(data) > devTraceMaxBlobBytes {
		return "", fmt.Errorf("devtrace: sample of %d bytes exceeds the per-blob bound", len(data))
	}
	id, _, err := r.store.PutStaged(ctx, bytes.NewReader(data), "")
	if err != nil {
		return "", fmt.Errorf("devtrace: stage sample: %w", err)
	}
	committed, err := r.store.Commit(ctx, id)
	if err != nil {
		_ = r.store.Abort(ctx, id)
		return "", fmt.Errorf("devtrace: commit sample: %w", err)
	}

	r.mu.Lock()
	if !r.retained[committed.SHA256] {
		r.retained[committed.SHA256] = true
		r.order = append(r.order, devHeldAsset{sha: committed.SHA256, size: committed.Size})
		r.total += committed.Size
	}
	r.types[committed.SHA256] = mediaType
	evicted := r.evictOldestLocked()
	// Eviction is a fact about the evidence, not just the ledger: a sample
	// whose frame blob went is a sample that is gone, and the lookup must not
	// hand back a reference to bytes that no longer exist.
	if len(evicted) > 0 {
		dropped := map[string]bool{}
		for _, sha := range evicted {
			dropped[sha] = true
		}
		kept := r.samples[:0]
		for _, s := range r.samples {
			if dropped[s.frame] {
				continue
			}
			kept = append(kept, s)
		}
		r.samples = kept
	}
	// The eviction decision is final under the lock; the walk happens outside
	// it so a slow store cannot hold the recorder's state.
	retained := make(map[string]bool, len(r.retained))
	for k, v := range r.retained {
		retained[k] = v
	}
	r.mu.Unlock()

	if _, err := r.store.DeleteUnreachable(ctx, func(hash string) bool { return retained[hash] }, false); err != nil {
		return "", fmt.Errorf("devtrace: evict oldest samples: %w", err)
	}
	return committed.SHA256, nil
}

// heldSizeLocked returns the booked size of one retained blob, or 0 when the
// ledger does not hold it. Caller holds r.mu.
func (r *devTraceRecorder) heldSizeLocked(sha string) int64 {
	for _, a := range r.order {
		if a.sha == sha {
			return a.size
		}
	}
	return 0
}

// evictOldestLocked drops the oldest unpinned retained blob until the rolling
// byte total fits the per-session quota again (issue #46: the frame and crop of
// a committing or spoken read are pinned, so the rolling sweep only reclaims
// the idle evidence around them). It returns the hashes it dropped; a caller
// that got some must prune the samples that referenced them. Caller holds r.mu.
func (r *devTraceRecorder) evictOldestLocked() []string {
	var evicted []string
	for r.total > r.quota && len(r.order) > 0 {
		idx := 0
		for idx < len(r.order) && r.pinned[r.order[idx].sha] {
			idx++
		}
		if idx >= len(r.order) {
			break // every retained blob is pinned; the pinned budget owns the bytes
		}
		oldest := r.order[idx]
		if idx == 0 {
			r.order = r.order[1:]
		} else {
			rest := make([]devHeldAsset, 0, len(r.order)-1)
			for i, a := range r.order {
				if i != idx {
					rest = append(rest, a)
				}
			}
			r.order = rest
		}
		if r.retained[oldest.sha] {
			delete(r.retained, oldest.sha)
			r.total -= oldest.size
			evicted = append(evicted, oldest.sha)
		}
	}
	return evicted
}

// pinLocked marks shas as pinned evidence and books their bytes against the
// per-session pinned budget, evicting the oldest pinned first once that budget
// overflows. It returns the hashes it had to evict. Caller holds r.mu.
func (r *devTraceRecorder) pinLocked(shas []string) []string {
	var evicted []string
	for _, sha := range shas {
		if r.pinned[sha] {
			continue
		}
		size := r.heldSizeLocked(sha)
		r.pinned[sha] = true
		r.pinnedOrder = append(r.pinnedOrder, devHeldAsset{sha: sha, size: size})
		r.pinnedTotal += size
		for r.pinnedTotal > r.pinnedQuota && len(r.pinnedOrder) > 0 {
			oldest := r.pinnedOrder[0]
			r.pinnedOrder = r.pinnedOrder[1:]
			delete(r.pinned, oldest.sha)
			r.pinnedTotal -= oldest.size
			evicted = append(evicted, oldest.sha)
			if r.dropFromOrderLocked(oldest.sha) {
				delete(r.retained, oldest.sha)
				r.total -= oldest.size
			}
		}
	}
	return evicted
}

// dropFromOrderLocked removes one ledger entry. It reports whether the entry
// was present. Caller holds r.mu.
func (r *devTraceRecorder) dropFromOrderLocked(sha string) bool {
	for i, a := range r.order {
		if a.sha == sha {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return true
		}
	}
	return false
}

// PinSample pins the stored evidence behind one committed or spoken read —
// the frame and the crops that were stored for it (issue #46) — together with
// the last reads before it, so the rolling idle eviction cannot take the "why
// did OCR read X" evidence while the line is still the one the operator is
// asking about. The caller names the sample that produced the committing or
// read-through-spoken read; the recorder marks the sample's frame and crop
// blobs as pinned and books the bytes against the per-session pinned budget,
// which evicts the oldest pinned first once that budget is full. The last
// reads before it are pinned alongside the committing read, so the context a
// commit was made from survives the same idle sweep.
//
// A sample whose frame is no longer retained has nothing left to pin: the pin
// lands on the evidence, not on the policy, and an already-evicted read is gone
// rather than pinned forever.
func (r *devTraceRecorder) PinSample(ctx context.Context, s *devFrameSample) error {
	if s == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.retained[s.frame] {
		return nil
	}
	shas := r.pinnedShasLocked(s)
	if len(shas) == 0 {
		return nil
	}
	evicted := r.pinLocked(shas)
	if len(evicted) > 0 {
		dropped := map[string]bool{}
		for _, sha := range evicted {
			dropped[sha] = true
		}
		kept := r.samples[:0]
		for _, s := range r.samples {
			if dropped[s.frame] {
				continue
			}
			kept = append(kept, s)
		}
		r.samples = kept
		if _, err := r.store.DeleteUnreachable(ctx, func(hash string) bool { return r.retained[hash] }, false); err != nil {
			return fmt.Errorf("devtrace: evict pinned evidence: %w", err)
		}
	}
	return nil
}

// pinnedShasLocked returns the evidence hashes to pin for one read: the
// frame and the crops that were stored for it, plus the last devTracePinContextReads
// retained reads before it. The hashes come back oldest read first — the
// order the pinned budget evicts in once it overflows — so the context
// precedes the committing read it was read alongside of. Caller holds r.mu.
func (r *devTraceRecorder) pinnedShasLocked(s *devFrameSample) []string {
	var context []*devFrameSample
	for i := len(r.samples) - 1; i >= 0; i-- {
		cand := &r.samples[i]
		if cand.frame == s.frame && cand.ts == s.ts {
			break // reached the committing read; the reads before it are context
		}
		context = append(context, cand)
		if len(context) >= devTracePinContextReads {
			break
		}
	}
	var shas []string
	// context is newest-first; rebuild it oldest-first, then the committing read.
	for i := len(context) - 1; i >= 0; i-- {
		c := context[i]
		shas = append(shas, c.frame)
		cIDs := make([]string, 0, len(c.crops))
		for id := range c.crops {
			cIDs = append(cIDs, id)
		}
		sort.Strings(cIDs)
		for _, id := range cIDs {
			shas = append(shas, c.crops[id])
		}
	}
	// The committing read's own evidence, newest of the pinned set.
	shas = append(shas, s.frame)
	cropIDs := make([]string, 0, len(s.crops))
	for id := range s.crops {
		cropIDs = append(cropIDs, id)
	}
	sort.Strings(cropIDs)
	for _, id := range cropIDs {
		shas = append(shas, s.crops[id])
	}
	return shas
}

// ── image helpers (stdlib only: no new Go modules, ADR-0008) ───────────────

// downscaleToWidth converts img to RGBA and scales it down so its width is at
// most maxW, preserving the aspect ratio. Nearest-neighbour: the output is a
// human preview for the developer inspector, not OCR input, so a box filter is
// spend the appliance would rather not make at capture rate.
func downscaleToWidth(img image.Image, maxW int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 || maxW <= 0 {
		return nil
	}
	if sw <= maxW {
		rgba := image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.Draw(rgba, rgba.Bounds(), img, b.Min, draw.Src)
		return rgba
	}
	dh := sh * maxW / sw
	if dh < 1 {
		dh = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, maxW, dh))
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + y*sh/dh
		for x := 0; x < maxW; x++ {
			sx := b.Min.X + x*sw/maxW
			out.Set(x, y, img.At(sx, sy))
		}
	}
	return out
}

func encodeJPEG(img image.Image, quality int) []byte {
	if img == nil {
		return nil
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
	return buf.Bytes()
}

func encodePNG(img image.Image) []byte {
	if img == nil {
		return nil
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// devTraceEventData is the two additions the recorder makes to a text.observed
// payload (docs/spec/13 "sampled image references"): content hashes, never
// bytes — the bytes travel only through GET /api/v1/dev/assets (developer
// role) and never leave the box (docs/spec/01 "Key invariants" 3).
func attachDevTraceData(data json.RawMessage, s *devFrameSample, regionID string) json.RawMessage {
	if s == nil {
		return data
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	m["frame_asset"] = s.frame
	if sha, ok := s.crops[regionID]; ok {
		m["crop_asset"] = sha
	}
	// The crop basis of the sample (ADR-0082 amendment 1): the window the
	// region overlay was cut through, in fractions of the frame, so the
	// developer inspector can draw the same box and say whether the tracker
	// had settled when the sample was taken.
	if s.basis != nil {
		m["active_window"] = map[string]any{
			"x":       s.basis.X,
			"y":       s.basis.Y,
			"w":       s.basis.W,
			"h":       s.basis.H,
			"settled": s.basis.Settled,
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}
