// Package skills implements the skill repository service: archive validation,
// disk I/O for bundle storage, and orchestration on top of store.SkillStore.
//
// Archives are gzipped tarballs containing at least a SKILL.md at the root with
// YAML frontmatter. The service validates the archive shape on upload, computes
// SHA-256 for integrity, persists the file under <bundlesDir>/<slug>/<version>.tar.gz,
// and records a row in skill_versions.
package skills

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
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/comma-compliance/arc-relay/internal/skills/subhash"
	"github.com/comma-compliance/arc-relay/internal/store"
)

// MaxArchiveSize caps how large an uploaded skill bundle can be. Enforced at
// the service layer; HTTP handlers should also wrap request bodies with
// http.MaxBytesReader so the cap is enforced before fully reading a hostile
// upload.
const MaxArchiveSize = 5 * 1024 * 1024 // 5 MiB

// ErrInvalidArchive is the umbrella error returned for any archive-shape failure
// (missing SKILL.md, bad frontmatter, slug mismatch, path traversal, oversize).
// Service callers should surface this as a 400 to clients.
var ErrInvalidArchive = errors.New("invalid skill archive")

// ErrSkillNotFound is returned when a skill or specific version is not found.
var ErrSkillNotFound = errors.New("skill not found")

// frontmatterPattern extracts the YAML frontmatter block at the top of a
// SKILL.md file. The leading `---` must be on line 1 (no BOM, no leading blanks)
// and the closing `---` must be on its own line. We allow CRLF line endings
// because Windows-authored skill files are a real possibility.
var frontmatterPattern = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n`)

// versionPattern is a deliberately strict semver-ish check. We require
// MAJOR.MINOR.PATCH to keep version comparisons trivial across CLI/UI and to
// rule out accidental tag-style versions ("v1", "alpha"). Pre-release suffixes
// can be added later if real demand appears.
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$`)

// Manifest is the structured form of the SKILL.md frontmatter we persist
// alongside each version. Only Name + Description are typed — the rest is
// preserved verbatim in Extra so the upstream SKILL.md schema can evolve
// (e.g. argument-hint accepting a string OR a flow sequence) without breaking
// older relays.
type Manifest struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// Service is the skill repository service.
type Service struct {
	store      *store.SkillStore
	bundlesDir string
}

// New constructs a Service. bundlesDir is created (mode 0700) on first use if
// it does not already exist; archives are written under it as
// <slug>/<version>.tar.gz.
func New(s *store.SkillStore, bundlesDir string) *Service {
	return &Service{store: s, bundlesDir: bundlesDir}
}

// BundlesDir returns the directory the service writes archives into. Useful
// for tests and for surfacing in /api/skills/stats.
func (s *Service) BundlesDir() string { return s.bundlesDir }

// UploadInput is what an upload handler hands to the service after auth.
// SlugOverride lets the caller force a slug (e.g. when the URL says
// /api/skills/{slug}/versions); when empty, the slug is taken from the
// frontmatter's `name` field.
type UploadInput struct {
	SlugOverride string
	Version      string
	Archive      []byte
	UploadedBy   string // user id, for audit trail
	DisplayName  string // optional override; defaults to manifest description's first line or slug
	Description  string // optional; defaults to manifest description
	Visibility   string // "public" or "restricted"; defaults to "restricted"
}

// UploadResult bundles the persisted skill + version rows for the caller.
type UploadResult struct {
	Skill    *store.Skill        `json:"skill"`
	Version  *store.SkillVersion `json:"version"`
	Manifest *Manifest           `json:"manifest"`
}

