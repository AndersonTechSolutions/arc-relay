// Package sync — skill installation and reconciliation.
//
// Skills are installed into <skillsDir>/<slug>/, where <skillsDir> is
// ~/.claude/skills/ on macOS/Linux. Each managed directory contains a
// .arc-sync-version file recording the installed version + slug + relay base
// URL — the marker is what distinguishes arc-sync-managed skills from
// hand-installed ones, so `skill sync` only touches its own.
package sync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/comma-compliance/arc-relay/internal/cli/relay"
)

// SkillMarkerFile lives at the root of each arc-sync-managed skill directory.
// Its presence + contents distinguish arc-sync skills from hand-installed
// ones: `skill sync` will refuse to remove a directory that lacks this file.
const SkillMarkerFile = ".arc-sync-version"

// MaxSkillArchiveBytes mirrors skills.MaxArchiveSize on the relay. Defined
// here to avoid pulling internal/skills (which transitively pulls
// internal/store + go-sqlite3 → CGO).
const MaxSkillArchiveBytes = 5 * 1024 * 1024

// SkillMarker is the JSON shape of .arc-sync-version. SHA256 + RelayURL let
// `sync` decide whether the local install is still in sync with the relay.
type SkillMarker struct {
	Slug     string `json:"slug"`
	Version  string `json:"version"`
	SHA256   string `json:"sha256"`
	RelayURL string `json:"relay_url"`
}

// InstalledSkill is one row in the local view, derived from SkillMarker plus
// any free-form skills that arc-sync didn't manage (Managed=false; we
// won't touch those during sync).
type InstalledSkill struct {
	Slug    string
	Version string // empty if not managed by arc-sync
	Path    string
	Managed bool
}

// SkillSyncReport summarizes the actions a sync run took (or would take in
// dry-run mode). Used by both the CLI human output and any future JSON output.
type SkillSyncReport struct {
	Installed   []SkillSyncAction `json:"installed"`
	Updated     []SkillSyncAction `json:"updated"`
	Removed     []SkillSyncAction `json:"removed"`
	Unchanged   []SkillSyncAction `json:"unchanged"`
	SkippedHand []SkillSyncAction `json:"skipped_hand_installed"`
	Errors      []SkillSyncError  `json:"errors,omitempty"`
}

// SkillSyncAction is one action in a sync run.
type SkillSyncAction struct {
	Slug     string `json:"slug"`
	Version  string `json:"version,omitempty"`
	Previous string `json:"previous,omitempty"` // for updates: the version we replaced
}

// SkillSyncError captures a per-skill failure without aborting the whole run.
type SkillSyncError struct {
	Slug    string `json:"slug"`
	Message string `json:"message"`
}

// SkillSyncOptions controls a sync run.
type SkillSyncOptions struct {
	DryRun bool
}

// SkillManager bundles everything the skill subcommands need: the relay HTTP
// client + the local skills root.
type SkillManager struct {
	Client    *relay.Client
	SkillsDir string
}

// DefaultSkillsDir returns ~/.claude/skills, the canonical Claude Code skill
// directory. Override only in tests.
func DefaultSkillsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// ListInstalled returns one entry per directory under SkillsDir. Missing
// SkillsDir is not an error — we return an empty slice. Directories without
// the marker file are returned with Managed=false.
func (m *SkillManager) ListInstalled() ([]InstalledSkill, error) {
	entries, err := os.ReadDir(m.SkillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading skills dir: %w", err)
	}
	out := make([]InstalledSkill, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(m.SkillsDir, e.Name())
		marker, err := readMarker(path)
		if err != nil && !os.IsNotExist(err) {
			out = append(out, InstalledSkill{Slug: e.Name(), Path: path})
			continue
		}
		if marker == nil {
			out = append(out, InstalledSkill{Slug: e.Name(), Path: path})
			continue
		}
		out = append(out, InstalledSkill{
			Slug: marker.Slug, Version: marker.Version,
			Path: path, Managed: true,
		})
	}
	return out, nil
}

