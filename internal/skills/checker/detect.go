package checker

// detect.go composes the lower-level git helpers (EnsureCache / ResolveSHA /
// LogPath / CheckoutSubpath) and the subhash package into a single Detect()
// function that classifies upstream-vs-known state into one of four outcomes.
//
// The four outcomes exist so the caller (cron service, Task 9) can avoid
// invoking an LLM classifier for the common cases where nothing meaningful
// has changed:
//
//   - NoMovement:    upstream HEAD is exactly where we left it. Cheapest path.
//   - NoPathTouch:   HEAD moved, but no commit touched our subpath. Caller can
//                    advance last_seen_sha without re-hashing.
//   - RevertedToSame: subpath was modified but ended up byte-identical to what
//                    we already had (e.g. revert + re-apply). Caller advances
//                    last_seen_sha; last_seen_hash is unchanged by definition.
//   - Drift:         real change. Caller hands off to LLM classifier.

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/skills/subhash"
)

// Result is the four-way outcome of Detect. Stringly-typed so it survives
// JSON round-trips (Task 9 will emit these as Prometheus labels and store
// them as text in drift_reports).
type Result string

const (
	ResultNoMovement     Result = "no_movement"
	ResultNoPathTouch    Result = "no_path_touch"
	ResultRevertedToSame Result = "reverted_to_same"
	ResultDrift          Result = "drift"
)

// UpstreamRef is the minimal upstream description Detect needs. We define a
// local struct (instead of importing internal/store.SkillUpstream or
// internal/cli/sync.Upstream) to keep this package free of upward
// dependencies — both higher layers can build an UpstreamRef from their own
// types in a single line.
type UpstreamRef struct {
	GitSubpath string // e.g. skills/foo (empty = repo root)
	GitRef     string // branch / tag / SHA; empty = HEAD
}

// Detection is the result of a Detect() call. Fields are populated only when
// meaningful for the Result variant:
//
//   - NoMovement:     NewSHA only.
//   - NoPathTouch:    NewSHA only.
//   - RevertedToSame: NewSHA + NewHash.
//   - Drift:          NewSHA + NewHash + CommitsAhead + ChangedFiles + DiffSummary.
type Detection struct {
	Result       Result
	NewSHA       string
	NewHash      string
	CommitsAhead int
	ChangedFiles []string
	DiffSummary  string
}

// truncationSuffix is appended to DiffSummary if it had to be cut. Keeping it
// as an exported-ish constant makes the suffix discoverable for tests and
// for downstream prompt-building code (Task 10).
const truncationSuffix = "\n... (truncated)"