// Upload validates an archive, persists it on disk, and inserts skill_versions.
// Auto-creates the skill row on first upload. Returns ErrInvalidArchive on any
// validation failure with a wrapped diagnostic message.
//
// The on-disk write is non-atomic across crashes (we write directly to the
// destination path), but the DB insert happens after the write succeeds, so
// the only failure mode is "archive file exists with no DB row" — those are
// reaped by Phase 1's GC pass and never served because nothing references them.
func (s *Service) Upload(in *UploadInput) (*UploadResult, error) {
	if len(in.Archive) == 0 {
		return nil, fmt.Errorf("%w: archive is empty", ErrInvalidArchive)
	}
	if int64(len(in.Archive)) > MaxArchiveSize {
		return nil, fmt.Errorf("%w: archive %d bytes exceeds limit of %d", ErrInvalidArchive, len(in.Archive), MaxArchiveSize)
	}
	if !versionPattern.MatchString(in.Version) {
		return nil, fmt.Errorf("%w: version %q must be MAJOR.MINOR.PATCH (got %q)", ErrInvalidArchive, in.Version, in.Version)
	}

	manifest, err := ValidateArchive(in.Archive)
	if err != nil {
		return nil, err
	}

	slug := in.SlugOverride
	if slug == "" {
		slug = manifest.Name
	}
	if err := store.ValidateSlug(slug); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArchive, err)
	}
	if manifest.Name != slug {
		return nil, fmt.Errorf("%w: frontmatter name %q does not match slug %q", ErrInvalidArchive, manifest.Name, slug)
	}

	sum := sha256.Sum256(in.Archive)
	sha := hex.EncodeToString(sum[:])

	skill, err := s.store.GetSkillBySlug(slug)
	if err != nil {
		return nil, fmt.Errorf("looking up skill: %w", err)
	}
	uploadedBy := nullableString(in.UploadedBy)
	displayName := strings.TrimSpace(in.DisplayName)
	if displayName == "" {
		displayName = firstNonEmptyLine(manifest.Description)
		if displayName == "" {
			displayName = slug
		}
	}
	visibility := in.Visibility
	if visibility == "" {
		visibility = "restricted"
	}
	description := strings.TrimSpace(in.Description)
	if description == "" {
		description = strings.TrimSpace(manifest.Description)
	}

	if skill == nil {
		skill = &store.Skill{
			Slug:        slug,
			DisplayName: displayName,
			Description: description,
			Visibility:  visibility,
			CreatedBy:   uploadedBy,
		}
		if err := s.store.CreateSkill(skill); err != nil {
			return nil, fmt.Errorf("creating skill: %w", err)
		}
	} else {
		// Refresh display_name + description on each upload so re-publishing a
		// skill with a clearer summary actually updates the listing. Visibility
		// is NOT updated here — flipping public/restricted should be an
		// explicit admin action, not a side effect of `arc-sync skill push`.
		if displayName != skill.DisplayName || description != skill.Description {
			if err := s.store.UpdateSkillMeta(skill.ID, displayName, description, ""); err != nil {
				return nil, fmt.Errorf("updating skill meta: %w", err)
			}
			skill.DisplayName = displayName
			skill.Description = description
		}
	}

	archivePath := filepath.Join(skill.Slug, in.Version+".tar.gz")
	if err := s.writeArchive(archivePath, in.Archive); err != nil {
		return nil, fmt.Errorf("writing archive: %w", err)
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encoding manifest: %w", err)
	}

	version := &store.SkillVersion{
		SkillID:       skill.ID,
		Version:       in.Version,
		ArchivePath:   archivePath,
		ArchiveSize:   int64(len(in.Archive)),
		ArchiveSHA256: sha,
		Manifest:      manifestJSON,
		UploadedBy:    uploadedBy,
	}
	if err := s.store.CreateVersion(version); err != nil {
		// On DB failure, leave the archive on disk for the GC reaper to clean up.
		// Removing it here on failure can race with another upload.
		return nil, fmt.Errorf("recording version: %w", err)
	}

	skill.LatestVersion = in.Version
	return &UploadResult{Skill: skill, Version: version, Manifest: manifest}, nil
}

// OpenArchive returns a ReadCloser over the on-disk archive for (skillID, version).
// The caller is responsible for closing it. Returns ErrSkillNotFound if the
// version row does not exist; returns os.ErrNotExist (wrapped) if the row
// exists but the file is missing on disk (operator should treat this as
// corruption — likely cleanup ran past a still-referenced archive).
func (s *Service) OpenArchive(skillID, version string) (io.ReadCloser, *store.SkillVersion, error) {
	v, err := s.store.GetVersion(skillID, version)
	if err != nil {
		return nil, nil, err
	}
	if v == nil {
		return nil, nil, ErrSkillNotFound
	}
	full := filepath.Join(s.bundlesDir, v.ArchivePath)
	f, err := os.Open(full)
	if err != nil {
		return nil, nil, fmt.Errorf("opening archive: %w", err)
	}
	return f, v, nil
}

