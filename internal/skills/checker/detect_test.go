package checker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comma-compliance/arc-relay/internal/skills/subhash"
)

// hashSubpathAt extracts <sha>:<subpath> from cacheDir into a tempdir and
// returns its subhash. Used by tests to compute "lastSeenHash" baselines.
func hashSubpathAt(t *testing.T, cacheDir, sha, subpath string) string {
	t.Helper()
	tmp, err := os.MkdirTemp("", "detect-hash-*")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := CheckoutSubpath(context.Background(), cacheDir, sha, subpath, tmp); err != nil {
		t.Fatalf("CheckoutSubpath: %v", err)
	}
	h, err := subhash.Hash(tmp)
	if err != nil {
		t.Fatalf("subhash.Hash: %v", err)
	}
	return h
}

func TestDetect_NoMovement(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1"}, "skills/foo: v1")

	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	head, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	hash := hashSubpathAt(t, cache, head, "skills/foo")

	d, err := Detect(context.Background(), &UpstreamRef{
		GitSubpath: "skills/foo",
		GitRef:     "origin/main",
	}, head, hash, cache, 4096)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Result != ResultNoMovement {
		t.Fatalf("Result = %q, want %q", d.Result, ResultNoMovement)
	}
	if d.NewSHA != head {
		t.Fatalf("NewSHA = %q, want %q", d.NewSHA, head)
	}
}

func TestDetect_NoPathTouch(t *testing.T) {
	src := makeTestRepo(t)
	// Establish baseline that touches the subpath.
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1"}, "skills/foo: v1")
	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	baselineSHA, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve baseline: %v", err)
	}
	baselineHash := hashSubpathAt(t, cache, baselineSHA, "skills/foo")

	// Add an unrelated commit upstream that does NOT touch skills/foo.
	addCommit(t, src, map[string]string{"unrelated.txt": "x"}, "unrelated")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}

	d, err := Detect(context.Background(), &UpstreamRef{
		GitSubpath: "skills/foo",
		GitRef:     "origin/main",
	}, baselineSHA, baselineHash, cache, 4096)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Result != ResultNoPathTouch {
		t.Fatalf("Result = %q, want %q", d.Result, ResultNoPathTouch)
	}
	if d.NewSHA == baselineSHA {
		t.Fatalf("NewSHA should differ from baseline (HEAD moved): got %q", d.NewSHA)
	}
}

func TestDetect_RevertedToSame(t *testing.T) {
	src := makeTestRepo(t)
	// Baseline: skills/foo/SKILL.md = v1
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1"}, "skills/foo: v1")
	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	baselineSHA, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve baseline: %v", err)
	}
	baselineHash := hashSubpathAt(t, cache, baselineSHA, "skills/foo")

	// Mutate, then revert to identical content.
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v2"}, "skills/foo: v2")
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1"}, "skills/foo: revert to v1")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}

	d, err := Detect(context.Background(), &UpstreamRef{
		GitSubpath: "skills/foo",
		GitRef:     "origin/main",
	}, baselineSHA, baselineHash, cache, 4096)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Result != ResultRevertedToSame {
		t.Fatalf("Result = %q, want %q (NewSHA=%s NewHash=%s)", d.Result, ResultRevertedToSame, d.NewSHA, d.NewHash)
	}
	if d.NewSHA == baselineSHA {
		t.Fatalf("NewSHA should differ from baseline: got %q", d.NewSHA)
	}
	if d.NewHash != baselineHash {
		t.Fatalf("NewHash should match baselineHash; got %q, want %q", d.NewHash, baselineHash)
	}
}

