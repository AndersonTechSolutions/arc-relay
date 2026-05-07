package store_test

import (
	"testing"
	"time"

	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/testutil"
)

func TestUserCreate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	user, err := users.Create("alice", "password123", "user")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if user.ID == "" {
		t.Error("user ID should be generated")
	}
	if user.Username != "alice" {
		t.Errorf("Username = %q, want %q", user.Username, "alice")
	}
	if user.Role != "user" {
		t.Errorf("Role = %q, want %q", user.Role, "user")
	}
	if user.AccessLevel != "write" {
		t.Errorf("AccessLevel = %q, want %q (default)", user.AccessLevel, "write")
	}
	if user.PasswordHash == "" {
		t.Error("PasswordHash should be set")
	}
	if user.PasswordHash == "password123" {
		t.Error("PasswordHash should not be plaintext")
	}
}

func TestUserCreateDuplicate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	_, err := users.Create("alice", "pass1", "user")
	if err != nil {
		t.Fatalf("first Create() error = %v", err)
	}

	_, err = users.Create("alice", "pass2", "user")
	if err == nil {
		t.Error("duplicate Create() should return error")
	}
}

func TestCreateWithAccessLevel(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	t.Run("admin role forces admin access level", func(t *testing.T) {
		user, err := users.CreateWithAccessLevel("admin1", "pass", "admin", "read", nil)
		if err != nil {
			t.Fatalf("CreateWithAccessLevel() error = %v", err)
		}
		if user.AccessLevel != "admin" {
			t.Errorf("AccessLevel = %q, want %q (forced by admin role)", user.AccessLevel, "admin")
		}
	})

	t.Run("explicit access level for non-admin", func(t *testing.T) {
		user, err := users.CreateWithAccessLevel("reader", "pass", "user", "read", nil)
		if err != nil {
			t.Fatalf("CreateWithAccessLevel() error = %v", err)
		}
		if user.AccessLevel != "read" {
			t.Errorf("AccessLevel = %q, want %q", user.AccessLevel, "read")
		}
	})
}

func TestAuthenticate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	_, err := users.Create("bob", "correct-password", "user")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	t.Run("valid password", func(t *testing.T) {
		user, err := users.Authenticate("bob", "correct-password")
		if err != nil {
			t.Fatalf("Authenticate() error = %v", err)
		}
		if user == nil {
			t.Fatal("Authenticate() returned nil for valid credentials")
		}
		if user.Username != "bob" {
			t.Errorf("Username = %q, want %q", user.Username, "bob")
		}
	})

	t.Run("invalid password", func(t *testing.T) {
		user, err := users.Authenticate("bob", "wrong-password")
		if err != nil {
			t.Fatalf("Authenticate() error = %v", err)
		}
		if user != nil {
			t.Error("Authenticate() should return nil for invalid password")
		}
	})

	t.Run("nonexistent user", func(t *testing.T) {
		user, err := users.Authenticate("nonexistent", "password")
		if err != nil {
			t.Fatalf("Authenticate() error = %v", err)
		}
		if user != nil {
			t.Error("Authenticate() should return nil for nonexistent user")
		}
	})
}

