// Package checker implements upstream-update detection for skills.
//
// This file wraps the local `git` binary (already present in every container the
// relay runs in) to manage a per-skill cache directory, fetch upstream, resolve
// refs, query path-touching commits, and extract subpath contents.
//
// Why shell out instead of using go-git? The plan explicitly forbids the
// dependency; the `git` binary is reliable, well-tested, and supports the
// `--filter=blob:none` partial-clone flag we need.
package checker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/store"
)

// EnsureCache makes sure cacheDir contains an up-to-date partial clone of
// gitURL.
//
// Behaviour:
//
//   - If cacheDir/.git does not exist, do a fresh clone with
//     `git clone --no-tags --filter=blob:none <url> <cacheDir>` (full commit
//     history, but blobs lazy-fetched on demand).
//   - If cacheDir/.git exists, run `git -C <cacheDir> fetch origin --prune
//     --no-tags` to pick up new commits/refs.
//   - On *any* failure from the above (bad fetch, corrupted repo, missing
//     pack, etc.) wipe cacheDir and re-clone exactly once. Disk is cheap;
//     trying to be clever with `git fsck` is not.
//
// Honours ctx for cancellation through exec.CommandContext.
func EnsureCache(ctx context.Context, cacheDir, gitURL string) error {
	if cacheDir == "" {
		return fmt.Errorf("EnsureCache: cacheDir must not be empty")
	}
	if gitURL == "" {
		return fmt.Errorf("EnsureCache: gitURL must not be empty")
	}
	// Re-checked here, not only at the write boundary, so a row that predates
	// validation (or arrives by some other path) still cannot reach git.
	if err := store.ValidateGitURL(gitURL); err != nil {
		return fmt.Errorf("EnsureCache: %w", err)
	}

	// Pre-check ctx so a cancelled context fails fast, even if no subcommand
	// would have failed (e.g. the no-op path where .git already exists and
	// fetch would have succeeded). Keeps test semantics tight.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("EnsureCache: %w", err)
	}

	if hasGitDir(cacheDir) {
		if err := gitFetch(ctx, cacheDir); err == nil {
			return nil
		}
		// Fetch failed: assume corruption, wipe, fall through to clone.
	}

	// Clone path (also the recovery path).
	if err := wipeAndClone(ctx, cacheDir, gitURL); err != nil {
		return err
	}
	return nil
}