// ResolveLatest returns the skill + the latest non-yanked version. Used by
// download endpoints when a client asks for "<slug>" without a version pin.
func (s *Service) ResolveLatest(slug string) (*store.Skill, *store.SkillVersion, error) {
	skill, err := s.store.GetSkillBySlug(slug)
	if err != nil {
		return nil, nil, err
	}
	if skill == nil || skill.YankedAt != nil {
		return nil, nil, ErrSkillNotFound
	}
	if skill.LatestVersion == "" {
		return skill, nil, ErrSkillNotFound
	}
	v, err := s.store.GetVersion(skill.ID, skill.LatestVersion)
	if err != nil {
		return nil, nil, err
	}
	if v == nil || v.YankedAt != nil {
		// Latest pointer is stale (e.g. version was yanked but pointer not
		// rolled back). Treat as no-published-version for now; Phase 1 GC
		// task will reconcile latest_version with the newest non-yanked row.
		return skill, nil, ErrSkillNotFound
	}
	return skill, v, nil
}

// ValidateArchive parses a gzipped tar archive in-memory and ensures it
// contains a SKILL.md at the root with valid YAML frontmatter. Returns the
// parsed manifest on success.
//
// Checks: gzip header, tar entries iterable, SKILL.md present, no entries
// referencing absolute or parent paths, frontmatter present + parseable as
// YAML, name field non-empty.
func ValidateArchive(archive []byte) (*Manifest, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("%w: not a gzip stream: %v", ErrInvalidArchive, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var skillMD []byte
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: tar read error: %v", ErrInvalidArchive, err)
		}
		// Reject path traversal in any entry name. tar headers can contain
		// arbitrary strings; we don't extract here, but Phase 1 install code
		// will, and a single relay-side check is the right place to fail fast.
		if !safeArchivePath(hdr.Name) {
			return nil, fmt.Errorf("%w: unsafe archive path %q", ErrInvalidArchive, hdr.Name)
		}
		// Match SKILL.md at root, allowing for an optional leading "./".
		clean := strings.TrimPrefix(hdr.Name, "./")
		if clean == "SKILL.md" && hdr.Typeflag == tar.TypeReg {
			// Clamp the read to MaxArchiveSize so a malicious header claiming
			// huge Size doesn't blow memory.
			limited := io.LimitReader(tr, MaxArchiveSize)
			b, err := io.ReadAll(limited)
			if err != nil {
				return nil, fmt.Errorf("%w: reading SKILL.md: %v", ErrInvalidArchive, err)
			}
			skillMD = b
		}
	}
	if skillMD == nil {
		return nil, fmt.Errorf("%w: SKILL.md not found at archive root", ErrInvalidArchive)
	}

	m := frontmatterPattern.FindSubmatch(skillMD)
	if m == nil {
		return nil, fmt.Errorf("%w: SKILL.md is missing YAML frontmatter", ErrInvalidArchive)
	}
	raw := map[string]any{}
	if err := yaml.Unmarshal(m[1], &raw); err != nil {
		return nil, fmt.Errorf("%w: frontmatter YAML invalid: %v", ErrInvalidArchive, err)
	}
	manifest := &Manifest{Extra: map[string]any{}}
	for k, v := range raw {
		switch k {
		case "name":
			if s, ok := v.(string); ok {
				manifest.Name = s
			} else {
				return nil, fmt.Errorf("%w: frontmatter name must be a string", ErrInvalidArchive)
			}
		case "description":
			if s, ok := v.(string); ok {
				manifest.Description = s
			}
			// Non-string descriptions are tolerated — they get stashed in Extra.
			if _, ok := v.(string); !ok {
				manifest.Extra[k] = v
			}
		default:
			manifest.Extra[k] = v
		}
	}
	if manifest.Name == "" {
		return nil, fmt.Errorf("%w: frontmatter name is required", ErrInvalidArchive)
	}
	if len(manifest.Extra) == 0 {
		manifest.Extra = nil
	}
	return manifest, nil
}