func TestUserGetAndGetByUsername(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	created, _ := users.Create("charlie", "pass", "user")

	t.Run("Get found", func(t *testing.T) {
		user, err := users.Get(created.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if user == nil {
			t.Fatal("Get() returned nil")
		}
		if user.Username != "charlie" {
			t.Errorf("Username = %q, want %q", user.Username, "charlie")
		}
	})

	t.Run("Get not found", func(t *testing.T) {
		user, err := users.Get("nonexistent-id")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if user != nil {
			t.Error("Get() should return nil for nonexistent ID")
		}
	})

	t.Run("GetByUsername found", func(t *testing.T) {
		user, err := users.GetByUsername("charlie")
		if err != nil {
			t.Fatalf("GetByUsername() error = %v", err)
		}
		if user == nil {
			t.Fatal("GetByUsername() returned nil")
		}
		if user.ID != created.ID {
			t.Errorf("ID = %q, want %q", user.ID, created.ID)
		}
	})

	t.Run("GetByUsername not found", func(t *testing.T) {
		user, err := users.GetByUsername("nonexistent")
		if err != nil {
			t.Fatalf("GetByUsername() error = %v", err)
		}
		if user != nil {
			t.Error("GetByUsername() should return nil for nonexistent username")
		}
	})
}

func TestUserList(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	if _, err := users.Create("user1", "pass", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := users.Create("user2", "pass", "admin"); err != nil {
		t.Fatal(err)
	}

	list, err := users.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 {
		t.Errorf("List() returned %d users, want 2", len(list))
	}
}

func TestUserDeleteCascadesAPIKeys(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	user, err := users.Create("deleteme", "pass", "user")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := users.CreateAPIKey(user.ID, "my-key", nil, nil); err != nil {
		t.Fatal(err)
	}

	// Verify key exists
	keys, _ := users.ListAPIKeys(user.ID)
	if len(keys) != 1 {
		t.Fatalf("expected 1 API key before delete, got %d", len(keys))
	}

	if err := users.Delete(user.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// User should be gone
	found, _ := users.Get(user.ID)
	if found != nil {
		t.Error("user should be deleted")
	}

	// API keys should be cascade-deleted
	var keyCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM api_keys WHERE user_id = ?", user.ID).Scan(&keyCount); err != nil {
		t.Fatal(err)
	}
	if keyCount != 0 {
		t.Errorf("API keys should be cascade-deleted, got %d remaining", keyCount)
	}
}

func TestEnsureAdmin(t *testing.T) {
	t.Run("creates admin when none exist", func(t *testing.T) {
		db := testutil.OpenTestDB(t)
		users := store.NewUserStore(db)

		if err := users.EnsureAdmin("admin-pass"); err != nil {
			t.Fatalf("EnsureAdmin() error = %v", err)
		}

		admin, err := users.GetByUsername("admin")
		if err != nil {
			t.Fatalf("GetByUsername() error = %v", err)
		}
		if admin == nil {
			t.Fatal("admin user should have been created")
		}
		if admin.Role != "admin" {
			t.Errorf("Role = %q, want %q", admin.Role, "admin")
		}
		if admin.AccessLevel != "admin" {
			t.Errorf("AccessLevel = %q, want %q", admin.AccessLevel, "admin")
		}
	})

	t.Run("idempotent when users exist", func(t *testing.T) {
		db := testutil.OpenTestDB(t)
		users := store.NewUserStore(db)

		if _, err := users.Create("existing", "pass", "user"); err != nil {
			t.Fatal(err)
		}

		if err := users.EnsureAdmin("admin-pass"); err != nil {
			t.Fatalf("EnsureAdmin() error = %v", err)
		}

		// Should not create another admin
		list, _ := users.List()
		if len(list) != 1 {
			t.Errorf("user count = %d, want 1 (no new admin created)", len(list))
		}
	})
}

func TestAPIKeyRoundTrip(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	user, _ := users.Create("apiuser", "pass", "user")

	rawKey, ak, err := users.CreateAPIKey(user.ID, "test-key", nil, nil)
	if err != nil {
		t.Fatalf("CreateAPIKey() error = %v", err)
	}
	if rawKey == "" {
		t.Error("rawKey should not be empty")
	}
	if ak.ID == "" {
		t.Error("APIKey ID should be generated")
	}
	if ak.Name != "test-key" {
		t.Errorf("Name = %q, want %q", ak.Name, "test-key")
	}

	// Validate the raw key
	validated, validatedKey, err := users.ValidateAPIKey(rawKey)
	if err != nil {
		t.Fatalf("ValidateAPIKey() error = %v", err)
	}
	if validated == nil {
		t.Fatal("ValidateAPIKey() returned nil user for valid key")
	}
	if validated.ID != user.ID {
		t.Errorf("validated user ID = %q, want %q", validated.ID, user.ID)
	}
	if validatedKey == nil {
		t.Fatal("ValidateAPIKey() returned nil api_key for valid key")
	}
	if validatedKey.ID != ak.ID {
		t.Errorf("validated api_key ID = %q, want %q", validatedKey.ID, ak.ID)
	}
}

func TestValidateAPIKeyInvalid(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	user, ak, err := users.ValidateAPIKey("nonexistent-key")
	if err != nil {
		t.Fatalf("ValidateAPIKey() error = %v", err)
	}
	if user != nil {
		t.Error("ValidateAPIKey() should return nil user for nonexistent key")
	}
	if ak != nil {
		t.Error("ValidateAPIKey() should return nil api_key for nonexistent key")
	}
}

func TestCountActiveAPIKeys(t *testing.T) {
	db := testutil.OpenTestDB(t)
	st := store.NewUserStore(db)
	u, _ := st.Create("device-user", "pw", "user")

	_, k1, _ := st.CreateAPIKey(u.ID, "key1", nil, nil)
	_, k2, _ := st.CreateAPIKey(u.ID, "key2", nil, nil)

	now := time.Now()
	_, _ = db.Exec(`UPDATE api_keys SET last_used = ? WHERE id = ?`, now, k1.ID)
	_, _ = db.Exec(`UPDATE api_keys SET last_used = ? WHERE id = ?`, now.Add(-2*time.Hour), k2.ID)

	total, active, err := st.CountActiveAPIKeys(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("want total=2, got %d", total)
	}
	if active != 1 {
		t.Errorf("want active=1 (only k1 within last hour), got %d", active)
	}
}

func TestListAllAPIKeys(t *testing.T) {
	db := testutil.OpenTestDB(t)
	st := store.NewUserStore(db)

	alice, _ := st.Create("lak-alice", "pw", "user")
	bob, _ := st.Create("lak-bob", "pw", "user")

	_, ka, _ := st.CreateAPIKey(alice.ID, "alice-key", nil, nil)
	_, _, _ = st.CreateAPIKey(bob.ID, "bob-key", nil, nil)

	// Alice's key recently used; Bob's never used.
	_, _ = db.Exec(`UPDATE api_keys SET last_used = ? WHERE id = ?`, time.Now(), ka.ID)

	keys, err := st.ListAllAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("want 2 keys, got %d", len(keys))
	}
	// Recently-used key (alice's) must come first.
	if keys[0].OwnerUsername != "lak-alice" {
		t.Errorf("want lak-alice first, got %s", keys[0].OwnerUsername)
	}
	if keys[1].OwnerUsername != "lak-bob" {
		t.Errorf("want lak-bob second, got %s", keys[1].OwnerUsername)
	}
}

func TestValidateAPIKeyRevoked(t *testing.T) {
	db := testutil.OpenTestDB(t)
	users := store.NewUserStore(db)

	user, _ := users.Create("revokeuser", "pass", "user")
	rawKey, ak, _ := users.CreateAPIKey(user.ID, "to-revoke", nil, nil)

	if err := users.RevokeAPIKey(ak.ID); err != nil {
		t.Fatalf("RevokeAPIKey() error = %v", err)
	}

	// Validate should return nil for revoked key
	validated, validatedKey, err := users.ValidateAPIKey(rawKey)
	if err != nil {
		t.Fatalf("ValidateAPIKey() error = %v", err)
	}
	if validated != nil {
		t.Error("ValidateAPIKey() should return nil user for revoked key")
	}
	if validatedKey != nil {
		t.Error("ValidateAPIKey() should return nil api_key for revoked key")
	}
}
