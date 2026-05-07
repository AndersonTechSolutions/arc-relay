package store

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID                 string    `json:"id"`
	Username           string    `json:"username"`
	PasswordHash       string    `json:"-"`
	Role               string    `json:"role"`
	AccessLevel        string    `json:"access_level"`
	DefaultProfileID   *string   `json:"default_profile_id,omitempty"` // user's default profile for RBAC
	MustChangePassword bool      `json:"must_change_password,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	ProfileID          *string   `json:"profile_id,omitempty"`   // effective profile (resolved at auth time)
	ProfileName        string    `json:"profile_name,omitempty"` // populated on read, not stored
}

type APIKey struct {
	ID          string     `json:"id"`
	UserID      string     `json:"user_id"`
	KeyHash     string     `json:"-"`
	Name        string     `json:"name"`
	ProfileID   *string    `json:"profile_id,omitempty"`
	ProfileName string     `json:"profile_name,omitempty"` // populated on read, not stored
	CreatedAt   time.Time  `json:"created_at"`
	LastUsed    *time.Time `json:"last_used,omitempty"`
	Revoked     bool       `json:"revoked"`
	// Capabilities are coarse-grained verbs (e.g. "skills:write") that grant
	// non-admin keys specific write powers without granting full admin. Admin
	// keys (owning user has role='admin') ignore this list — their effective
	// capability set is "everything". See migration 018 + middleware
	// requireCapability for the enforcement path.
	Capabilities []string `json:"capabilities,omitempty"`
}

// HasCapability reports whether the key has been granted the named capability.
// Pure check on the stored list — does NOT consider the owning user's admin
// status. Callers that want admin-bypass semantics should use the middleware
// helper requireCapability, which combines both.
func (k *APIKey) HasCapability(cap string) bool {
	if k == nil {
		return false
	}
	for _, c := range k.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

type UserStore struct {
	db *DB
}

func NewUserStore(db *DB) *UserStore {
	return &UserStore{db: db}
}

func (s *UserStore) Create(username, password, role string) (*User, error) {
	return s.CreateWithAccessLevel(username, password, role, "", nil)
}

func (s *UserStore) CreateWithAccessLevel(username, password, role, accessLevel string, defaultProfileID *string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}

	// Force admin access level for admin role
	if role == "admin" {
		accessLevel = "admin"
	}
	if accessLevel == "" {
		accessLevel = "write"
	}

	user := &User{
		ID:               uuid.New().String(),
		Username:         username,
		PasswordHash:     string(hash),
		Role:             role,
		AccessLevel:      accessLevel,
		DefaultProfileID: defaultProfileID,
		CreatedAt:        time.Now(),
	}

	_, err = s.db.Exec(`
		INSERT INTO users (id, username, password_hash, role, access_level, default_profile_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.PasswordHash, user.Role, user.AccessLevel, user.DefaultProfileID, user.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("creating user: %w", err)
	}
	return user, nil
}

func (s *UserStore) Authenticate(username, password string) (*User, error) {
	user := &User{}
	err := s.db.QueryRow(`
		SELECT id, username, password_hash, role, access_level, default_profile_id, must_change_password, created_at
		FROM users WHERE username = ?`, username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &user.AccessLevel, &user.DefaultProfileID, &user.MustChangePassword, &user.CreatedAt)
	if err == sql.ErrNoRows {
		// Run bcrypt against a dummy hash so the user-not-found path costs
		// the same as the wrong-password path. Without this, response time
		// (microseconds vs ~50ms) leaks which usernames exist.
		_ = bcrypt.CompareHashAndPassword(dummyBcryptHash, []byte(password))
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up user: %w", err)
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, nil
	}

	return user, nil
}

// dummyBcryptHash is a precomputed bcrypt hash used to pad timing on the
// user-not-found path in Authenticate, mitigating username enumeration.
// The plaintext is irrelevant — the hash exists only to give bcrypt
// something legitimately shaped to verify against, and the comparison
// always fails because no real account password matches.
var dummyBcryptHash = mustGenerateDummyBcryptHash()