// Install pulls (slug, version) from the relay and writes it to
// <SkillsDir>/<slug>/. Existing managed installs are replaced atomically
// (write to a sibling temp dir, swap, remove the old). Refuses to overwrite
// a directory that doesn't carry the arc-sync marker — that's hand-installed
// content.
func (m *SkillManager) Install(slug, version string) (*SkillMarker, error) {
	dest := filepath.Join(m.SkillsDir, slug)
	existing, err := readMarker(dest)
	if err != nil && !os.IsNotExist(err) {
		// Marker file unreadable / corrupt — assume hand-managed.
		return nil, fmt.Errorf("skill %q exists but %s is unreadable: %w (refusing to overwrite — back it up and rerun)",
			slug, SkillMarkerFile, err)
	}
	if _, err := os.Stat(dest); err == nil && existing == nil {
		return nil, fmt.Errorf("skill %q exists at %s but is not arc-sync-managed (no %s marker); not touching it",
			slug, dest, SkillMarkerFile)
	}

	archive, sha, err := m.Client.DownloadSkillVersion(slug, version)
	if err != nil {
		return nil, fmt.Errorf("download %s@%s: %w", slug, version, err)
	}
	if int64(len(archive)) > MaxSkillArchiveBytes {
		// Defense in depth — relay caps too, but a misbehaving relay shouldn't
		// blow our memory.
		return nil, fmt.Errorf("archive %d bytes exceeds %d cap", len(archive), MaxSkillArchiveBytes)
	}
	if sha != "" {
		gotSum := sha256.Sum256(archive)
		if hex.EncodeToString(gotSum[:]) != sha {
			return nil, fmt.Errorf("archive sha256 mismatch — got %x, expected %s", gotSum, sha)
		}
	}

	if err := os.MkdirAll(m.SkillsDir, 0o755); err != nil {
		return nil, fmt.Errorf("ensure skills dir: %w", err)
	}
	tmpDir, err := os.MkdirTemp(m.SkillsDir, ".arc-sync-install-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	// Called through a wrapper so the success path's reassignment of cleanup
	// is honoured. `defer cleanup()` would capture the current function value
	// and keep removing tmpDir even after it was renamed into place.
	defer func() { cleanup() }()

	if err := extractTarGz(archive, tmpDir); err != nil {
		return nil, fmt.Errorf("extract archive: %w", err)
	}

	marker := &SkillMarker{
		Slug:     slug,
		Version:  version,
		SHA256:   sha,
		RelayURL: m.Client.BaseURL,
	}
	if err := writeMarker(tmpDir, marker); err != nil {
		return nil, fmt.Errorf("write marker: %w", err)
	}

	// Atomic swap: rename old dest aside, rename tmp to dest, then remove old.
	// This minimizes the window where a process running in parallel sees a
	// partially-extracted directory.
	if existing != nil {
		stale := dest + ".arc-sync-stale"
		_ = os.RemoveAll(stale)
		if err := os.Rename(dest, stale); err != nil {
			return nil, fmt.Errorf("rename old: %w", err)
		}
		if err := os.Rename(tmpDir, dest); err != nil {
			// Try to roll back the previous install.
			_ = os.Rename(stale, dest)
			return nil, fmt.Errorf("rename new: %w", err)
		}
		_ = os.RemoveAll(stale)
	} else {
		if err := os.Rename(tmpDir, dest); err != nil {
			return nil, fmt.Errorf("rename: %w", err)
		}
	}
	cleanup = func() {} // tmpDir was renamed into place
	return marker, nil
}

// Remove deletes <SkillsDir>/<slug>/ if and only if the directory is
// arc-sync-managed (has the marker file). Returns nil when the slug isn't
// installed (idempotent removal).
func (m *SkillManager) Remove(slug string) error {
	dest := filepath.Join(m.SkillsDir, slug)
	marker, err := readMarker(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading marker for %q: %w", slug, err)
	}
	if marker == nil {
		return fmt.Errorf("skill %q at %s is not arc-sync-managed (no %s marker); refusing to remove",
			slug, dest, SkillMarkerFile)
	}
	return os.RemoveAll(dest)
}

// Sync reconciles the local skills directory against the relay's
// /api/skills/assigned response. Skills missing locally are installed; skills
// with a different version are updated; skills present locally but no longer
// assigned (by arc-sync) are removed. Hand-installed directories are
// reported as skipped — never touched.
func (m *SkillManager) Sync(opts SkillSyncOptions) (*SkillSyncReport, error) {
	assigned, err := m.Client.ListAssignedSkills()
	if err != nil {
		return nil, fmt.Errorf("list assigned: %w", err)
	}
	installed, err := m.ListInstalled()
	if err != nil {
		return nil, fmt.Errorf("list installed: %w", err)
	}

	report := &SkillSyncReport{}
	installedBySlug := map[string]InstalledSkill{}
	for _, i := range installed {
		installedBySlug[i.Slug] = i
	}

	desired := map[string]bool{}
	for _, a := range assigned {
		if a.Skill == nil {
			continue
		}
		slug := a.Skill.Slug
		desired[slug] = true
		wantVersion := a.Skill.LatestVersion
		if a.PinnedVersion != nil && *a.PinnedVersion != "" {
			wantVersion = *a.PinnedVersion
		}
		if wantVersion == "" {
			report.Errors = append(report.Errors, SkillSyncError{
				Slug:    slug,
				Message: "relay reports no published version; skipping",
			})
			continue
		}

		cur, isInstalled := installedBySlug[slug]
		if isInstalled && !cur.Managed {
			report.SkippedHand = append(report.SkippedHand, SkillSyncAction{Slug: slug})
			continue
		}
		if isInstalled && cur.Version == wantVersion {
			report.Unchanged = append(report.Unchanged, SkillSyncAction{Slug: slug, Version: wantVersion})
			continue
		}

		if opts.DryRun {
			if isInstalled {
				report.Updated = append(report.Updated, SkillSyncAction{
					Slug: slug, Version: wantVersion, Previous: cur.Version,
				})
			} else {
				report.Installed = append(report.Installed, SkillSyncAction{Slug: slug, Version: wantVersion})
			}
			continue
		}

		previous := cur.Version
		if _, err := m.Install(slug, wantVersion); err != nil {
			report.Errors = append(report.Errors, SkillSyncError{Slug: slug, Message: err.Error()})
			continue
		}
		if isInstalled {
			report.Updated = append(report.Updated, SkillSyncAction{
				Slug: slug, Version: wantVersion, Previous: previous,
			})
		} else {
			report.Installed = append(report.Installed, SkillSyncAction{Slug: slug, Version: wantVersion})
		}
	}

	// Anything we manage but the relay no longer assigns: remove it.
	for slug, cur := range installedBySlug {
		if desired[slug] {
			continue
		}
		if !cur.Managed {
			// Hand-installed skill not in the assigned list — leave it alone.
			report.SkippedHand = append(report.SkippedHand, SkillSyncAction{Slug: slug})
			continue
		}
		if opts.DryRun {
			report.Removed = append(report.Removed, SkillSyncAction{Slug: slug, Version: cur.Version})
			continue
		}
		if err := m.Remove(slug); err != nil {
			report.Errors = append(report.Errors, SkillSyncError{Slug: slug, Message: err.Error()})
			continue
		}
		report.Removed = append(report.Removed, SkillSyncAction{Slug: slug, Version: cur.Version})
	}
	return report, nil
}

// readMarker reads .arc-sync-version inside dir. Returns (nil, nil) if the
// directory exists but the marker file does not — i.e. hand-installed skill.
// Returns (nil, os.ErrNotExist wrapped) if the directory itself is missing.
func readMarker(dir string) (*SkillMarker, error) {
	path := filepath.Join(dir, SkillMarkerFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Did the directory itself exist? If yes → hand-installed; nil/nil.
			// If no → propagate ErrNotExist so callers can detect "not present".
			if _, statErr := os.Stat(dir); statErr == nil {
				return nil, nil
			}
			return nil, err
		}
		return nil, err
	}
	var marker SkillMarker
	if err := json.Unmarshal(b, &marker); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &marker, nil
}