func TestDetect_Drift(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1\nline-a\n"}, "skills/foo: v1")
	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	baselineSHA, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve baseline: %v", err)
	}
	baselineHash := hashSubpathAt(t, cache, baselineSHA, "skills/foo")

	// Real change: modify SKILL.md and add a new file under the subpath.
	addCommit(t, src, map[string]string{
		"skills/foo/SKILL.md":  "v2\nline-a\nline-b\n",
		"skills/foo/extra.txt": "added\n",
	}, "skills/foo: v2 + extra")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}

	d, err := Detect(context.Background(), &UpstreamRef{
		GitSubpath: "skills/foo",
		GitRef:     "origin/main",
	}, baselineSHA, baselineHash, cache, 4096)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Result != ResultDrift {
		t.Fatalf("Result = %q, want %q", d.Result, ResultDrift)
	}
	if d.NewSHA == baselineSHA {
		t.Fatalf("NewSHA should differ: got %q", d.NewSHA)
	}
	if d.NewHash == "" || d.NewHash == baselineHash {
		t.Fatalf("NewHash should be populated and differ from baseline; got %q (baseline %q)", d.NewHash, baselineHash)
	}
	if d.CommitsAhead < 1 {
		t.Fatalf("CommitsAhead = %d, want >= 1", d.CommitsAhead)
	}
	if len(d.ChangedFiles) == 0 {
		t.Fatalf("ChangedFiles empty, want at least one")
	}
	// At least SKILL.md and extra.txt should appear.
	got := strings.Join(d.ChangedFiles, "\n")
	if !strings.Contains(got, "skills/foo/SKILL.md") {
		t.Fatalf("ChangedFiles missing SKILL.md: %v", d.ChangedFiles)
	}
	if !strings.Contains(got, "skills/foo/extra.txt") {
		t.Fatalf("ChangedFiles missing extra.txt: %v", d.ChangedFiles)
	}
	if d.DiffSummary == "" {
		t.Fatalf("DiffSummary empty, expected git diff --stat output")
	}
}

func TestDetect_DriftDiffSummaryTruncated(t *testing.T) {
	src := makeTestRepo(t)
	// Many files so diff --stat output is reasonably long.
	files := map[string]string{}
	for i := 0; i < 20; i++ {
		files[filepath.Join("skills/foo", "f"+itoa(i)+".txt")] = "v1\n"
	}
	addCommit(t, src, files, "skills/foo: v1 many files")

	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	baselineSHA, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve baseline: %v", err)
	}
	baselineHash := hashSubpathAt(t, cache, baselineSHA, "skills/foo")

	// Modify all of them.
	files2 := map[string]string{}
	for i := 0; i < 20; i++ {
		files2[filepath.Join("skills/foo", "f"+itoa(i)+".txt")] = "v2\n"
	}
	addCommit(t, src, files2, "skills/foo: v2 many files")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}

	const maxBytes = 64
	d, err := Detect(context.Background(), &UpstreamRef{
		GitSubpath: "skills/foo",
		GitRef:     "origin/main",
	}, baselineSHA, baselineHash, cache, maxBytes)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Result != ResultDrift {
		t.Fatalf("Result = %q, want %q", d.Result, ResultDrift)
	}
	if !strings.HasSuffix(d.DiffSummary, "\n... (truncated)") {
		t.Fatalf("expected truncation suffix, got: %q", d.DiffSummary)
	}
	// Allow up to maxBytes + suffix length.
	const suffix = "\n... (truncated)"
	if len(d.DiffSummary) > maxBytes+len(suffix) {
		t.Fatalf("DiffSummary length = %d, expected <= %d", len(d.DiffSummary), maxBytes+len(suffix))
	}
}

// TestDetect_FirstCheckNoBaseline exercises the empty-lastSeenSHA path: a
// caller with no prior baseline should always get a Drift result, with the
// empty-tree sentinel in diffRange producing a non-empty ChangedFiles list
// (every file at HEAD:subpath is "added").
func TestDetect_FirstCheckNoBaseline(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1\n"}, "skills/foo: v1")

	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}

	d, err := Detect(context.Background(), &UpstreamRef{
		GitSubpath: "skills/foo",
		GitRef:     "HEAD",
	}, "" /*lastSeenSHA*/, "" /*lastSeenHash*/, cache, 8192)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if d.Result != ResultDrift {
		t.Fatalf("Result = %q, want %q", d.Result, ResultDrift)
	}
	if d.NewSHA == "" {
		t.Fatalf("NewSHA empty, want populated")
	}
	if d.NewHash == "" {
		t.Fatalf("NewHash empty, want populated")
	}
	if len(d.ChangedFiles) == 0 {
		t.Fatalf("ChangedFiles empty, want at least one (empty-tree sentinel should list every file as added)")
	}
	if !strings.Contains(strings.Join(d.ChangedFiles, "\n"), "skills/foo/SKILL.md") {
		t.Fatalf("ChangedFiles missing SKILL.md: %v", d.ChangedFiles)
	}
}

// itoa is a minimal int-to-string helper to avoid importing strconv just for tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