func mustGenerateDummyBcryptHash() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing-padding"), bcrypt.DefaultCost)
	if err != nil {
		panic("bcrypt: failed to generate dummy hash for timing padding: " + err.Error())
	}
	return h
}

func (s *UserStore) Get(id string) (*User, error) {
	user := &User{}
	err := s.db.QueryRow(`
		SELECT id, username, password_hash, role, access_level, default_profile_id, must_change_password, created_at
		FROM users WHERE id = ?`, id,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &user.AccessLevel, &user.DefaultProfileID, &user.MustChangePassword, &user.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting user: %w", err)
	}
	return user, nil
}

func (s *UserStore) GetByUsername(username string) (*User, error) {
	user := &User{}
	err := s.db.QueryRow(`
		SELECT id, username, password_hash, role, access_level, default_profile_id, must_change_password, created_at
		FROM users WHERE username = ?`, username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &user.AccessLevel, &user.DefaultProfileID, &user.MustChangePassword, &user.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting user by username: %w", err)
	}
	return user, nil
}

func (s *UserStore) List() ([]*User, error) {
	rows, err := s.db.Query(`
		SELECT u.id, u.username, u.password_hash, u.role, u.access_level,
		       u.default_profile_id, u.must_change_password, COALESCE(ap.name, ''), u.created_at
		FROM users u
		LEFT JOIN agent_profiles ap ON u.default_profile_id = ap.id
		ORDER BY u.created_at`)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var users []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.AccessLevel,
			&u.DefaultProfileID, &u.MustChangePassword, &u.ProfileName, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning user: %w", err)
		}
		users = append(users, u)
	}
	return users, nil
}

func (s *UserStore) UpdateProfile(id string, defaultProfileID *string) error {
	_, err := s.db.Exec(`UPDATE users SET default_profile_id = ? WHERE id = ?`, defaultProfileID, id)
	return err
}

func (s *UserStore) UpdateRole(id, role string) error {
	_, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
	if role == "admin" {
		_, _ = s.db.Exec(`UPDATE users SET access_level = 'admin' WHERE id = ?`, id)
	}
	return err
}

// SetPassword updates a user's password hash and clears the must_change_password flag.
func (s *UserStore) SetPassword(id, newPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	_, err = s.db.Exec(`UPDATE users SET password_hash = ?, must_change_password = FALSE WHERE id = ?`, string(hash), id)
	return err
}

// SetMustChangePassword sets or clears the forced password rotation flag.
func (s *UserStore) SetMustChangePassword(id string, must bool) error {
	_, err := s.db.Exec(`UPDATE users SET must_change_password = ? WHERE id = ?`, must, id)
	return err
}

// CreateWithAccessLevelTx creates a user within an existing transaction.
func (s *UserStore) CreateWithAccessLevelTx(tx *sql.Tx, username, password, role, accessLevel string, defaultProfileID *string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}
	if role == "admin" {
		accessLevel = "admin"
	}
	if accessLevel == "" {
		accessLevel = "write"
	}
	user := &User{
		ID:               uuid.New().String(),
		Username:         username,
		PasswordHash:     string(hash),
		Role:             role,
		AccessLevel:      accessLevel,
		DefaultProfileID: defaultProfileID,
		CreatedAt:        time.Now(),
	}
	_, err = tx.Exec(`
		INSERT INTO users (id, username, password_hash, role, access_level, default_profile_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		user.ID, user.Username, user.PasswordHash, user.Role, user.AccessLevel, user.DefaultProfileID, user.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("creating user: %w", err)
	}
	return user, nil
}

// CreateAPIKeyTx generates a new API key within an existing transaction.
// capabilities may be nil; pass an empty slice or nil for keys that should
// inherit the owning user's role-based powers without any additional grants.
func (s *UserStore) CreateAPIKeyTx(tx *sql.Tx, userID, name string, profileID *string, capabilities []string) (string, *APIKey, error) {
	rawKey := uuid.New().String()
	keyHash := hashAPIKey(rawKey)
	caps := normalizeCapabilities(capabilities)
	capsJSON, err := marshalCapabilities(caps)
	if err != nil {
		return "", nil, err
	}
	ak := &APIKey{
		ID:           uuid.New().String(),
		UserID:       userID,
		KeyHash:      keyHash,
		Name:         name,
		ProfileID:    profileID,
		CreatedAt:    time.Now(),
		Capabilities: caps,
	}
	_, err = tx.Exec(`
		INSERT INTO api_keys (id, user_id, key_hash, name, profile_id, created_at, capabilities)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ak.ID, ak.UserID, ak.KeyHash, ak.Name, ak.ProfileID, ak.CreatedAt, capsJSON,
	)
	if err != nil {
		return "", nil, fmt.Errorf("creating api key: %w", err)
	}
	return rawKey, ak, nil
}

