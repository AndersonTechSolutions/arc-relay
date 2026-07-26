package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/comma-compliance/arc-relay/internal/config"
	"github.com/comma-compliance/arc-relay/internal/docker"
	"github.com/comma-compliance/arc-relay/internal/llm"
	mcpmemory "github.com/comma-compliance/arc-relay/internal/mcp/memory"
	"github.com/comma-compliance/arc-relay/internal/memory"
	"github.com/comma-compliance/arc-relay/internal/memory/extractor"
	"github.com/comma-compliance/arc-relay/internal/middleware"
	"github.com/comma-compliance/arc-relay/internal/oauth"
	"github.com/comma-compliance/arc-relay/internal/proxy"
	"github.com/comma-compliance/arc-relay/internal/recipes"
	"github.com/comma-compliance/arc-relay/internal/server"
	"github.com/comma-compliance/arc-relay/internal/skills"
	"github.com/comma-compliance/arc-relay/internal/skills/checker"
	"github.com/comma-compliance/arc-relay/internal/store"
	"github.com/comma-compliance/arc-relay/internal/web"
	"github.com/comma-compliance/arc-relay/migrations"
	migrationsmemory "github.com/comma-compliance/arc-relay/migrations-memory"
)

func main() {
	// Restrictive umask: all files the relay creates (DBs, WAL/shm files,
	// VACUUM-INTO backups) inherit mode 0600 / dirs 0700. Belt-and-braces
	// alongside store.Open's explicit chmod on the main DB file.
	syscall.Umask(0o077)

	configPath := flag.String("config", "", "path to config file (TOML)")
	flag.Parse()

	// Initialize a default JSON logger before config loads so early errors are structured.
	logLevel := new(slog.LevelVar)
	logLevel.Set(slog.LevelInfo)
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	// Load config
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	// Reinitialize logger with the configured level
	logLevel.Set(cfg.SlogLevel())
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	// Initialize Sentry error tracking
	if cfg.SentryDSN != "" {
		if err := sentry.Init(sentry.ClientOptions{
			Dsn:              cfg.SentryDSN,
			EnableTracing:    false,
			AttachStacktrace: true,
		}); err != nil {
			slog.Warn("sentry init failed", "err", err)
		} else {
			slog.Info("sentry error tracking enabled")
			defer sentry.Flush(2 * time.Second)
		}
	}

	// Open database with embedded migrations
	db, err := store.Open(cfg.Database.Path, migrations.FS)
	if err != nil {
		slog.Error("failed to open database", "err", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	// Initialize stores
	crypto := store.NewConfigEncryptor(cfg.Encryption.Key)
	serverStore := store.NewServerStore(db, crypto)
	userStore := store.NewUserStore(db)
	accessStore := store.NewAccessStore(db)
	profileStore := store.NewProfileStore(db)
	requestLogStore := store.NewRequestLogStore(db)
	sessionStore := store.NewSessionStore(db)

	// Ensure default admin user exists
	adminPw := cfg.Auth.AdminPassword
	if adminPw == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			slog.Error("failed to generate random admin password", "err", err)
			os.Exit(1)
		}
		adminPw = hex.EncodeToString(b)
		// SECURITY: Do not log the generated password in cleartext.
		// It is printed to stderr only so the operator can retrieve it
		// from a secure log sink at startup.
		slog.Warn("no admin password configured, generated random password - set ARC_RELAY_ADMIN_PASSWORD to use a fixed password")
	}
	if err := userStore.EnsureAdmin(adminPw); err != nil {
		slog.Error("failed to ensure admin user", "err", err)
		os.Exit(1)
	}

	// Initialize Docker manager
	dockerMgr, err := docker.NewManager(cfg.Docker.Socket, cfg.Docker.Network)
	if err != nil {
		slog.Warn("docker not available - managed servers will not work, remote servers still available", "err", err)
		dockerMgr = nil
	}

	// Initialize OAuth manager
	oauthMgr := oauth.NewManager(serverStore, cfg.PublicBaseURL())

	// Initialize middleware
	middlewareStore := store.NewMiddlewareStore(db)
	archiveQueueStore := store.NewArchiveQueueStore(db, crypto)
	archiveEventLogger := func(evt *store.MiddlewareEvent) {
		if err := middlewareStore.LogEvent(evt); err != nil {
			slog.Warn("archive dispatcher: failed to log event", "err", err)
		}
	}
	archiveDispatcher := middleware.NewArchiveDispatcher(archiveQueueStore, archiveEventLogger)
	archiveDispatcher.Start()
	mwRegistry := middleware.NewRegistry(middlewareStore, archiveDispatcher)

	// Register custom middleware here. Any type implementing middleware.Middleware
	// can be registered with mwRegistry.Register(descriptor, factoryFunc) and then
	// enabled per-server via the web UI or API. See README.md "Writing Custom
	// Middleware" for a working example.
	//
	// mwRegistry.Register(middleware.Descriptor{
	//     Name: "tenant_tagger", DisplayName: "Tenant Tagger",
	//     Description: "Tags requests with tenant ID",
	//     DefaultPriority: 50, DisplayOrder: 50, Scope: "server",
	// }, mymiddleware.Factory)

	// Initialize proxy manager
	proxyMgr := proxy.NewManager(serverStore, dockerMgr, oauthMgr, accessStore)

	// Auto-start all configured servers
	go func() {
		servers, err := serverStore.List()
		if err != nil {
			slog.Warn("failed to list servers for auto-start", "err", err)
			return
		}
		ctx := context.Background()
		for _, s := range servers {
			if err := proxyMgr.StartServer(ctx, s); err != nil {
				slog.Error("auto-start failed", "server", s.Name, "err", err)
			} else {
				slog.Info("auto-started server", "server", s.Name)
			}
		}
	}()

	// Initialize invite store
	inviteStore := store.NewInviteStore(db)

	// Initialize OAuth token store (for Claude Desktop and other OAuth clients)
	oauthTokenStore := store.NewOAuthTokenStore(db)

	// Start health monitor
	healthMon := proxy.NewHealthMonitor(proxyMgr, serverStore, 30*time.Second)
	healthMon.Start()

	// Open memory database (separate SQLite file to isolate WAL from auth-critical writes)
	memDBPath := cfg.Database.MemoryPath
	if memDBPath == "" {
		memDBPath = filepath.Join(filepath.Dir(cfg.Database.Path), "memory.db")
	}
	memDB, err := store.Open(memDBPath, migrationsmemory.FS)
	if err != nil {
		slog.Error("failed to open memory db", "err", err)
		os.Exit(1)
	}
	defer func() { _ = memDB.Close() }()
	memDB.StartBackup(6 * time.Hour)
	defer memDB.StopBackup()
	slog.Info("memory database opened", "path", memDBPath)

	// Memory subsystem wiring (Task 4).
	messageStore := store.NewMessageStore(memDB)
	sessionMemoryStore := store.NewSessionMemoryStore(memDB)
	extractionStore := store.NewExtractionStore(memDB)
	memSvc := memory.NewService(sessionMemoryStore, messageStore)

	// Phase B: extractor wiring. The mem0 backend is looked up by name
	// ("code-memory") at every Extract call so reconnects don't require
	// a relay restart. If the server isn't registered, Extract returns
	// ErrBackendUnavailable and the cron loop just no-ops on its next cycle.
	extractorSvc := extractor.NewService(sessionMemoryStore, messageStore, extractionStore,
		func() (extractor.Backend, bool) {
			srv, err := serverStore.GetByName("code-memory")
			if err != nil || srv == nil {
				return nil, false
			}
			b, ok := proxyMgr.GetBackend(srv.ID)
			return b, ok
		},
		// Username resolver: relay UUID → username. So mem0 stores under
		// "ian" instead of "363e03f9-...", merging with the user's
		// interactive code-memory namespace.
		func(userID string) (string, bool) {
			u, err := userStore.Get(userID)
			if err != nil || u == nil {
				return "", false
			}
			return u.Username, true
		})

	// Optional LLM classifier for memory categorization (Phase B Level 2).
	// If ARC_RELAY_CLASSIFIER_API_KEY is set, every chunk gets a `category`
	// metadata field (user/feedback/project/reference/none) before being
	// sent to mem0. Adds ~$0.0002/chunk on top of mem0's extraction cost.
	if classifierKey := os.Getenv("ARC_RELAY_CLASSIFIER_API_KEY"); classifierKey != "" {
		classifierModel := os.Getenv("ARC_RELAY_CLASSIFIER_MODEL")
		classifierBaseURL := os.Getenv("ARC_RELAY_CLASSIFIER_BASE_URL")
		extractorSvc.SetClassifier(extractor.NewOpenAIClassifier(
			classifierKey, classifierModel, classifierBaseURL))
		modelLog := classifierModel
		if modelLog == "" {
			modelLog = "gpt-4o-mini"
		}
		baseLog := classifierBaseURL
		if baseLog == "" {
			baseLog = "https://api.openai.com/v1"
		}
		slog.Info("memory extractor classifier enabled",
			"model", modelLog, "base_url", baseLog)
	} else {
		slog.Info("memory extractor classifier not configured (set ARC_RELAY_CLASSIFIER_API_KEY to enable)")
	}
	// extractorSvc.RunCron is started below after ctx is created.

	memHandlers := web.NewMemoryHandlers(memSvc, extractorSvc, func(ctx context.Context) string {
		if u := server.UserFromContext(ctx); u != nil {
			return u.ID
		}
		return ""
	})
	memMcp := mcpmemory.NewServer(memSvc, func(ctx context.Context) string {
		if u := server.UserFromContext(ctx); u != nil {
			return u.ID
		}
		return ""
	})

	// Skill repository wiring (Phase 1).
	skillBundlesDir := cfg.Skills.BundlesDir
	if skillBundlesDir == "" {
		skillBundlesDir = filepath.Join(filepath.Dir(cfg.Database.Path), "skills")
	}
	if err := os.MkdirAll(skillBundlesDir, 0o700); err != nil {
		slog.Error("failed to create skills bundles dir", "path", skillBundlesDir, "err", err)
		os.Exit(1)
	}
	slog.Info("skills bundles dir", "path", skillBundlesDir)
	skillStore := store.NewSkillStore(db)
	skillSvc := skills.New(skillStore, skillBundlesDir)

	// Setup-recipe registry wiring (Phase 1).
	recipeStore := store.NewSetupRecipeStore(db)
	recipeSvc := recipes.New(recipeStore)
	recipeHandlers := web.NewRecipesHandlers(recipeSvc, recipeStore, userStore, server.UserFromContext, server.APIKeyFromContext)

	// Start periodic database backup (every 6 hours, keeps 2 copies)
	db.StartBackup(6 * time.Hour)

	// Periodic cleanup of expired OAuth tokens and refresh tokens
	oauthRefreshStore := store.NewOAuthRefreshTokenStore(db)
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			oauthTokenStore.Cleanup()
			oauthRefreshStore.Cleanup()
		}
	}()

	// Initialize tool optimization stores and LLM client
	optimizeStore := store.NewOptimizeStore(db)
	llmClient := llm.NewClient(cfg.LLM.APIKey, cfg.LLM.Model)
	if llmClient.Available() {
		slog.Info("LLM tool optimizer available", "model", llmClient.Model())
	}
	proxyMgr.OptimizeStore = optimizeStore

	// Skill upstream-update checker (Phase 3). Disabled by default; opt in via
	// TOML [skills.checker] enabled = true OR ARC_RELAY_SKILLS_CHECKER_ENABLED=1.
	// Defaults applied in config.Load (24h interval, <dataDir>/upstream-cache,
	// 60s clone timeout, 32KiB diff cap).
	//
	// We construct the Service even when disabled = false, *if* we want the
	// on-demand POST /api/skills/<slug>/check-drift endpoint to work without
	// the cron loop running. For now we only construct it when enabled, and
	// the HTTP handler returns 503 in the disabled case — operators have to
	// flip the cron on to use the endpoint.
	var skillChecker *checker.Service
	if cfg.Skills.Checker.Enabled {
		skillChecker = checker.NewService(skillStore, skillSvc, llmClient, cfg.Skills.Checker)
	}
	skillHandlers := web.NewSkillsHandlers(skillSvc, skillStore, userStore, skillChecker, server.UserFromContext, server.APIKeyFromContext)

	// Start HTTP server
	srv := server.New(cfg, serverStore, userStore, proxyMgr, oauthMgr, accessStore, profileStore, requestLogStore, sessionStore, middlewareStore, mwRegistry, healthMon, inviteStore, oauthTokenStore, optimizeStore, llmClient, messageStore, sessionMemoryStore, memHandlers, memMcp, memSvc, skillStore, skillSvc, skillHandlers, recipeStore, recipeSvc, recipeHandlers)

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Phase B: cron extraction backstop. Cancels when ctx is canceled at
	// shutdown. Logs cycle stats at INFO every 30 min.
	go extractorSvc.RunCron(ctx, 30*time.Minute)

	if skillChecker != nil {
		go skillChecker.RunCron(ctx, cfg.Skills.Checker.Interval)
		slog.Info("skill checker enabled",
			"interval", cfg.Skills.Checker.Interval,
			"cache_dir", cfg.Skills.Checker.UpstreamCacheDir)
	} else {
		slog.Info("skill checker disabled (set ARC_RELAY_SKILLS_CHECKER_ENABLED=1 to enable)")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		slog.Info("shutting down")
		healthMon.Stop()
		archiveDispatcher.Stop()
		db.StopBackup()
		proxyMgr.StopAll(ctx)
		if dockerMgr != nil {
			_ = dockerMgr.Close()
		}
		// Close DB explicitly before exiting so WAL is checkpointed cleanly.
		if err := db.Close(); err != nil {
			slog.Warn("error closing database", "err", err)
		}
		os.Exit(0)
	}()

	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