// writeMarker writes a SkillMarker to .arc-sync-version inside dir.
func writeMarker(dir string, marker *SkillMarker) error {
	b, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, SkillMarkerFile), b, 0o600)
}

// extractTarGz extracts a gzipped tar archive into dest. Refuses any entry
// that would escape dest via path traversal. The dest dir is expected to
// already exist.
func extractTarGz(archive []byte, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		clean := filepath.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if !safeRelPath(clean) {
			return fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		target := filepath.Join(dest, clean)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode(hdr.Mode, 0o755)); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		// TypeRegA is not listed: archive/tar has normalized it to TypeReg
		// on read since Go 1.11, and the constant is deprecated.
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode(hdr.Mode, 0o644))
			if err != nil {
				return fmt.Errorf("open %s: %w", target, err)
			}
			lim := io.LimitReader(tr, MaxSkillArchiveBytes)
			if _, err := io.Copy(f, lim); err != nil {
				_ = f.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", target, err)
			}
		default:
			// Skip symlinks, devices, fifos, etc. — skill bundles shouldn't
			// contain them, and silently dropping them is safer than letting
			// them through.
			continue
		}
	}
	return nil
}

// PackageSkill walks dir and returns a gzipped tarball + the slug parsed from
// the SKILL.md frontmatter. The slug is whatever the caller's `name:` field is
// — server-side validation will reject mismatches if the operator passes a
// different --slug elsewhere. Excludes hidden files, including the
// .arc-sync-version marker (so re-publishing an installed skill doesn't carry
// install-side state into the archive).
func PackageSkill(dir string) (archive []byte, slug string, err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, "", fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("%s is not a directory", dir)
	}
	skillMD, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return nil, "", fmt.Errorf("reading SKILL.md: %w", err)
	}
	slug = parseFrontmatterName(skillMD)
	if slug == "" {
		return nil, "", fmt.Errorf("SKILL.md frontmatter missing required `name:` field")
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Skip hidden files (e.g. .git, .DS_Store, .arc-sync-version).
		base := filepath.Base(rel)
		if strings.HasPrefix(base, ".") {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return fmt.Errorf("tar header for %s: %w", rel, err)
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			// #nosec G122 -- packaging the operator's own local skill directory for
			// upload; a TOCTOU here would need a local attacker racing the user's own
			// filesystem, which is not a boundary this tool defends.
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()
		}
		return nil
	})
	if walkErr != nil {
		return nil, "", fmt.Errorf("packaging %s: %w", dir, walkErr)
	}
	if err := tw.Close(); err != nil {
		return nil, "", fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, "", fmt.Errorf("close gzip: %w", err)
	}
	return buf.Bytes(), slug, nil
}

