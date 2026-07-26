package checker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeTestRepo creates a fresh temp git repo with one commit on main.
// Returns the absolute path to the working tree.
func makeTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	gitRun(t, dir, "add", ".")
	gitCommit(t, dir, "init")
	return dir
}

// gitRun runs `git <args...>` in dir and fatals on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	gitRun(t, dir, "commit", "-m", msg)
}

// addCommit writes (or overwrites) the given files in repoDir, stages them,
// and commits with msg. Returns the resulting HEAD SHA.
func addCommit(t *testing.T, repoDir string, files map[string]string, msg string) string {
	t.Helper()
	for rel, contents := range files {
		full := filepath.Join(repoDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	gitRun(t, repoDir, "add", ".")
	gitCommit(t, repoDir, msg)
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// fileURL returns a file:// URL for a local repo (so `git clone` is happy).
func fileURL(p string) string {
	return "file://" + p
}

// ------------------------------------------------------------------
// EnsureCache
// ------------------------------------------------------------------

func TestEnsureCache_FreshClone(t *testing.T) {
	src := makeTestRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")

	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, ".git")); err != nil {
		t.Fatalf(".git not found after clone: %v", err)
	}
	out, err := exec.Command("git", "-C", cache, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	if strings.TrimSpace(string(out)) == "" {
		t.Fatalf("expected HEAD SHA, got empty")
	}
}

func TestEnsureCache_ReFetch(t *testing.T) {
	src := makeTestRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")

	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("first EnsureCache: %v", err)
	}
	beforeSHA := mustHEAD(t, cache)

	// Add a new commit upstream.
	newSHA := addCommit(t, src, map[string]string{"b.txt": "new\n"}, "second")

	// Re-running EnsureCache should fetch (not re-clone) and pull in the new commit.
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("second EnsureCache: %v", err)
	}

	// HEAD itself doesn't move on a bare-mirror-style fetch into a non-mirror clone,
	// but we should at least be able to resolve the upstream ref.
	got, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("ResolveSHA origin/main: %v", err)
	}
	if got != newSHA {
		t.Fatalf("after fetch, origin/main = %s, want %s", got, newSHA)
	}
	_ = beforeSHA
}

func TestEnsureCache_RecoversFromCorruption(t *testing.T) {
	src := makeTestRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")

	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("first EnsureCache: %v", err)
	}

	// Corrupt: scribble garbage over HEAD so subsequent git commands fail.
	if err := os.WriteFile(filepath.Join(cache, ".git", "HEAD"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatalf("damage HEAD: %v", err)
	}
	// Also nuke a pack ref to make fetch unhappy.
	if err := os.RemoveAll(filepath.Join(cache, ".git", "refs")); err != nil {
		t.Fatalf("rm refs: %v", err)
	}

	// Should recover by re-cloning.
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("recovery EnsureCache: %v", err)
	}
	if _, err := exec.Command("git", "-C", cache, "rev-parse", "HEAD").Output(); err != nil {
		t.Fatalf("git rev-parse HEAD failed after recovery: %v", err)
	}
}

func TestEnsureCache_CtxCancelled(t *testing.T) {
	src := makeTestRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before we try anything

	if err := EnsureCache(ctx, cache, fileURL(src)); err == nil {
		t.Fatalf("expected error from cancelled ctx, got nil")
	}
}

// ------------------------------------------------------------------
// ResolveSHA
// ------------------------------------------------------------------

func TestResolveSHA_HEAD(t *testing.T) {
	src := makeTestRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}

	got, err := ResolveSHA(context.Background(), cache, "")
	if err != nil {
		t.Fatalf("ResolveSHA: %v", err)
	}
	if !looksLikeSHA(got) {
		t.Fatalf("expected SHA-shaped string, got %q", got)
	}

	got2, err := ResolveSHA(context.Background(), cache, "HEAD")
	if err != nil {
		t.Fatalf("ResolveSHA HEAD: %v", err)
	}
	if got != got2 {
		t.Fatalf("HEAD vs empty mismatch: %s vs %s", got, got2)
	}
}

func TestResolveSHA_Branch(t *testing.T) {
	src := makeTestRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}

	got, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("ResolveSHA origin/main: %v", err)
	}
	if !looksLikeSHA(got) {
		t.Fatalf("expected SHA, got %q", got)
	}
}

func TestResolveSHA_CommitSHA(t *testing.T) {
	src := makeTestRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	head, err := ResolveSHA(context.Background(), cache, "HEAD")
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}

	got, err := ResolveSHA(context.Background(), cache, head)
	if err != nil {
		t.Fatalf("ResolveSHA(sha): %v", err)
	}
	if got != head {
		t.Fatalf("rev-parse(sha)=%s, want %s", got, head)
	}
}

func TestResolveSHA_UnknownRef(t *testing.T) {
	src := makeTestRepo(t)
	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}

	if _, err := ResolveSHA(context.Background(), cache, "no-such-ref-zzz"); err == nil {
		t.Fatalf("expected error for unknown ref, got nil")
	}
}

// ------------------------------------------------------------------
// LogPath
// ------------------------------------------------------------------

