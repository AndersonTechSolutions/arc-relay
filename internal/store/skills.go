package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Skill is one row in `skills`. Visibility, latest_version pointer, and yanked_at
// live with the metadata; archives themselves live on disk and are tracked in
// SkillVersion rows.
type Skill struct {
	ID            string     `json:"id"`
	Slug          string     `json:"slug"`
	DisplayName   string     `json:"display_name"`
	Description   string     `json:"description"`
	Visibility    string     `json:"visibility"`
	LatestVersion string     `json:"latest_version,omitempty"`
	YankedAt      *time.Time `json:"yanked_at,omitempty"`
	CreatedBy     *string    `json:"created_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Outdated      int        `json:"outdated"`
}

// SkillVersion is one row in `skill_versions`. Each row corresponds to a single
// uploaded archive at <bundles_dir>/<archive_path>. SHA-256 lets the client
// verify integrity after download.
type SkillVersion struct {
	SkillID       string          `json:"skill_id"`
	Version       string          `json:"version"`
	ArchivePath   string          `json:"archive_path"`
	ArchiveSize   int64           `json:"archive_size"`
	ArchiveSHA256 string          `json:"archive_sha256"`
	Manifest      json.RawMessage `json:"manifest"`
	YankedAt      *time.Time      `json:"yanked_at,omitempty"`
	UploadedBy    *string         `json:"uploaded_by,omitempty"`
	UploadedAt    time.Time       `json:"uploaded_at"`
}

// SkillAssignment grants a user access to a restricted skill. A NULL Version
// means "follow latest" — i.e. the user always gets whatever Skill.LatestVersion
// points to at sync time.
type SkillAssignment struct {
	SkillID    string    `json:"skill_id"`
	UserID     string    `json:"user_id"`
	Version    *string   `json:"version,omitempty"`
	AssignedBy *string   `json:"assigned_by,omitempty"`
	AssignedAt time.Time `json:"assigned_at"`
}

// SkillUpstream is the opted-in upstream-tracking row for a skill.
// One row per skill_id (1:1 with skills).
type SkillUpstream struct {
	SkillID                string
	UpstreamType           string // "git"
	GitURL                 string
	GitSubpath             string
	GitRef                 string
	LastCheckedAt          *time.Time
	LastSeenSHA            *string
	LastSeenHash           *string
	DriftDetectedAt        *time.Time
	DriftRelayVersion      *string
	DriftRelayHash         *string
	DriftUpstreamSHA       *string
	DriftUpstreamHash      *string
	DriftCommitsAhead      *int
	DriftSeverity          *string
	DriftSummary           *string
	DriftRecommendedAction *string
	DriftLLMModel          *string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// DriftReport is what the checker writes when drift is detected.
// All fields required.
type DriftReport struct {
	RelayVersion      string
	RelayHash         string
	UpstreamSHA       string
	UpstreamHash      string
	CommitsAhead      int
	Severity          string // cosmetic|minor|major|security|unknown
	Summary           string
	RecommendedAction string
	LLMModel          string // empty if fallback path
	DetectedAt        time.Time
}

// ErrSkillSlugConflict is returned when a slug is already taken.
var ErrSkillSlugConflict = errors.New("skill slug already exists")

// ErrSkillVersionConflict is returned when (skill_id, version) is already taken.
var ErrSkillVersionConflict = errors.New("skill version already exists")

// SkillStore is the persistence layer for skills, versions, and assignments.
type SkillStore struct {
	db *DB
}

// NewSkillStore returns a SkillStore backed by db.
func NewSkillStore(db *DB) *SkillStore {
	return &SkillStore{db: db}
}

// CreateSkill inserts a new skill row. Reuses the slug regex from servers.go
// via ValidateSlug so all relay-managed slugs share one rule.
func (s *SkillStore) CreateSkill(sk *Skill) error {
	if err := ValidateSlug(sk.Slug); err != nil {
		return err
	}
	if sk.ID == "" {
		sk.ID = uuid.New().String()
	}
	if sk.Visibility == "" {
		sk.Visibility = "restricted"
	}
	if sk.Visibility != "public" && sk.Visibility != "restricted" {
		return fmt.Errorf("invalid visibility %q", sk.Visibility)
	}
	now := time.Now()
	sk.CreatedAt = now
	sk.UpdatedAt = now

	_, err := s.db.Exec(`
		INSERT INTO skills (id, slug, display_name, description, visibility, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sk.ID, sk.Slug, sk.DisplayName, sk.Description, sk.Visibility, sk.CreatedBy, sk.CreatedAt, sk.UpdatedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrSkillSlugConflict
		}
		return fmt.Errorf("creating skill: %w", err)
	}
	return nil
}