// ResolveSHA returns the trimmed output of `git -C <cacheDir> rev-parse <ref>`.
// An empty ref defaults to HEAD.
func ResolveSHA(ctx context.Context, cacheDir, ref string) (string, error) {
	if cacheDir == "" {
		return "", fmt.Errorf("ResolveSHA: cacheDir must not be empty")
	}
	if ref == "" {
		ref = "HEAD"
	}
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("ResolveSHA: ref %q must not start with '-'", ref)
	}
	out, err := runGit(ctx, cacheDir, "rev-parse", "--verify", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// LogPath returns the lines from `git log <range> --oneline -- <subpath>` as a
// slice (one line per commit; empty slice if no commits touch the subpath).
//
// Range semantics:
//
//   - fromSHA == "" && toSHA == "" → full log of HEAD limited by subpath.
//   - fromSHA == "" && toSHA != "" → log of toSHA limited by subpath.
//   - fromSHA != "" && toSHA != "" → log of fromSHA..toSHA limited by subpath
//     (commits reachable from toSHA but not from fromSHA).
//   - fromSHA != "" && toSHA == "" → invalid; returns an error. Callers should
//     resolve a toSHA before asking.
func LogPath(ctx context.Context, cacheDir, fromSHA, toSHA, subpath string) ([]string, error) {
	if cacheDir == "" {
		return nil, fmt.Errorf("LogPath: cacheDir must not be empty")
	}
	if fromSHA != "" && toSHA == "" {
		return nil, fmt.Errorf("LogPath: fromSHA without toSHA is ambiguous")
	}

	args := []string{"log", "--oneline"}
	switch {
	case fromSHA != "" && toSHA != "":
		args = append(args, fmt.Sprintf("%s..%s", fromSHA, toSHA))
	case toSHA != "":
		args = append(args, toSHA)
	}
	// Subpath separator must come last so git knows what's a ref vs path.
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

// CheckoutSubpath extracts the contents of <cacheDir>:<sha>:<subpath> into
// destDir. If subpath is empty, the entire tree at <sha> is extracted.
//
// Implementation: `git -C cacheDir archive <sha> -- <subpath>` piped into
// `tar -x -C destDir --strip-components=N`, where N is the number of path
// segments in subpath. This is cleaner than `git checkout` into a working tree
// because it doesn't pollute cacheDir's index/HEAD and doesn't require us to
// move files post-hoc.
//
// destDir is created if missing. After this returns successfully, destDir
// contains exactly what was at <sha>:<subpath>/ — no leading subpath
// component, no sibling files.
//
// On error, destDir contents are undefined and may be partial or absent;
// callers should treat destDir as opaque-on-error and either delete it or use
// a fresh tempdir per call. (Implementation: on archive failure we kill tar
// and best-effort `os.RemoveAll(destDir)` to avoid silently retaining a
// partial extract.)
func CheckoutSubpath(ctx context.Context, cacheDir, sha, subpath, destDir string) error {
	if cacheDir == "" {
		return fmt.Errorf("CheckoutSubpath: cacheDir must not be empty")
	}
	if sha == "" {
		return fmt.Errorf("CheckoutSubpath: sha must not be empty")
	}
	if destDir == "" {
		return fmt.Errorf("CheckoutSubpath: destDir must not be empty")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("CheckoutSubpath: mkdir destDir: %w", err)
	}

	// Normalise subpath: strip leading/trailing slashes, collapse "./".
	clean := strings.Trim(filepath.ToSlash(subpath), "/")
	stripComponents := 0
	if clean != "" {
		stripComponents = strings.Count(clean, "/") + 1
	}

	archiveArgs := []string{"-C", cacheDir, "archive", "--format=tar", sha}
	if clean != "" {
		archiveArgs = append(archiveArgs, "--", clean)
	}
	// #nosec G204 -- binary is the literal "git"; the ref is a resolved SHA
	// and the subpath is normalised and passed after "--". No shell.
	archiveCmd := exec.CommandContext(ctx, "git", archiveArgs...)
	archiveCmd.Env = gitEnv()

	tarArgs := []string{"-x", "-C", destDir}
	if stripComponents > 0 {
		tarArgs = append(tarArgs, fmt.Sprintf("--strip-components=%d", stripComponents))
	}
	// #nosec G204 -- binary is the literal "tar"; every argument is internally
	// constructed (destDir and an integer --strip-components). No shell.
	tarCmd := exec.CommandContext(ctx, "tar", tarArgs...)

	// Wire git stdout → tar stdin via an os.Pipe so both processes stream
	// concurrently and neither has to buffer the whole archive in memory.
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("CheckoutSubpath: pipe: %w", err)
	}
	archiveCmd.Stdout = pw
	tarCmd.Stdin = pr

	var archiveErr, tarErr bytes.Buffer
	archiveCmd.Stderr = &archiveErr
	tarCmd.Stderr = &tarErr

	if err := tarCmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return fmt.Errorf("CheckoutSubpath: start tar: %w", err)
	}
	// tar inherited a dup of pr; the parent doesn't need it any more. Defer
	// so a panic between here and the end still releases the fd.
	defer func() { _ = pr.Close() }()

	if err := archiveCmd.Start(); err != nil {
		_ = pw.Close()
		_ = tarCmd.Process.Kill()
		_ = tarCmd.Wait()
		return fmt.Errorf("CheckoutSubpath: start git archive: %w", err)
	}

	// archiveCmd writes to pw; once it exits we close pw so tar sees EOF.
	// pw.Close() MUST be explicit (not deferred) because tar.Wait() blocks
	// until tar sees EOF on its stdin — a deferred close would deadlock since
	// defers run LIFO at function return, after tar.Wait().
	archiveWait := archiveCmd.Wait()
	if archiveWait != nil {
		// Archive failed mid-stream. tar may still be sitting in read(2) on
		// pr; kill it so it doesn't extract a partial archive into destDir
		// and so tar.Wait() doesn't hang. Then nuke destDir best-effort to
		// avoid leaving partial files for the caller to deal with.
		_ = tarCmd.Process.Kill()
		_ = tarCmd.Wait()
		_ = pw.Close()
		_ = os.RemoveAll(destDir)
		return fmt.Errorf("CheckoutSubpath: git archive %s -- %s: %w: %s",
			sha, clean, archiveWait, strings.TrimSpace(archiveErr.String()))
	}
	_ = pw.Close()
	tarWait := tarCmd.Wait()
	if tarWait != nil {
		_ = os.RemoveAll(destDir)
		return fmt.Errorf("CheckoutSubpath: tar extract: %w: %s",
			tarWait, strings.TrimSpace(tarErr.String()))
	}
	return nil
}