// parseFrontmatterName extracts the `name:` field from a SKILL.md's YAML
// frontmatter without pulling in a YAML parser. The regex looks for the value
// on the same line as the key (most skills are written this way; multiline
// folded scalars for `name` would be unusual).
func parseFrontmatterName(skillMD []byte) string {
	s := string(skillMD)
	end := strings.Index(s, "\n---")
	if !strings.HasPrefix(s, "---") || end < 0 {
		return ""
	}
	frontmatter := s[3 : end+1]
	for _, line := range strings.Split(frontmatter, "\n") {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "name:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(trim, "name:"))
		val = strings.Trim(val, "\"'")
		return val
	}
	return ""
}

// safeRelPath rejects paths that escape the destination via .. or absolute
// roots. filepath.Clean has already been applied by the caller.
//
// Backslashes are treated as separators on every platform, not just Windows.
// filepath.Clean (applied by the caller) is platform-specific and only folds
// `\` on Windows, so checking the raw value against "../" let `..\escape.txt`
// through everywhere else — which is why this reproduced solely on the Windows
// CI runner. Deciding it here rather than deferring to filepath keeps the
// verdict independent of GOOS: the same archive is judged the same way on
// every platform, and tar's only defined separator is "/", so a backslash in
// an entry name is anomalous regardless of who extracts it.
func safeRelPath(p string) bool {
	if p == "" || p == "." {
		return false
	}
	if filepath.IsAbs(p) {
		return false
	}
	s := strings.ReplaceAll(p, `\`, "/")
	if strings.HasPrefix(s, "/") {
		return false
	}
	if s == ".." || strings.HasPrefix(s, "../") || strings.Contains(s, "/../") {
		return false
	}
	return true
}

// mode picks an os.FileMode for an extracted file. tar.Header.Mode is int64
// with extra metadata bits we don't want to leak through. Falls back to
// fallback if the header value is zero or out of range.
func mode(headerMode int64, fallback os.FileMode) os.FileMode {
	if headerMode == 0 {
		return fallback
	}
	// #nosec G115 -- the conversion is immediately masked with & os.ModePerm,
	// so any int64 the tar header carries — negative or oversized — yields a
	// value in [0,0777]. setuid/setgid/sticky cannot survive the mask.
	m := os.FileMode(headerMode) & os.ModePerm
	if m == 0 {
		return fallback
	}
	return m
}