func TestLogPath_NoCommitsTouchingSubpath(t *testing.T) {
	src := makeTestRepo(t)
	// Add a top-level commit that doesn't touch skills/foo.
	addCommit(t, src, map[string]string{"top.txt": "x"}, "top-level only")
	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	to, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve to: %v", err)
	}

	lines, err := LogPath(context.Background(), cache, "", to, "skills/foo")
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected 0 lines, got %d: %v", len(lines), lines)
	}
}

func TestLogPath_ReturnsTouchedCommits(t *testing.T) {
	src := makeTestRepo(t)
	// commit 1: add skills/foo/SKILL.md
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1"}, "skills/foo: v1")
	// commit 2: unrelated change
	addCommit(t, src, map[string]string{"unrelated.txt": "x"}, "unrelated")
	// commit 3: bump skills/foo
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v2"}, "skills/foo: v2")

	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	to, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	lines, err := LogPath(context.Background(), cache, "", to, "skills/foo")
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	// Two commits touch skills/foo (v1 and v2).
	if len(lines) != 2 {
		t.Fatalf("expected 2 commits touching skills/foo, got %d: %v", len(lines), lines)
	}
}

func TestLogPath_FromToRange(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v1"}, "skills/foo: v1")

	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	first, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve first: %v", err)
	}

	// Add another commit touching the subpath, then re-fetch.
	addCommit(t, src, map[string]string{"skills/foo/SKILL.md": "v2"}, "skills/foo: v2")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	second, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve second: %v", err)
	}

	lines, err := LogPath(context.Background(), cache, first, second, "skills/foo")
	if err != nil {
		t.Fatalf("LogPath range: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 commit in range, got %d: %v", len(lines), lines)
	}
}

// ------------------------------------------------------------------
// CheckoutSubpath
// ------------------------------------------------------------------

func TestCheckoutSubpath_ExtractsContents(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{
		"a/b/file.txt": "hello",
		"a/b/sub/x.md": "x",
		"c/d.txt":      "should not be in dest",
	}, "fixture")

	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	sha, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "dest")
	if err := CheckoutSubpath(context.Background(), cache, sha, "a/b", dest); err != nil {
		t.Fatalf("CheckoutSubpath: %v", err)
	}

	// dest/file.txt and dest/sub/x.md should exist; dest/c/d.txt should not.
	if b, err := os.ReadFile(filepath.Join(dest, "file.txt")); err != nil {
		t.Fatalf("expected dest/file.txt: %v", err)
	} else if string(b) != "hello" {
		t.Fatalf("file.txt = %q, want %q", b, "hello")
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", "x.md")); err != nil {
		t.Fatalf("expected dest/sub/x.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "c", "d.txt")); !os.IsNotExist(err) {
		t.Fatalf("dest/c/d.txt should not exist; stat err=%v", err)
	}
	// Also: there should not be a leading a/b/ in the dest.
	if _, err := os.Stat(filepath.Join(dest, "a", "b", "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("dest/a/b/file.txt should not exist (subpath prefix should be stripped); stat err=%v", err)
	}
}

func TestCheckoutSubpath_EmptySubpathExtractsFullTree(t *testing.T) {
	src := makeTestRepo(t)
	addCommit(t, src, map[string]string{
		"a.txt":      "alpha",
		"dir1/b.txt": "beta",
	}, "fixture")

	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	sha, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "dest")
	if err := CheckoutSubpath(context.Background(), cache, sha, "", dest); err != nil {
		t.Fatalf("CheckoutSubpath: %v", err)
	}

	if b, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil {
		t.Fatalf("expected dest/a.txt: %v", err)
	} else if string(b) != "alpha" {
		t.Fatalf("a.txt = %q, want %q", b, "alpha")
	}
	if b, err := os.ReadFile(filepath.Join(dest, "dir1", "b.txt")); err != nil {
		t.Fatalf("expected dest/dir1/b.txt: %v", err)
	} else if string(b) != "beta" {
		t.Fatalf("dir1/b.txt = %q, want %q", b, "beta")
	}
}

// TestLogPath_FullHistoryToSHA covers the fromSHA="" + toSHA != "" branch:
// it should return every commit reachable from toSHA that touches the
// subpath (i.e. full history, not a range).
func TestLogPath_FullHistoryToSHA(t *testing.T) {
	src := makeTestRepo(t)
	// commit 2: only this one touches subpath/
	addCommit(t, src, map[string]string{"unrelated1.txt": "x"}, "c1 unrelated")
	addCommit(t, src, map[string]string{"subpath/file.txt": "v1"}, "c2 touches subpath")
	addCommit(t, src, map[string]string{"unrelated2.txt": "y"}, "c3 unrelated")

	cache := filepath.Join(t.TempDir(), "cache")
	if err := EnsureCache(context.Background(), cache, fileURL(src)); err != nil {
		t.Fatalf("EnsureCache: %v", err)
	}
	head, err := ResolveSHA(context.Background(), cache, "origin/main")
	if err != nil {
		t.Fatalf("resolve head: %v", err)
	}

	lines, err := LogPath(context.Background(), cache, "", head, "subpath/")
	if err != nil {
		t.Fatalf("LogPath: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 commit touching subpath/, got %d: %v", len(lines), lines)
	}
}

// ------------------------------------------------------------------
// helpers
// ------------------------------------------------------------------

func mustHEAD(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD in %s: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

func looksLikeSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		isHexDigit := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHexDigit {
			return false
		}
	}
	return true
}
