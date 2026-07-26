package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/comma-compliance/arc-relay/internal/cli/config"
)

// resolveSelfUpdateRelayURL returns the relay URL for the self-update
// download, using a priority order tighter than ResolveCredentials:
//
//  1. $ARC_SYNC_URL env var (so cron jobs and minimal hosts work without
//     a config file or an API key — /download/ is unauthenticated)
//  2. config.json's relay_url field (without requiring api_key)
//
// We deliberately avoid config.ResolveCredentials here because it requires
// both URL and API key together, but self-update never authenticates.
func resolveSelfUpdateRelayURL(configDir string) (string, error) {
	if v := os.Getenv("ARC_SYNC_URL"); v != "" {
		return v, nil
	}
	path := config.ConfigPath(configDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no relay URL: set $ARC_SYNC_URL or run 'arc-sync init'")
		}
		return "", fmt.Errorf("reading config %s: %w", path, err)
	}
	var cfg struct {
		RelayURL string `json:"relay_url"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parsing config %s: %w", path, err)
	}
	if cfg.RelayURL == "" {
		return "", fmt.Errorf("config %s: relay_url is empty", path)
	}
	return cfg.RelayURL, nil
}

// selfUpdateBinaryName returns the relay /download/<binary> path component for
// the current platform — matches the asset names goreleaser produces and the
// allowlist in internal/web/device_auth.go.
func selfUpdateBinaryName(goos, goarch string) string {
	suffix := ""
	if goos == "windows" {
		suffix = ".exe"
	}
	return fmt.Sprintf("arc-sync-%s-%s%s", goos, goarch, suffix)
}

// selfUpdateMinSize is the floor below which we refuse to install a download.
// Real arc-sync binaries are ~7 MiB; anything smaller is a captive-portal
// HTML page, a redirect that didn't follow, or a corrupt partial transfer.
const selfUpdateMinSize = 1 << 20 // 1 MiB

func runSelfUpdate() {
	dryRun := false
	jsonOut := false
	for _, a := range os.Args[2:] {
		switch a {
		case "--dry-run":
			dryRun = true
		case "--json":
			jsonOut = true
		case "--help", "-h":
			fmt.Println(`Usage: arc-sync self-update [flags]

Replace the running arc-sync binary with the latest version from the
configured relay's /download/ endpoint. Atomic replace via same-directory
rename. No-op when the served binary's hash matches the current install.

Flags:
  --dry-run     Fetch + hash but don't replace; print what would happen
  --json        Emit a single JSON status line instead of human text`)
			return
		default:
			fmt.Fprintf(os.Stderr, "self-update: unknown flag %q\n", a)
			os.Exit(2)
		}
	}

	configDir, err := config.DefaultConfigDir()
	if err != nil {
		failSelfUpdate(jsonOut, "config dir: "+err.Error())
	}
	relayURL, err := resolveSelfUpdateRelayURL(configDir)
	if err != nil {
		failSelfUpdate(jsonOut, err.Error())
	}

	binName := selfUpdateBinaryName(runtime.GOOS, runtime.GOARCH)
	url := strings.TrimRight(relayURL, "/") + "/download/" + binName

	selfPath, err := os.Executable()
	if err != nil {
		failSelfUpdate(jsonOut, "resolve own path: "+err.Error())
	}
	if resolved, err := filepath.EvalSymlinks(selfPath); err == nil {
		selfPath = resolved
	}

	dir := filepath.Dir(selfPath)
	tmp, err := os.CreateTemp(dir, ".arc-sync-update-*")
	if err != nil {
		failSelfUpdate(jsonOut, "tempfile: "+err.Error())
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	if !jsonOut {
		fmt.Fprintf(os.Stderr, "Fetching %s\n", url)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		cleanupTmp()
		failSelfUpdate(jsonOut, "GET: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		cleanupTmp()
		failSelfUpdate(jsonOut, fmt.Sprintf("relay returned HTTP %d for %s", resp.StatusCode, url))
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		failSelfUpdate(jsonOut, "download: "+err.Error())
	}
	if n < selfUpdateMinSize {
		_ = os.Remove(tmpPath)
		failSelfUpdate(jsonOut, fmt.Sprintf("suspiciously small download (%d bytes; expected >=%d) — relay misconfigured?", n, selfUpdateMinSize))
	}
	newHash := hex.EncodeToString(h.Sum(nil))

	oldHash := ""
	if oldBytes, err := os.ReadFile(selfPath); err == nil {
		oh := sha256.Sum256(oldBytes)
		oldHash = hex.EncodeToString(oh[:])
	}

	if newHash == oldHash {
		_ = os.Remove(tmpPath)
		emitSelfUpdate(jsonOut, "up_to_date", oldHash, newHash, selfPath)
		return
	}

	if dryRun {
		_ = os.Remove(tmpPath)
		emitSelfUpdate(jsonOut, "would_update", oldHash, newHash, selfPath)
		return
	}

	// Match the running binary's mode (preserves +x without assuming 0755).
	mode := os.FileMode(0o755)
	if info, err := os.Stat(selfPath); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = os.Remove(tmpPath)
		failSelfUpdate(jsonOut, "chmod: "+err.Error())
	}

	// Atomic replace within the same directory — POSIX rename(2) is atomic.
	// On Windows os.Rename does best-effort (MoveFile with replace flag).
	if err := os.Rename(tmpPath, selfPath); err != nil {
		_ = os.Remove(tmpPath)
		failSelfUpdate(jsonOut, "rename: "+err.Error())
	}

	emitSelfUpdate(jsonOut, "updated", oldHash, newHash, selfPath)
	// (Earlier versions printed a "run codesign --force --sign -" hint here.
	// In practice the rename above creates a fresh inode rather than mutating
	// the existing one, which sidesteps the amfid SIGKILL behavior described
	// in macos_amfid_sigkill_replaced_binary memory — verified by repeated
	// runs on darwin without ever needing manual re-signing. Hint removed.)
}

func failSelfUpdate(jsonOut bool, msg string) {
	if jsonOut {
		fmt.Printf(`{"status":"error","error":%q}`+"\n", msg)
	} else {
		fmt.Fprintf(os.Stderr, "self-update: %s\n", msg)
	}
	os.Exit(1)
}

func emitSelfUpdate(jsonOut bool, status, oldHash, newHash, path string) {
	if jsonOut {
		fmt.Printf(`{"status":%q,"old_hash":%q,"new_hash":%q,"path":%q}`+"\n", status, oldHash, newHash, path)
		return
	}
	short := func(s string) string {
		if len(s) >= 16 {
			return s[:16]
		}
		return s
	}
	switch status {
	case "up_to_date":
		fmt.Printf("arc-sync up to date (%s)\n", short(newHash))
	case "would_update":
		fmt.Printf("[dry-run] would update arc-sync: %s -> %s\n", short(oldHash), short(newHash))
		fmt.Printf("Path: %s\n", path)
	case "updated":
		fmt.Printf("arc-sync updated: %s -> %s\n", short(oldHash), short(newHash))
		fmt.Printf("Path: %s\n", path)
	}
}