func (s *UserStore) Delete(id string) error {
	_, err := s.db.Exec("DELETE FROM users WHERE id = ?", id)
	return err
}

// EnsureAdmin creates the default admin user if no users exist.
// Also ensures existing admin users have access_level = 'admin'.
func (s *UserStore) EnsureAdmin(password string) error {
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return fmt.Errorf("counting users: %w", err)
	}
	if count > 0 {
		// Ensure all admin-role users have admin access level
		_, _ = s.db.Exec(`UPDATE users SET access_level = 'admin' WHERE role = 'admin' AND access_level != 'admin'`)
		return nil
	}
	_, err := s.Create("admin", password, "admin")
	return err
}

// API Key operations

func hashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// CreateAPIKey generates a new API key and returns it (plaintext shown once).
// capabilities may be nil/empty for keys that inherit only the owning user's
// role-based powers. Recognized capability strings: see migration 018.
func (s *UserStore) CreateAPIKey(userID, name string, profileID *string, capabilities []string) (string, *APIKey, error) {
	rawKey := uuid.New().String() // the plaintext key
	keyHash := hashAPIKey(rawKey)
	caps := normalizeCapabilities(capabilities)
	capsJSON, err := marshalCapabilities(caps)
	if err != nil {
		return "", nil, err
	}

	ak := &APIKey{
		ID:           uuid.New().String(),
		UserID:       userID,
		KeyHash:      keyHash,
		Name:         name,
		ProfileID:    profileID,
		CreatedAt:    time.Now(),
		Capabilities: caps,
	}

	_, err = s.db.Exec(`
		INSERT INTO api_keys (id, user_id, key_hash, name, profile_id, created_at, capabilities)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ak.ID, ak.UserID, ak.KeyHash, ak.Name, ak.ProfileID, ak.CreatedAt, capsJSON,
	)
	if err != nil {
		return "", nil, fmt.Errorf("creating api key: %w", err)
	}
	return rawKey, ak, nil
}

// SupportedCapabilities is the canonical list of capability strings the relay
// recognizes. Issuing a key with a capability not in this list is allowed
// (forward-compatible with future server versions) but the relay will simply
// never check for it. Kept short on purpose — additive growth only.
var SupportedCapabilities = []string{
	"skills:write",
	"skills:yank",
	"recipes:write",
	"recipes:yank",
}

// normalizeCapabilities trims, deduplicates, and sorts the input. Empty
// strings are dropped. Returns an empty slice (never nil) for stable JSON
// serialization — `[]` rather than `null` in the column.
func normalizeCapabilities(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func marshalCapabilities(caps []string) (string, error) {
	if caps == nil {
		caps = []string{}
	}
	b, err := json.Marshal(caps)
	if err != nil {
		return "", fmt.Errorf("marshaling capabilities: %w", err)
	}
	return string(b), nil
}

func unmarshalCapabilities(s string) ([]string, error) {
	if s == "" {
		return []string{}, nil
	}
	var caps []string
	if err := json.Unmarshal([]byte(s), &caps); err != nil {
		return nil, fmt.Errorf("parsing capabilities JSON: %w", err)
	}
	return caps, nil
}

// ValidateAPIKey checks a raw API key and returns the associated user AND
// the api_key row itself (so middleware can read per-key capabilities).
// Resolution order for effective profile:
//  1. Key has explicit profile_id → use that
//  2. Owning user has default_profile_id → use that
//  3. No profile → legacy tier-based access via access_level
//
// Returns (nil, nil, nil) when the key is unknown, hash-mismatched, or
// revoked — same semantics as the original (*User, error) signature for the
// "not authenticated" case. Callers must handle nil-User gracefully.
func (s *UserStore) ValidateAPIKey(rawKey string) (*User, *APIKey, error) {
	keyHash := hashAPIKey(rawKey)

	var keyID, userID, name string
	var storedHash string
	var revoked bool
	var keyProfileID sql.NullString
	var capsJSON sql.NullString
	var createdAt time.Time
	var lastUsed sql.NullTime
	err := s.db.QueryRow(`
		SELECT id, user_id, key_hash, name, revoked, profile_id, created_at, last_used, capabilities
		FROM api_keys WHERE key_hash = ?`, keyHash,
	).Scan(&keyID, &userID, &storedHash, &name, &revoked, &keyProfileID, &createdAt, &lastUsed, &capsJSON)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("looking up api key: %w", err)
	}

	// Constant-time comparison
	if subtle.ConstantTimeCompare([]byte(keyHash), []byte(storedHash)) != 1 {
		return nil, nil, nil
	}
	if revoked {
		return nil, nil, nil
	}

	// Update last_used
	_, _ = s.db.Exec("UPDATE api_keys SET last_used = ? WHERE key_hash = ?", time.Now(), keyHash)

	user, err := s.Get(userID)
	if err != nil || user == nil {
		return nil, nil, err
	}

	// Resolve effective profile: key-level override > user default > none
	if keyProfileID.Valid {
		user.ProfileID = &keyProfileID.String
	} else if user.DefaultProfileID != nil {
		user.ProfileID = user.DefaultProfileID
	}

	caps, err := unmarshalCapabilities(capsJSON.String)
	if err != nil {
		// Don't fail auth on a malformed capabilities column — treat as
		// "no capabilities" and log via the warn path. A bad row shouldn't
		// brick the whole API.
		caps = []string{}
	}

	ak := &APIKey{
		ID:           keyID,
		UserID:       userID,
		KeyHash:      storedHash,
		Name:         name,
		CreatedAt:    createdAt,
		Revoked:      revoked,
		Capabilities: caps,
	}
	if keyProfileID.Valid {
		v := keyProfileID.String
		ak.ProfileID = &v
	}
	if lastUsed.Valid {
		t := lastUsed.Time
		ak.LastUsed = &t
	}

	return user, ak, nil
}

func (s *UserStore) ListAPIKeys(userID string) ([]*APIKey, error) {
	rows, err := s.db.Query(`
		SELECT ak.id, ak.user_id, ak.name, ak.profile_id, COALESCE(ap.name, ''), ak.created_at, ak.last_used, ak.revoked, ak.capabilities
		FROM api_keys ak
		LEFT JOIN agent_profiles ap ON ak.profile_id = ap.id
		WHERE ak.user_id = ? ORDER BY ak.created_at`, userID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []*APIKey
	for rows.Next() {
		k := &APIKey{}
		var profileID sql.NullString
		var capsJSON sql.NullString
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &profileID, &k.ProfileName, &k.CreatedAt, &k.LastUsed, &k.Revoked, &capsJSON); err != nil {
			return nil, fmt.Errorf("scanning api key: %w", err)
		}
		if profileID.Valid {
			k.ProfileID = &profileID.String
		}
		if capsJSON.Valid {
			caps, err := unmarshalCapabilities(capsJSON.String)
			if err == nil {
				k.Capabilities = caps
			}
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *UserStore) RevokeAPIKey(id string) error {
	_, err := s.db.Exec("UPDATE api_keys SET revoked = TRUE WHERE id = ?", id)
	return err
}

// RecentUserRow is returned by RecentUsers.
type RecentUserRow struct {
	UserID    string
	Username  string
	CreatedAt time.Time
}

// RecentUsers returns users created within the since window, newest-first.
func (s *UserStore) RecentUsers(limit int, since time.Time) ([]*RecentUserRow, error) {
	rows, err := s.db.Query(`
		SELECT id, username, created_at FROM users
		WHERE created_at >= ?
		ORDER BY created_at DESC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("recent users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*RecentUserRow
	for rows.Next() {
		r := &RecentUserRow{}
		if err := rows.Scan(&r.UserID, &r.Username, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning recent user: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentAPIKeyRow is returned by RecentAPIKeys.
type RecentAPIKeyRow struct {
	KeyID        string
	UserID       string
	KeyName      string
	Username     string
	Capabilities []string
	CreatedAt    time.Time
}

// RecentAPIKeys returns API keys created within the since window, newest-first,
// with the owning user's username joined in.
func (s *UserStore) RecentAPIKeys(limit int, since time.Time) ([]*RecentAPIKeyRow, error) {
	rows, err := s.db.Query(`
		SELECT ak.id, ak.user_id, ak.name, COALESCE(u.username, ''),
		       ak.capabilities, ak.created_at
		FROM api_keys ak
		LEFT JOIN users u ON ak.user_id = u.id
		WHERE ak.created_at >= ?
		ORDER BY ak.created_at DESC
		LIMIT ?`, since, limit)
	if err != nil {
		return nil, fmt.Errorf("recent api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*RecentAPIKeyRow
	for rows.Next() {
		r := &RecentAPIKeyRow{}
		var capsJSON sql.NullString
		if err := rows.Scan(&r.KeyID, &r.UserID, &r.KeyName, &r.Username, &capsJSON, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning recent api key: %w", err)
		}
		if capsJSON.Valid {
			caps, _ := unmarshalCapabilities(capsJSON.String)
			r.Capabilities = caps
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountActiveAPIKeys returns the total count of non-revoked keys with a last_used timestamp
// (total) and those with last_used within window of now (active).
func (s *UserStore) CountActiveAPIKeys(window time.Duration) (total, active int, err error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE last_used IS NOT NULL AND revoked = 0`)
	if err = row.Scan(&total); err != nil {
		return 0, 0, fmt.Errorf("counting total devices: %w", err)
	}
	since := time.Now().Add(-window)
	row = s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE last_used >= ? AND revoked = 0`, since)
	if err = row.Scan(&active); err != nil {
		return 0, 0, fmt.Errorf("counting active devices: %w", err)
	}
	return total, active, nil
}

// APIKeyWithOwner extends APIKey with the owning user's username, used by the admin fleet view.
type APIKeyWithOwner struct {
	APIKey
	OwnerUsername string
}

// ListAllAPIKeys returns all API keys across all users, with owner username and
// profile name joined in. Sorted: most-recently-active first, never-used last.
func (s *UserStore) ListAllAPIKeys() ([]*APIKeyWithOwner, error) {
	rows, err := s.db.Query(`
		SELECT ak.id, ak.user_id, COALESCE(u.username, '') AS owner_username,
		       ak.name, ak.profile_id, COALESCE(ap.name, ''),
		       ak.created_at, ak.last_used, ak.revoked, ak.capabilities
		FROM api_keys ak
		LEFT JOIN users u ON ak.user_id = u.id
		LEFT JOIN agent_profiles ap ON ak.profile_id = ap.id
		ORDER BY CASE WHEN ak.last_used IS NULL THEN 1 ELSE 0 END,
		         ak.last_used DESC, ak.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing all api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*APIKeyWithOwner
	for rows.Next() {
		k := &APIKeyWithOwner{}
		var profileID sql.NullString
		var capsJSON sql.NullString
		var lastUsed sql.NullTime
		if err := rows.Scan(&k.ID, &k.UserID, &k.OwnerUsername, &k.Name,
			&profileID, &k.ProfileName, &k.CreatedAt, &lastUsed, &k.Revoked, &capsJSON); err != nil {
			return nil, fmt.Errorf("scanning api key with owner: %w", err)
		}
		if profileID.Valid {
			k.ProfileID = &profileID.String
		}
		if lastUsed.Valid {
			t := lastUsed.Time
			k.LastUsed = &t
		}
		if capsJSON.Valid {
			caps, _ := unmarshalCapabilities(capsJSON.String)
			k.Capabilities = caps
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