// ------------------------------------------------------------------
// internals
// ------------------------------------------------------------------

func hasGitDir(cacheDir string) bool {
	st, err := os.Stat(filepath.Join(cacheDir, ".git"))
	if err != nil {
		return false
	}
	return st.IsDir()
}

func gitFetch(ctx context.Context, cacheDir string) error {
	_, err := runGit(ctx, cacheDir, "fetch", "origin", "--prune", "--no-tags")
	return err
}

func wipeAndClone(ctx context.Context, cacheDir, gitURL string) error {
	if err := os.RemoveAll(cacheDir); err != nil {
		return fmt.Errorf("EnsureCache: wipe %s: %w", cacheDir, err)
	}
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		return fmt.Errorf("EnsureCache: mkdir parent of %s: %w", cacheDir, err)
	}
	// #nosec G204 -- binary is the literal "git" and the URL is passed after
	// "--" so it cannot be read as a flag. store.ValidateGitURL rejects
	// transport helpers before the row is stored, and gitEnv pins
	// GIT_ALLOW_PROTOCOL so ext:: cannot execute a command. No shell.
	cmd := exec.CommandContext(ctx, "git",
		"clone", "--no-tags", "--filter=blob:none",
		"--", gitURL, cacheDir,
	)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("EnsureCache: git clone %s: %w: %s",
			gitURL, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runGit executes `git -C <cacheDir> <args...>` with our hardened env and
// returns stdout. On non-zero exit, the error message includes the args, exit
// status, and the captured stderr.
func runGit(ctx context.Context, cacheDir string, args ...string) (string, error) {
	full := append([]string{"-C", cacheDir}, args...)
	// #nosec G204 -- binary is the literal "git"; args are internal literals
	// plus cacheDir, which the relay derives from a hash. No shell.
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s",
			strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// gitEnv returns the environment for git subprocesses: inherits the parent's
// env plus GIT_TERMINAL_PROMPT=0 (no interactive prompts on auth failure),
// neutralised global/system gitconfig (so a developer's local config can't
// affect the relay), and an explicit transport allowlist.
//
// GIT_ALLOW_PROTOCOL pins the set of transports rather than relying on git's
// defaults. Current git already refuses ext:: (which would run an arbitrary
// command as the relay user) unless it is enabled, but that is a default we
// would rather not depend on: the upstream URL is caller-supplied, and a
// downgrade to an older git or a future default change should not turn into
// code execution here. Naming the transports makes the guarantee ours.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_ALLOW_PROTOCOL="+strings.Join(store.AllowedGitProtocols, ":"),
	)
}