// Detect classifies upstream state against the caller's last-known
// (lastSeenSHA, lastSeenHash) into one of the four Result variants.
//
// Preconditions:
//   - cacheDir already contains a clone (caller is responsible for
//     EnsureCache()). Detect deliberately does not refresh the cache; the
//     cron service does that once per tick before iterating skills.
//   - lastSeenSHA may be empty (first-ever check). In that case we always
//     run through the full pipeline; the diff range becomes "" → newSHA,
//     which LogPath/git-diff interpret as "full history reachable from
//     newSHA". This matches steady-state semantics: in practice the push
//     handler (Task 4) seeds last_seen_* on registration, so the
//     empty-lastSeenSHA case is only hit for skills that were tracked but
//     never had a baseline — rare, and "always classify as drift" is the
//     safe default.
//
// llmDiffMaxBytes caps the size of DiffSummary; if the raw `git diff --stat`
// output exceeds this, the summary is truncated and suffixed with
// "\n... (truncated)".
func Detect(
	ctx context.Context,
	upstream *UpstreamRef,
	lastSeenSHA, lastSeenHash, cacheDir string,
	llmDiffMaxBytes int,
) (*Detection, error) {
	if upstream == nil {
		return nil, fmt.Errorf("Detect: upstream must not be nil")
	}
	if cacheDir == "" {
		return nil, fmt.Errorf("Detect: cacheDir must not be empty")
	}

	// Stage 1: did upstream HEAD move at all?
	newSHA, err := ResolveSHA(ctx, cacheDir, upstream.GitRef)
	if err != nil {
		return nil, fmt.Errorf("Detect: resolve %q: %w", upstream.GitRef, err)
	}
	if lastSeenSHA != "" && newSHA == lastSeenSHA {
		return &Detection{Result: ResultNoMovement, NewSHA: newSHA}, nil
	}

	// Stage 2: did any commit since lastSeenSHA touch our subpath?
	commits, err := LogPath(ctx, cacheDir, lastSeenSHA, newSHA, upstream.GitSubpath)
	if err != nil {
		return nil, fmt.Errorf("Detect: log %s..%s -- %s: %w",
			lastSeenSHA, newSHA, upstream.GitSubpath, err)
	}
	if len(commits) == 0 {
		return &Detection{Result: ResultNoPathTouch, NewSHA: newSHA}, nil
	}

	// Stage 3: extract subpath at newSHA, hash it, compare to lastSeenHash.
	tmpDir, err := os.MkdirTemp("", "checker-detect-*")
	if err != nil {
		return nil, fmt.Errorf("Detect: mkdtemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	if err := CheckoutSubpath(ctx, cacheDir, newSHA, upstream.GitSubpath, tmpDir); err != nil {
		return nil, fmt.Errorf("Detect: checkout %s:%s: %w",
			newSHA, upstream.GitSubpath, err)
	}
	newHash, err := subhash.Hash(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("Detect: subhash: %w", err)
	}
	if lastSeenHash != "" && newHash == lastSeenHash {
		return &Detection{
			Result:  ResultRevertedToSame,
			NewSHA:  newSHA,
			NewHash: newHash,
		}, nil
	}

	// Stage 4: real drift. Gather diff metadata for the LLM / UI.
	changed, err := gitDiffNames(ctx, cacheDir, lastSeenSHA, newSHA, upstream.GitSubpath)
	if err != nil {
		return nil, fmt.Errorf("Detect: diff names: %w", err)
	}
	summary, err := gitDiffSummary(ctx, cacheDir, lastSeenSHA, newSHA, upstream.GitSubpath, llmDiffMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("Detect: diff stat: %w", err)
	}

	return &Detection{
		Result:       ResultDrift,
		NewSHA:       newSHA,
		NewHash:      newHash,
		CommitsAhead: len(commits),
		ChangedFiles: changed,
		DiffSummary:  summary,
	}, nil
}

// gitDiffNames returns `git diff --name-only fromSHA..toSHA -- subpath` as a
// slice of paths (one per line, empty slice if no changes).
//
// fromSHA == "" → uses git's well-known empty-tree SHA so the call still
// produces useful output (every file at toSHA:subpath is "added"). This
// matches the LogPath semantics: empty-from means "full history reachable
// from toSHA".
func gitDiffNames(ctx context.Context, cacheDir, fromSHA, toSHA, subpath string) ([]string, error) {
	args := []string{"diff", "--name-only", diffRange(fromSHA, toSHA)}
	if subpath != "" {
		args = append(args, "--", subpath)
	}
	out, err := runGit(ctx, cacheDir, args...)
	if err != nil {
		return nil, err
	}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(out, "\n"), nil
}

// gitDiffSummary returns `git diff --stat fromSHA..toSHA -- subpath`,
// truncated to maxBytes (with the truncation suffix appended) if the raw
// output exceeds that limit. maxBytes <= 0 disables truncation.
func gitDiffSummary(ctx context.Context, cacheDir, fromSHA, toSHA, subpath string, maxBytes int) (string, error) {
	args := []string{"diff", "--stat", diffRange(fromSHA, toSHA)}
	if subpath != "" {
		args = append(args, "--", subpath)
	}
	out, err := runGit(ctx, cacheDir, args...)
	if err != nil {
		return "", err
	}
	if maxBytes > 0 && len(out) > maxBytes {
		out = out[:maxBytes] + truncationSuffix
	}
	return out, nil
}

// diffRange returns "from..to" for a normal diff, or just "to" against the
// empty-tree sentinel SHA when from is empty (first-ever check). Git ships
// with the empty-tree object hard-coded at this hash, so this works in any
// repository without us having to create a tree first.
func diffRange(fromSHA, toSHA string) string {
	const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	if fromSHA == "" {
		return emptyTree + ".." + toSHA
	}
	return fromSHA + ".." + toSHA
}