// GetSkill returns a skill by id, or (nil, nil) if not found.
func (s *SkillStore) GetSkill(id string) (*Skill, error) {
	return s.scanSkillRow(s.db.QueryRow(`
		SELECT id, slug, display_name, description, visibility,
		       COALESCE(latest_version, ''), yanked_at, created_by, created_at, updated_at, outdated
		FROM skills WHERE id = ?`, id))
}

// GetSkillBySlug returns a skill by slug, or (nil, nil) if not found.
func (s *SkillStore) GetSkillBySlug(slug string) (*Skill, error) {
	return s.scanSkillRow(s.db.QueryRow(`
		SELECT id, slug, display_name, description, visibility,
		       COALESCE(latest_version, ''), yanked_at, created_by, created_at, updated_at, outdated
		FROM skills WHERE slug = ?`, slug))
}

// ListSkills returns all skills ordered by slug. Yanked skills are included —
// callers filter as needed; admin views want them, public listings don't.
func (s *SkillStore) ListSkills() ([]*Skill, error) {
	rows, err := s.db.Query(`
		SELECT id, slug, display_name, description, visibility,
		       COALESCE(latest_version, ''), yanked_at, created_by, created_at, updated_at, outdated
		FROM skills ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("listing skills: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Skill
	for rows.Next() {
		sk, err := s.scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// UpdateSkillMeta patches the mutable metadata fields. Slug, ID, and
// latest_version are managed elsewhere (slug rename via separate API in Phase 1
// if ever; latest_version is set as a side effect of CreateVersion).
func (s *SkillStore) UpdateSkillMeta(id, displayName, description, visibility string) error {
	if visibility != "" && visibility != "public" && visibility != "restricted" {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	_, err := s.db.Exec(`
		UPDATE skills SET display_name = ?, description = ?, visibility = COALESCE(NULLIF(?, ''), visibility),
		                  updated_at = ?
		WHERE id = ?`,
		displayName, description, visibility, time.Now(), id,
	)
	if err != nil {
		return fmt.Errorf("updating skill meta: %w", err)
	}
	return nil
}

// YankSkill marks a skill as yanked at now(). Yanked skills are hidden from
// public listings and `assigned` queries but the row + its archives are kept,
// so previously-installed clients keep working until they sync next.
func (s *SkillStore) YankSkill(id string) error {
	now := time.Now()
	_, err := s.db.Exec(`UPDATE skills SET yanked_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
	if err != nil {
		return fmt.Errorf("yanking skill: %w", err)
	}
	return nil
}

// UnyankSkill clears yanked_at, returning the skill to active listings.
func (s *SkillStore) UnyankSkill(id string) error {
	_, err := s.db.Exec(`UPDATE skills SET yanked_at = NULL, updated_at = ? WHERE id = ?`, time.Now(), id)
	if err != nil {
		return fmt.Errorf("unyanking skill: %w", err)
	}
	return nil
}

// DeleteSkill removes a skill row. ON DELETE CASCADE cleans up versions and
// assignments. Disk archives are NOT removed by this — the caller is expected
// to do that after a successful delete (or schedule it for cleanup). Hard
// delete should be reserved for "never published" mistakes; prefer YankSkill.
func (s *SkillStore) DeleteSkill(id string) error {
	_, err := s.db.Exec(`DELETE FROM skills WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting skill: %w", err)
	}
	return nil
}

// CreateVersion inserts a version row and atomically advances skills.latest_version
// to the new version. The atomic update keeps "latest" pointing at the most
// recently uploaded version regardless of semver ordering — uploaders are
// expected to push monotonically increasing versions; if they don't, the latest
// pointer reflects upload order, which is the intuitive behavior for a fleet
// rollout (the most recent push is the one operators want clients to pick up).
func (s *SkillStore) CreateVersion(v *SkillVersion) error {
	if v.Version == "" {
		return fmt.Errorf("version is required")
	}
	if v.ArchivePath == "" {
		return fmt.Errorf("archive_path is required")
	}
	if v.ArchiveSize <= 0 {
		return fmt.Errorf("archive_size must be > 0")
	}
	if v.ArchiveSHA256 == "" {
		return fmt.Errorf("archive_sha256 is required")
	}
	if len(v.Manifest) == 0 {
		v.Manifest = json.RawMessage("{}")
	}
	v.UploadedAt = time.Now()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
		INSERT INTO skill_versions
		    (skill_id, version, archive_path, archive_size, archive_sha256, manifest, uploaded_by, uploaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		v.SkillID, v.Version, v.ArchivePath, v.ArchiveSize, v.ArchiveSHA256, string(v.Manifest), v.UploadedBy, v.UploadedAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrSkillVersionConflict
		}
		return fmt.Errorf("inserting skill version: %w", err)
	}

	if _, err := tx.Exec(`UPDATE skills SET latest_version = ?, updated_at = ? WHERE id = ?`,
		v.Version, v.UploadedAt, v.SkillID); err != nil {
		return fmt.Errorf("updating latest_version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// GetVersion returns one version row, or (nil, nil) if not found.
func (s *SkillStore) GetVersion(skillID, version string) (*SkillVersion, error) {
	v := &SkillVersion{}
	var manifest string
	var yankedAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT skill_id, version, archive_path, archive_size, archive_sha256, manifest,
		       yanked_at, uploaded_by, uploaded_at
		FROM skill_versions WHERE skill_id = ? AND version = ?`, skillID, version,
	).Scan(&v.SkillID, &v.Version, &v.ArchivePath, &v.ArchiveSize, &v.ArchiveSHA256, &manifest,
		&yankedAt, &v.UploadedBy, &v.UploadedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting skill version: %w", err)
	}
	v.Manifest = json.RawMessage(manifest)
	if yankedAt.Valid {
		t := yankedAt.Time
		v.YankedAt = &t
	}
	return v, nil
}

// ListVersions returns all versions for a skill, newest upload first.
func (s *SkillStore) ListVersions(skillID string) ([]*SkillVersion, error) {
	rows, err := s.db.Query(`
		SELECT skill_id, version, archive_path, archive_size, archive_sha256, manifest,
		       yanked_at, uploaded_by, uploaded_at
		FROM skill_versions WHERE skill_id = ? ORDER BY uploaded_at DESC`, skillID)
	if err != nil {
		return nil, fmt.Errorf("listing skill versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*SkillVersion
	for rows.Next() {
		v := &SkillVersion{}
		var manifest string
		var yankedAt sql.NullTime
		if err := rows.Scan(&v.SkillID, &v.Version, &v.ArchivePath, &v.ArchiveSize, &v.ArchiveSHA256,
			&manifest, &yankedAt, &v.UploadedBy, &v.UploadedAt); err != nil {
			return nil, fmt.Errorf("scanning skill version: %w", err)
		}
		v.Manifest = json.RawMessage(manifest)
		if yankedAt.Valid {
			t := yankedAt.Time
			v.YankedAt = &t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// YankVersion marks a single version as yanked. Existing clients can still
// download by exact `@version` pin (Phase 1 will surface this as a flag);
// "follow latest" sync skips yanked versions.
func (s *SkillStore) YankVersion(skillID, version string) error {
	now := time.Now()
	res, err := s.db.Exec(`UPDATE skill_versions SET yanked_at = ? WHERE skill_id = ? AND version = ?`,
		now, skillID, version)
	if err != nil {
		return fmt.Errorf("yanking skill version: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UnyankVersion clears yanked_at on a version.
func (s *SkillStore) UnyankVersion(skillID, version string) error {
	res, err := s.db.Exec(`UPDATE skill_versions SET yanked_at = NULL WHERE skill_id = ? AND version = ?`,
		skillID, version)
	if err != nil {
		return fmt.Errorf("unyanking skill version: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AssignSkill grants a user access to a restricted skill. version may be empty
// to mean "follow latest". Calling AssignSkill on an existing assignment is an
// upsert — the version pin is replaced.
func (s *SkillStore) AssignSkill(a *SkillAssignment) error {
	if a.SkillID == "" || a.UserID == "" {
		return fmt.Errorf("skill_id and user_id are required")
	}
	a.AssignedAt = time.Now()
	_, err := s.db.Exec(`
		INSERT INTO skill_assignments (skill_id, user_id, version, assigned_by, assigned_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(skill_id, user_id) DO UPDATE SET
		    version = excluded.version,
		    assigned_by = excluded.assigned_by,
		    assigned_at = excluded.assigned_at`,
		a.SkillID, a.UserID, a.Version, a.AssignedBy, a.AssignedAt,
	)
	if err != nil {
		return fmt.Errorf("assigning skill: %w", err)
	}
	return nil
}

// UnassignSkill revokes a single user's access to a restricted skill.
func (s *SkillStore) UnassignSkill(skillID, userID string) error {
	_, err := s.db.Exec(`DELETE FROM skill_assignments WHERE skill_id = ? AND user_id = ?`, skillID, userID)
	if err != nil {
		return fmt.Errorf("unassigning skill: %w", err)
	}
	return nil
}

// ListAssignmentsForSkill returns who has been granted access to a given skill.
func (s *SkillStore) ListAssignmentsForSkill(skillID string) ([]*SkillAssignment, error) {
	rows, err := s.db.Query(`
		SELECT skill_id, user_id, version, assigned_by, assigned_at
		FROM skill_assignments WHERE skill_id = ? ORDER BY assigned_at`, skillID)
	if err != nil {
		return nil, fmt.Errorf("listing assignments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanAssignments(rows)
}

// AssignedSkill is the row shape returned by AssignedForUser — joins skill
// metadata onto the assignment so the client gets everything it needs in one
// call (slug, latest_version, version pin if any).
type AssignedSkill struct {
	Skill         *Skill  `json:"skill"`
	PinnedVersion *string `json:"pinned_version,omitempty"`
}

// AssignedForUser returns every skill the user has access to right now —
// public-and-not-yanked plus restricted skills with an explicit assignment.
// Used by `arc-sync skill sync` to compute the desired client state.
func (s *SkillStore) AssignedForUser(userID string) ([]*AssignedSkill, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.slug, s.display_name, s.description, s.visibility,
		       COALESCE(s.latest_version, ''), s.yanked_at, s.created_by, s.created_at, s.updated_at,
		       a.version
		FROM skills s
		LEFT JOIN skill_assignments a
		    ON a.skill_id = s.id AND a.user_id = ?
		WHERE s.yanked_at IS NULL
		  AND (s.visibility = 'public' OR a.user_id IS NOT NULL)
		ORDER BY s.slug`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing assigned skills: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*AssignedSkill
	for rows.Next() {
		sk := &Skill{}
		var yankedAt sql.NullTime
		var pinned sql.NullString
		if err := rows.Scan(&sk.ID, &sk.Slug, &sk.DisplayName, &sk.Description, &sk.Visibility,
			&sk.LatestVersion, &yankedAt, &sk.CreatedBy, &sk.CreatedAt, &sk.UpdatedAt, &pinned); err != nil {
			return nil, fmt.Errorf("scanning assigned skill: %w", err)
		}
		if yankedAt.Valid {
			t := yankedAt.Time
			sk.YankedAt = &t
		}
		as := &AssignedSkill{Skill: sk}
		if pinned.Valid {
			v := pinned.String
			as.PinnedVersion = &v
		}
		out = append(out, as)
	}
	return out, rows.Err()
}

// scanSkill reads one row off rows.Scan-compatible iterator into a Skill.
func (s *SkillStore) scanSkill(scanner interface {
	Scan(dest ...any) error
}) (*Skill, error) {
	sk := &Skill{}
	var yankedAt sql.NullTime
	if err := scanner.Scan(&sk.ID, &sk.Slug, &sk.DisplayName, &sk.Description, &sk.Visibility,
		&sk.LatestVersion, &yankedAt, &sk.CreatedBy, &sk.CreatedAt, &sk.UpdatedAt, &sk.Outdated); err != nil {
		return nil, fmt.Errorf("scanning skill: %w", err)
	}
	if yankedAt.Valid {
		t := yankedAt.Time
		sk.YankedAt = &t
	}
	return sk, nil
}

func (s *SkillStore) scanSkillRow(row *sql.Row) (*Skill, error) {
	sk, err := s.scanSkill(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sk, err
}

func scanAssignments(rows *sql.Rows) ([]*SkillAssignment, error) {
	var out []*SkillAssignment
	for rows.Next() {
		a := &SkillAssignment{}
		var v sql.NullString
		if err := rows.Scan(&a.SkillID, &a.UserID, &v, &a.AssignedBy, &a.AssignedAt); err != nil {
			return nil, fmt.Errorf("scanning skill assignment: %w", err)
		}
		if v.Valid {
			s := v.String
			a.Version = &s
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertUpstream inserts or updates the upstream-tracking row for a skill.
// On conflict (same skill_id) the upstream metadata fields are replaced; the
// last_seen_* / drift_* fields are preserved (use the dedicated check/drift
// methods to update those).
func (s *SkillStore) UpsertUpstream(u *SkillUpstream) error {
	if u.UpstreamType == "" {
		u.UpstreamType = "git"
	}
	if u.GitRef == "" {
		u.GitRef = "HEAD"
	}
	_, err := s.db.Exec(`
		INSERT INTO skill_upstreams (
			skill_id, upstream_type, git_url, git_subpath, git_ref,
			last_seen_hash, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(skill_id) DO UPDATE SET
			upstream_type = excluded.upstream_type,
			git_url       = excluded.git_url,
			git_subpath   = excluded.git_subpath,
			git_ref       = excluded.git_ref,
			updated_at    = CURRENT_TIMESTAMP
	`, u.SkillID, u.UpstreamType, u.GitURL, u.GitSubpath, u.GitRef, u.LastSeenHash)
	if err != nil {
		return fmt.Errorf("upserting skill upstream: %w", err)
	}
	return nil
}

// GetUpstream returns the upstream row for a skill, or (nil, nil) if none.
func (s *SkillStore) GetUpstream(skillID string) (*SkillUpstream, error) {
	row := s.db.QueryRow(`
		SELECT skill_id, upstream_type, git_url, git_subpath, git_ref,
			last_checked_at, last_seen_sha, last_seen_hash,
			drift_detected_at, drift_relay_version, drift_relay_hash,
			drift_upstream_sha, drift_upstream_hash, drift_commits_ahead,
			drift_severity, drift_summary, drift_recommended_action, drift_llm_model,
			created_at, updated_at
		FROM skill_upstreams WHERE skill_id = ?
	`, skillID)
	u, err := scanUpstream(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting skill upstream: %w", err)
	}
	return u, nil
}

// ClearUpstream removes the upstream row for a skill (opt-out of update
// tracking) and clears any stale `skills.outdated` flag in the same
// transaction. The flag would otherwise be misleading: with no upstream to
// compare against, "outdated" has no referent and the dashboard's drift card
// would have nothing to render.
func (s *SkillStore) ClearUpstream(skillID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM skill_upstreams WHERE skill_id = ?`, skillID); err != nil {
		return fmt.Errorf("clearing skill upstream: %w", err)
	}
	if _, err := tx.Exec(`UPDATE skills SET outdated = 0 WHERE id = ?`, skillID); err != nil {
		return fmt.Errorf("clearing outdated flag: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit clear upstream: %w", err)
	}
	return nil
}

// ListUpstreams returns every upstream-tracking row, ordered with never-checked
// rows first, then oldest-checked, with skill_id as a tiebreak. This is the
// order the cron iterator wants.
func (s *SkillStore) ListUpstreams() ([]*SkillUpstream, error) {
	rows, err := s.db.Query(`
		SELECT skill_id, upstream_type, git_url, git_subpath, git_ref,
			last_checked_at, last_seen_sha, last_seen_hash,
			drift_detected_at, drift_relay_version, drift_relay_hash,
			drift_upstream_sha, drift_upstream_hash, drift_commits_ahead,
			drift_severity, drift_summary, drift_recommended_action, drift_llm_model,
			created_at, updated_at
		FROM skill_upstreams
		ORDER BY last_checked_at IS NULL DESC, last_checked_at ASC, skill_id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing skill upstreams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*SkillUpstream
	for rows.Next() {
		u, err := scanUpstream(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning skill upstream: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUpstreamCheck records the result of a no-drift check (cron path):
// updates last_seen_sha, last_seen_hash, last_checked_at. Drift fields are not
// touched here — clear them via ClearDriftReport when a new version actually
// resolves the drift.
func (s *SkillStore) UpdateUpstreamCheck(skillID, sha, hash string, checkedAt time.Time) error {
	_, err := s.db.Exec(`
		UPDATE skill_upstreams SET
			last_seen_sha = ?, last_seen_hash = ?, last_checked_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE skill_id = ?
	`, sha, hash, checkedAt, skillID)
	if err != nil {
		return fmt.Errorf("updating upstream check: %w", err)
	}
	return nil
}

// WriteDriftReport persists a drift report for a skill and flips
// skills.outdated=1 atomically.
func (s *SkillStore) WriteDriftReport(skillID string, r *DriftReport) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		UPDATE skill_upstreams SET
			drift_detected_at = ?, drift_relay_version = ?, drift_relay_hash = ?,
			drift_upstream_sha = ?, drift_upstream_hash = ?, drift_commits_ahead = ?,
			drift_severity = ?, drift_summary = ?, drift_recommended_action = ?,
			drift_llm_model = ?,
			last_seen_sha = ?, last_seen_hash = ?, last_checked_at = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE skill_id = ?
	`, r.DetectedAt, r.RelayVersion, r.RelayHash,
		r.UpstreamSHA, r.UpstreamHash, r.CommitsAhead,
		r.Severity, r.Summary, r.RecommendedAction, r.LLMModel,
		r.UpstreamSHA, r.UpstreamHash, r.DetectedAt,
		skillID); err != nil {
		return fmt.Errorf("writing drift report: %w", err)
	}
	if _, err := tx.Exec(`UPDATE skills SET outdated = 1 WHERE id = ?`, skillID); err != nil {
		return fmt.Errorf("setting outdated flag: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit drift report: %w", err)
	}
	return nil
}

// ClearDriftReport clears all drift_* fields for a skill, records the latest
// upstream hash as last_seen_hash, and flips skills.outdated=0 atomically.
// Used after a fresh version is uploaded that brings the relay back in sync
// with upstream.
func (s *SkillStore) ClearDriftReport(skillID, latestSeenHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		UPDATE skill_upstreams SET
			drift_detected_at = NULL, drift_relay_version = NULL, drift_relay_hash = NULL,
			drift_upstream_sha = NULL, drift_upstream_hash = NULL, drift_commits_ahead = NULL,
			drift_severity = NULL, drift_summary = NULL, drift_recommended_action = NULL,
			drift_llm_model = NULL,
			last_seen_hash = ?, updated_at = CURRENT_TIMESTAMP
		WHERE skill_id = ?
	`, latestSeenHash, skillID); err != nil {
		return fmt.Errorf("clearing drift report: %w", err)
	}
	if _, err := tx.Exec(`UPDATE skills SET outdated = 0 WHERE id = ?`, skillID); err != nil {
		return fmt.Errorf("clearing outdated flag: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit clear drift: %w", err)
	}
	return nil
}

// scanUpstream reads one row off a rows.Scan-compatible iterator into a SkillUpstream.
func scanUpstream(scanner interface {
	Scan(dest ...any) error
}) (*SkillUpstream, error) {
	var u SkillUpstream
	var (
		lastCheckedAt   sql.NullTime
		lastSeenSHA     sql.NullString
		lastSeenHash    sql.NullString
		driftDetectedAt sql.NullTime
		driftRelayVer   sql.NullString
		driftRelayHash  sql.NullString
		driftUpSHA      sql.NullString
		driftUpHash     sql.NullString
		driftCommits    sql.NullInt64
		driftSeverity   sql.NullString
		driftSummary    sql.NullString
		driftAction     sql.NullString
		driftLLMModel   sql.NullString
	)
	if err := scanner.Scan(
		&u.SkillID, &u.UpstreamType, &u.GitURL, &u.GitSubpath, &u.GitRef,
		&lastCheckedAt, &lastSeenSHA, &lastSeenHash,
		&driftDetectedAt, &driftRelayVer, &driftRelayHash,
		&driftUpSHA, &driftUpHash, &driftCommits,
		&driftSeverity, &driftSummary, &driftAction, &driftLLMModel,
		&u.CreatedAt, &u.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if lastCheckedAt.Valid {
		t := lastCheckedAt.Time
		u.LastCheckedAt = &t
	}
	if lastSeenSHA.Valid {
		v := lastSeenSHA.String
		u.LastSeenSHA = &v
	}
	if lastSeenHash.Valid {
		v := lastSeenHash.String
		u.LastSeenHash = &v
	}
	if driftDetectedAt.Valid {
		t := driftDetectedAt.Time
		u.DriftDetectedAt = &t
	}
	if driftRelayVer.Valid {
		v := driftRelayVer.String
		u.DriftRelayVersion = &v
	}
	if driftRelayHash.Valid {
		v := driftRelayHash.String
		u.DriftRelayHash = &v
	}
	if driftUpSHA.Valid {
		v := driftUpSHA.String
		u.DriftUpstreamSHA = &v
	}
	if driftUpHash.Valid {
		v := driftUpHash.String
		u.DriftUpstreamHash = &v
	}
	if driftCommits.Valid {
		v := int(driftCommits.Int64)
		u.DriftCommitsAhead = &v
	}
	if driftSeverity.Valid {
		v := driftSeverity.String
		u.DriftSeverity = &v
	}
	if driftSummary.Valid {
		v := driftSummary.String
		u.DriftSummary = &v
	}
	if driftAction.Valid {
		v := driftAction.String
		u.DriftRecommendedAction = &v
	}
	if driftLLMModel.Valid {
		v := driftLLMModel.String
		u.DriftLLMModel = &v
	}
	return &u, nil
}