// writeArchive writes b under <bundlesDir>/<rel> with mode 0600 atomically:
// write to a sibling temp file, fsync, then rename. The parent directory is
// created with mode 0700 if missing.
func (s *Service) writeArchive(rel string, b []byte) error {
	full := filepath.Join(s.bundlesDir, rel)
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir bundle dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "upload-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	// Called through a wrapper so the success path's reassignment of cleanup
	// is honoured. `defer cleanup()` would capture the current function value
	// and keep removing tmpName even after it was renamed to its final path.
	defer func() { cleanup() }()

	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	cleanup = func() {} // success — the file is at `full` now
	return nil
}

// safeArchivePath returns true if name is a safe relative path that does not
// escape the destination directory when extracted.
func safeArchivePath(name string) bool {
	if name == "" {
		return false
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return false
	}
	return true
}

// firstNonEmptyLine returns the first line of s that has non-whitespace content,
// trimmed. Used to derive a default display_name from a long YAML description.
func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		trim := strings.TrimSpace(ln)
		if trim != "" {
			return trim
		}
	}
	return ""
}

// nullableString returns nil for "" and a pointer otherwise. Lets tests and
// callers leave the audit fields blank without writing zero-length strings.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ComputeSubtreeHashFromArchive extracts the gzipped tar archive into a
// temporary directory, computes the deterministic subtree hash via
// subhash.Hash, and returns the hex digest. The tempdir is removed before
// return on both success and failure.
//
// Used by the upstream checker to compute a relay-side hash from the latest
// published archive (so it can be persisted on the DriftReport) and by the
// upload handler to compute the post-upload hash that ClearDriftReport
// records as the new last_seen_hash baseline.
//
// Every entry's path is run through safeArchivePath; a single rejected entry
// fails the whole call rather than producing a partial extraction.
func (s *Service) ComputeSubtreeHashFromArchive(archive []byte) (string, error) {
	tmpDir, err := os.MkdirTemp("", "arc-relay-subhash-*")
	if err != nil {
		return "", fmt.Errorf("mkdtemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := extractTarGz(archive, tmpDir); err != nil {
		return "", err
	}
	return subhash.Hash(tmpDir)
}

// extractTarGz extracts a gzipped tar archive into destDir. Path-traversal
// entries (absolute paths, ".." components) are rejected via safeArchivePath
// — the same guard ValidateArchive applies during upload, repeated here so
// extraction is independently safe.
//
// Symlinks and hardlinks are skipped (not supported in skill bundles); only
// regular files and directories are materialised. The 5 MiB MaxArchiveSize
// is also enforced per-file via io.LimitReader so a malicious header
// claiming a huge Size cannot blow memory.
func extractTarGz(archive []byte, destDir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("%w: not a gzip stream: %v", ErrInvalidArchive, err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: tar read error: %v", ErrInvalidArchive, err)
		}
		if !safeArchivePath(hdr.Name) {
			return fmt.Errorf("%w: unsafe archive path %q", ErrInvalidArchive, hdr.Name)
		}
		clean := strings.TrimPrefix(hdr.Name, "./")
		// filepath.Join cleans "./" for us; safeArchivePath already rejected
		// any "../"-bearing entry so the result is guaranteed inside destDir.
		target := filepath.Join(destDir, clean)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
		// TypeRegA is not listed: archive/tar has normalized it to TypeReg
		// on read since Go 1.11, and the constant is deprecated.
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("mkdir parent of %q: %w", target, err)
			}
			mode := os.FileMode(hdr.Mode & 0o777)
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("create %q: %w", target, err)
			}
			limited := io.LimitReader(tr, MaxArchiveSize)
			if _, err := io.Copy(f, limited); err != nil {
				_ = f.Close()
				return fmt.Errorf("write %q: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %q: %w", target, err)
			}
		default:
			// Symlinks, hardlinks, devices, fifos: skipped silently. The hash
			// is computed over the extracted tree, so omitting them just
			// produces a different (but stable) hash for archives that
			// contain them. ValidateArchive doesn't reject them either.
			continue
		}
	}
	return nil
}
