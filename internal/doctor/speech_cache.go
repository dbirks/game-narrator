package doctor

// The speech-cache disk row (ADR-0159, issue #49): the space the speech cache
// holds that no current published profile revision can hit — the dead weight a
// `game-narrator cache prune` removes once the page-aware key change (ADR-0155)
// and the shared-slot attribution (ADR-0156) made the old entries un-hittable,
// and the pre-synthesis key change (ADR-0153) made whole-message keys stale.
//
// The row watches the two numbers that decide whether that prune has room to
// run: the free space on the data partition, and the unreachable share of the
// cache itself. Both are measured rather than guessed; the share is taken from
// the prune's own dry-run walk over the real store, so the row cannot drift from
// the operator-facing commands (the CLI's `cache prune`, the API's
// speech.cache_prune job) that act on the answer.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	goruntime "runtime"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// The speech.cache_disk row (ADR-0159, issue #49) and its fixed thresholds.
// Thresholds are constants, not configuration: a threshold the operator cannot
// change cannot be tuned into silence (docs/spec/13 "Diagnostics page and CLI").
const (
	speechCacheDiskCheckID = "speech.cache_disk"
	// speechCacheDiskFreeReserveBytes is the free space on the data partition
	// the prune must fit before it can reclaim anything: under it the cache
	// itself risks the partition, whatever the unreachable share says.
	speechCacheDiskFreeReserveBytes int64 = 2 << 30 // 2 GiB
	// speechCacheDiskUnreachableWarnShare is the unreachable-share fraction at
	// which the row warns: the prune's would-be answer crossing it means the
	// next full pre-synthesis pass would add roughly half a GB of dead weight.
	speechCacheDiskUnreachableWarnShare float64 = 0.30
	// speechCacheMaxAssetBlobBytes mirrors the CLI's per-blob ceiling for the
	// cache's WAV blobs, so the row's asset store opens with the same bound the
	// serving process puts on it.
	speechCacheMaxAssetBlobBytes int64 = 512 << 20
)

// statfsSpace asks the kernel for the free and total bytes of the filesystem
// holding dir — the same read the API's host-vitals section uses; platforms
// without a stdlib statfs answer ok=false and the row then says unknown.
var statfsSpace = func(dir string) (int64, int64, bool) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		return 0, 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	size := int64(st.Bsize)
	return int64(st.Bavail) * size, int64(st.Blocks) * size, true
}

// humanBytes renders a byte count for a check detail (a size, not a path or a
// credential: docs/spec/13 keeps the report free of private values).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		if value < float64(unit) || u == "TB" {
			return fmt.Sprintf("%.1f %s", value/float64(unit), u)
		}
		value /= float64(unit)
	}
	return fmt.Sprintf("%.1f PB", value/float64(unit))
}

// checkSpeechCacheDisk answers the speech.cache_disk row: the free space on the
// data partition, and — while that is within reserve — the unreachable share of
// the cache the prune would remove.
func (r *runner) checkSpeechCacheDisk(ctx context.Context) Check {
	if strings.TrimSpace(r.opts.DataDir) == "" {
		return Check{Status: StatusFail, Summary: "no data directory is configured, so the speech cache cannot be measured"}
	}
	free, _, ok := statfsSpace(r.opts.DataDir)
	if !ok {
		return Check{
			Status:  StatusOK,
			Summary: "speech cache disk space unknown (statfs unavailable)",
			Detail:  "the data partition's free space could not be measured on this platform",
		}
	}
	if free < speechCacheDiskFreeReserveBytes {
		return Check{
			Status:  StatusDegraded,
			Summary: "speech cache disk space is low on the data partition",
			Detail:  fmt.Sprintf("only %s free on the data partition; the cache and its prune need %s", humanBytes(free), humanBytes(speechCacheDiskFreeReserveBytes)),
			Remedy:  "run `game-narrator cache prune --dry-run`, then `game-narrator cache prune` against the data directory, or free space on the partition",
		}
	}

	entries, freed, cacheTotal, err := r.measureSpeechCacheUnreachable(ctx)
	if err != nil {
		return Check{
			Status:  StatusDegraded,
			Summary: "speech cache unreachable entries could not be measured",
			Detail:  err.Error(),
			Remedy:  "run `game-narrator migrate` against the data directory, then re-run the doctor",
		}
	}
	if cacheTotal <= 0 {
		return Check{
			Status:  StatusOK,
			Summary: "the speech cache holds no entries to measure",
			Detail:  "no unreachable share is reportable until the cache has entries",
		}
	}
	if float64(freed)/float64(cacheTotal) > speechCacheDiskUnreachableWarnShare {
		return Check{
			Status:  StatusDegraded,
			Summary: "speech cache unreachable entries exceed the warn threshold",
			Detail:  fmt.Sprintf("%d cache entries no current published revision can hit; the prune would free %s", entries, humanBytes(freed)),
			Remedy:  "run `game-narrator cache prune` against the data directory to reclaim the unreachable entries",
		}
	}
	return Check{
		Status:  StatusOK,
		Summary: "speech cache disk space is within reserve and the unreachable share is within the warn threshold",
		Detail:  fmt.Sprintf("%d cache entries carried, %s unreachable would free", entries, humanBytes(freed)),
	}
}

// measureSpeechCacheUnreachable runs the prune the operator would run — the
// CLI's `cache prune`, the API's speech.cache_prune job (ADR-0159) — as a dry run
// over the data directory, and returns the would-be answer: how many entries
// and how many bytes no current published revision can hit. It reuses the
// runtime's reachable-key walk and the cache's reference-checked eviction
// unchanged, so a doctor row can never disagree with the command that acts on
// it. The dry run deletes nothing: the answers it returns are the would-be ones.
func (r *runner) measureSpeechCacheUnreachable(ctx context.Context) (entries int, freed int64, total int64, err error) {
	st, err := store.Open(ctx, store.DBPath(r.opts.DataDir))
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = st.Close() }()
	assetStore, err := assets.NewLocal(filepath.Join(r.opts.DataDir, "assets"), assets.WithMaxSize(speechCacheMaxAssetBlobBytes))
	if err != nil {
		return 0, 0, 0, err
	}
	cache := speech.New(assetStore, store.NewSpeechCacheIndex(st), 8, speech.WithReferenceChecker(st))
	voiceSvc := &voices.Service{
		Store:         st,
		Assets:        assetStore,
		Speech:        cache,
		InstanceID:    voices.DefaultInstanceID,
		BackendDigest: "preview-v1",
	}
	entries, freed, err = runtime.PruneSpeechCache(ctx, st, assetStore, voiceSvc, cache, true)
	if err != nil {
		return 0, 0, 0, err
	}
	var cacheTotal int64
	if snap, lErr := cache.Index.LoadAll(ctx); lErr == nil {
		for _, e := range snap {
			cacheTotal += e.Size
		}
	}
	return entries, freed, cacheTotal, nil
}
