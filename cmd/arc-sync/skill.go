// arc-sync skill subcommands. Mirrors cmd/arc-sync/main.go's runMemory shape:
// a top-level dispatcher that picks a subcommand handler based on os.Args[2].
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/comma-compliance/arc-relay/internal/cli/config"
	"github.com/comma-compliance/arc-relay/internal/cli/relay"
	"github.com/comma-compliance/arc-relay/internal/cli/sync"
)

func runSkill() {
	if len(os.Args) < 3 {
		printSkillUsage()
		os.Exit(1)
	}
	switch os.Args[2] {
	case "list":
		runSkillList()
	case "install":
		runSkillInstall()
	case "remove", "rm", "uninstall":
		runSkillRemove()
	case "sync":
		runSkillSync()
	case "push":
		runSkillPush()
	case "assign":
		runSkillAssign()
	case "unassign":
		runSkillUnassign()
	case "check-updates":
		runSkillCheckUpdates()
	case "set-upstream":
		runSkillSetUpstream()
	case "clear-upstream":
		runSkillClearUpstream()
	case "edit":
		runSkillEdit()
	case "--help", "-h", "help":
		printSkillUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown skill subcommand: %s\n", os.Args[2])
		printSkillUsage()
		os.Exit(1)
	}
}

func printSkillUsage() {
	fmt.Println(`Usage: arc-sync skill <command> [args]

Commands:
  list [--installed|--remote|--assigned] [--json]
                        Show skills. --installed: local only (default).
                        --remote: full relay catalog. --assigned: relay's view of
                        what you should have installed.
  install <slug> [--version VERSION]
                        Pull a skill from the relay and install it under
                        ~/.claude/skills/<slug>/. Defaults to the latest version.
  remove <slug>         Remove an arc-sync-managed skill. Hand-installed skill
                        directories are refused (no .arc-sync-version marker).
  sync [--dry-run]      Reconcile ~/.claude/skills/ against the relay's assigned
                        list: install missing skills, update outdated ones,
                        remove skills no longer assigned. --dry-run prints
                        actions without performing them.
  push <dir> [--version V] [--visibility public|restricted]
                        Admin-only: package <dir> as a tar.gz and upload.
                        <dir> must contain SKILL.md at its root.
  assign <slug> <username> [--version V]
                        Admin-only: grant <username> access to a restricted
                        skill. Optional --version pins them to a specific
                        version (default: follow latest). Idempotent.
  unassign <slug> <username>
                        Admin-only: revoke <username>'s access to a skill.
  check-updates [<slug>]
                        Ask the relay to compare each tracked skill against its
                        recorded upstream and report drift. With <slug>, checks
                        a single skill and prints a one-line summary plus the
                        recommended action. Without args, iterates every skill
                        on the relay; skills without upstream tracking are
                        skipped silently.
  set-upstream <slug> --git-url URL [--path SUBPATH] [--ref REF]
                        Admin-only. Set or replace the upstream-tracking row
                        for an existing skill without re-uploading. Use this
                        to enable drift detection on a skill pushed with
                        --no-upstream, or to fix a typo'd path/ref.
  clear-upstream <slug>
                        Admin-only. Remove upstream tracking for a skill.
                        Future drift checker runs skip it silently and any
                        stale "outdated" flag is cleared.
  edit <slug> [--visibility public|restricted] [--display-name TEXT] [--description TEXT]
                        Admin-only. Patch a skill's mutable metadata in place
                        without bumping its version. Any subset of flags can
                        be set; omitted fields keep their current values.
                        Visibility flip propagates immediately to the read-side
                        ACL — restricting a public skill drops every user who
                        had access via visibility=public.

Skills install to ~/.claude/skills/<slug>/. arc-sync only touches directories
it created (those carrying a .arc-sync-version marker file); manually-installed
skills are left alone during sync.`)
}

func newSkillManager() *sync.SkillManager {
	configDir, err := config.DefaultConfigDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	creds, err := config.ResolveCredentials(configDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	skillsDir, err := sync.DefaultSkillsDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return &sync.SkillManager{
		Client: &relay.Client{
			BaseURL:    strings.TrimRight(creds.RelayURL, "/"),
			APIKey:     creds.APIKey,
			HTTPClient: &http.Client{Timeout: 60 * time.Second},
		},
		SkillsDir: skillsDir,
	}
}

func runSkillList() {
	args := os.Args[3:]
	jsonOut := hasFlagInArgs(args, "--json")
	mode := "installed"
	switch {
	case hasFlagInArgs(args, "--remote"):
		mode = "remote"
	case hasFlagInArgs(args, "--assigned"):
		mode = "assigned"
	case hasFlagInArgs(args, "--installed"):
		mode = "installed"
	}
	mgr := newSkillManager()

	switch mode {
	case "installed":
		rows, err := mgr.ListInstalled()
		if err != nil {
			fmt.Fprintln(os.Stderr, "skill list:", err)
			os.Exit(1)
		}
		if jsonOut {
			emitJSON(rows)
			return
		}
		if len(rows) == 0 {
			fmt.Println("No skills installed in ~/.claude/skills/.")
			return
		}
		fmt.Printf("%-32s  %-10s  %s\n", "SLUG", "VERSION", "STATUS")
		for _, r := range rows {
			status := "managed"
			if !r.Managed {
				status = "hand-installed (not arc-sync managed)"
			}
			version := r.Version
			if version == "" {
				version = "—"
			}
			fmt.Printf("%-32s  %-10s  %s\n", r.Slug, version, status)
		}
	case "remote":
		skills, err := mgr.Client.ListSkills()
		if err != nil {
			fmt.Fprintln(os.Stderr, "skill list:", err)
			os.Exit(1)
		}
		if jsonOut {
			emitJSON(skills)
			return
		}
		if len(skills) == 0 {
			fmt.Println("No skills published on the relay yet.")
			return
		}
		fmt.Printf("%-32s  %-10s  %-12s  %s\n", "SLUG", "VERSION", "VISIBILITY", "STATUS")
		for _, s := range skills {
			status := "active"
			switch {
			case s.YankedAt != nil:
				status = "yanked"
			case s.Outdated == 1:
				if s.Drift != nil && s.Drift.Severity != "" {
					status = "outdated · " + s.Drift.Severity
				} else {
					status = "outdated"
				}
			}
			ver := s.LatestVersion
			if ver == "" {
				ver = "—"
			}
			fmt.Printf("%-32s  %-10s  %-12s  %s\n", s.Slug, ver, s.Visibility, status)
		}
	case "assigned":
		assigned, err := mgr.Client.ListAssignedSkills()
		if err != nil {
			fmt.Fprintln(os.Stderr, "skill list:", err)
			os.Exit(1)
		}
		if jsonOut {
			emitJSON(assigned)
			return
		}
		if len(assigned) == 0 {
			fmt.Println("Relay reports no skills assigned to you.")
			return
		}
		fmt.Printf("%-32s  %-10s  %-12s\n", "SLUG", "VERSION", "VISIBILITY")
		for _, a := range assigned {
			ver := a.Skill.LatestVersion
			if a.PinnedVersion != nil && *a.PinnedVersion != "" {
				ver = *a.PinnedVersion + " (pinned)"
			}
			fmt.Printf("%-32s  %-10s  %-12s\n", a.Skill.Slug, ver, a.Skill.Visibility)
		}
	}
}

func runSkillInstall() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill install <slug> [--version VERSION]")
		os.Exit(1)
	}
	slug := args[0]
	version := getFlagValue(args[1:], "--version")
	mgr := newSkillManager()

	if version == "" {
		// Resolve "latest" via the relay so we can record the concrete version
		// in the marker. Doing this client-side keeps the relay's redirect
		// surface area smaller (no /api/skills/{slug}/versions/latest endpoint).
		detail, err := mgr.Client.GetSkill(slug)
		if err != nil {
			fmt.Fprintln(os.Stderr, "resolve skill:", err)
			os.Exit(1)
		}
		if detail == nil {
			fmt.Fprintf(os.Stderr, "skill %q not found on relay\n", slug)
			os.Exit(1)
		}
		if detail.Skill.LatestVersion == "" {
			fmt.Fprintf(os.Stderr, "skill %q has no published versions\n", slug)
			os.Exit(1)
		}
		version = detail.Skill.LatestVersion
	}

	marker, err := mgr.Install(slug, version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill install:", err)
		os.Exit(1)
	}
	fmt.Printf("Installed %s@%s into %s/%s/\n", marker.Slug, marker.Version, mgr.SkillsDir, marker.Slug)
}

func runSkillRemove() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill remove <slug>")
		os.Exit(1)
	}
	slug := args[0]
	mgr := newSkillManager()
	if err := mgr.Remove(slug); err != nil {
		fmt.Fprintln(os.Stderr, "skill remove:", err)
		os.Exit(1)
	}
	fmt.Printf("Removed %s from %s/.\n", slug, mgr.SkillsDir)
}

func runSkillSync() {
	args := os.Args[3:]
	dryRun := hasFlagInArgs(args, "--dry-run")
	jsonOut := hasFlagInArgs(args, "--json")
	mgr := newSkillManager()

	report, err := mgr.Sync(sync.SkillSyncOptions{DryRun: dryRun})
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill sync:", err)
		os.Exit(1)
	}
	if jsonOut {
		emitJSON(report)
		if len(report.Errors) > 0 {
			os.Exit(1)
		}
		return
	}
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	for _, a := range report.Installed {
		fmt.Printf("%sinstall %s@%s\n", prefix, a.Slug, a.Version)
	}
	for _, a := range report.Updated {
		fmt.Printf("%supdate  %s: %s → %s\n", prefix, a.Slug, a.Previous, a.Version)
	}
	for _, a := range report.Removed {
		fmt.Printf("%sremove  %s (was %s)\n", prefix, a.Slug, a.Version)
	}
	for _, a := range report.Unchanged {
		fmt.Printf("%sok      %s@%s\n", prefix, a.Slug, a.Version)
	}
	for _, a := range report.SkippedHand {
		fmt.Printf("%sskip    %s (hand-installed; not arc-sync-managed)\n", prefix, a.Slug)
	}
	if len(report.Errors) > 0 {
		fmt.Println()
		for _, e := range report.Errors {
			fmt.Fprintf(os.Stderr, "error %s: %s\n", e.Slug, e.Message)
		}
		os.Exit(1)
	}
	if len(report.Installed)+len(report.Updated)+len(report.Removed) == 0 && !dryRun {
		fmt.Println("Nothing to do — already in sync.")
	}
}

func runSkillPush() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill push <dir> [--version V] [--visibility public|restricted] [--upstream-git URL [--upstream-path PATH] [--upstream-ref REF]] [--no-upstream]")
		os.Exit(1)
	}
	dir := args[0]
	rest := args[1:]
	version := getFlagValue(rest, "--version")
	visibility := getFlagValue(rest, "--visibility")
	upstreamGit := getFlagValue(rest, "--upstream-git")
	upstreamPath := getFlagValue(rest, "--upstream-path")
	upstreamRef := getFlagValue(rest, "--upstream-ref")
	noUpstream := hasFlagInArgs(rest, "--no-upstream")
	if version == "" {
		fmt.Fprintln(os.Stderr, "skill push: --version is required (semver MAJOR.MINOR.PATCH)")
		os.Exit(1)
	}

	// Resolve upstream metadata before packaging so a malformed sidecar
	// fails fast — no point spending IO on a tarball we can't push.
	upstream, err := sync.LoadAndMerge(dir, upstreamGit, upstreamPath, upstreamRef, noUpstream)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill push:", err)
		os.Exit(1)
	}

	archive, slug, err := sync.PackageSkill(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill push:", err)
		os.Exit(1)
	}
	mgr := newSkillManager()
	res, err := mgr.Client.UploadSkill(slug, version, visibility, archive, upstream.ToWire())
	if err != nil {
		fmt.Fprintln(os.Stderr, "skill push:", err)
		os.Exit(1)
	}
	fmt.Printf("Published %s@%s (%d bytes, sha256=%s)\n",
		res.Skill.Slug, res.Version.Version, res.Version.ArchiveSize, res.Version.ArchiveSHA256)
}

func runSkillAssign() {
	args := os.Args[3:]
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill assign <slug> <username> [--version V]")
		os.Exit(1)
	}
	slug := args[0]
	username := args[1]
	version := getFlagValue(args[2:], "--version")
	mgr := newSkillManager()
	if err := mgr.Client.AssignSkill(slug, username, version); err != nil {
		fmt.Fprintln(os.Stderr, "skill assign:", err)
		os.Exit(1)
	}
	if version != "" {
		fmt.Printf("Granted %s access to skill %s pinned at version %s\n", username, slug, version)
	} else {
		fmt.Printf("Granted %s access to skill %s (follows latest)\n", username, slug)
	}
}

func runSkillUnassign() {
	args := os.Args[3:]
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill unassign <slug> <username>")
		os.Exit(1)
	}
	slug := args[0]
	username := args[1]
	mgr := newSkillManager()
	if err := mgr.Client.UnassignSkill(slug, username); err != nil {
		fmt.Fprintln(os.Stderr, "skill unassign:", err)
		os.Exit(1)
	}
	fmt.Printf("Revoked %s access to skill %s\n", username, slug)
}

// upstreamClient narrows the relay.Client surface used by the set/clear-
// upstream subcommands so the dispatcher tests can swap in a fake without
// constructing an httptest server. The real *relay.Client satisfies it.
type upstreamClient interface {
	SetUpstream(slug string, in *relay.SetUpstreamInput) (map[string]any, error)
	ClearUpstream(slug string) error
}

// editClient narrows the relay.Client surface used by `arc-sync skill edit`.
// Same fake-injection rationale as upstreamClient.
type editClient interface {
	PatchSkill(slug string, in *relay.PatchSkillInput) (*relay.Skill, error)
}

// runSkillEdit implements `arc-sync skill edit <slug> [--visibility V]
// [--display-name TEXT] [--description TEXT]`. Each flag is optional; at
// least one must be present.
func runSkillEdit() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill edit <slug> [--visibility public|restricted] [--display-name TEXT] [--description TEXT]")
		os.Exit(1)
	}
	slug := args[0]
	rest := args[1:]

	in := &relay.PatchSkillInput{}
	if hasFlagInArgs(rest, "--visibility") {
		v := getFlagValue(rest, "--visibility")
		in.Visibility = &v
	}
	if hasFlagInArgs(rest, "--display-name") {
		v := getFlagValue(rest, "--display-name")
		in.DisplayName = &v
	}
	if hasFlagInArgs(rest, "--description") {
		v := getFlagValue(rest, "--description")
		in.Description = &v
	}

	if in.Visibility == nil && in.DisplayName == nil && in.Description == nil {
		fmt.Fprintln(os.Stderr, "skill edit: at least one of --visibility, --display-name, --description is required")
		os.Exit(1)
	}

	mgr := newSkillManager()
	if err := editSkill(mgr.Client, slug, in, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}

// editSkill is the testable core of runSkillEdit.
func editSkill(c editClient, slug string, in *relay.PatchSkillInput, stdout, stderr io.Writer) error {
	updated, err := c.PatchSkill(slug, in)
	if err != nil {
		printSkillEditError(stderr, slug, err)
		return err
	}
	fmt.Fprintf(stdout, "Updated %s: visibility=%s display=%q\n",
		updated.Slug, updated.Visibility, updated.DisplayName)
	return nil
}

// printSkillEditError pretty-prints SkillHTTPError statuses for the edit path.
// Mirrors printSetUpstreamError's shape so the two CLI commands feel uniform.
func printSkillEditError(stderr io.Writer, slug string, err error) {
	var httpErr *relay.SkillHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Status {
		case http.StatusBadRequest:
			fmt.Fprintf(stderr, "skill %s: bad request — visibility must be public/restricted, display_name non-empty\n", slug)
			return
		case http.StatusForbidden:
			fmt.Fprintf(stderr, "skill %s: forbidden — admin role or skills:write API key required\n", slug)
			return
		case http.StatusNotFound:
			fmt.Fprintf(stderr, "skill %s: not found on relay\n", slug)
			return
		case http.StatusUnsupportedMediaType:
			fmt.Fprintf(stderr, "skill %s: relay rejected request body (likely a client/server mismatch)\n", slug)
			return
		}
	}
	fmt.Fprintf(stderr, "skill %s: %s\n", slug, err)
}

// runSkillSetUpstream implements `arc-sync skill set-upstream <slug>
// --git-url URL [--path SUBPATH] [--ref REF]`.
func runSkillSetUpstream() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill set-upstream <slug> --git-url URL [--path SUBPATH] [--ref REF]")
		os.Exit(1)
	}
	slug := args[0]
	rest := args[1:]
	gitURL := getFlagValue(rest, "--git-url")
	subpath := getFlagValue(rest, "--path")
	ref := getFlagValue(rest, "--ref")
	if gitURL == "" {
		fmt.Fprintln(os.Stderr, "skill set-upstream: --git-url is required")
		os.Exit(1)
	}
	mgr := newSkillManager()
	if err := setUpstream(mgr.Client, slug, gitURL, subpath, ref, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}

// setUpstream is the testable core of runSkillSetUpstream. It takes an
// upstreamClient (interface so tests can inject a fake) plus explicit writers
// for stdout/stderr.
func setUpstream(c upstreamClient, slug, gitURL, subpath, ref string, stdout, stderr io.Writer) error {
	resp, err := c.SetUpstream(slug, &relay.SetUpstreamInput{
		GitURL:  gitURL,
		Subpath: subpath,
		Ref:     ref,
	})
	if err != nil {
		printSetUpstreamError(stderr, slug, err)
		return err
	}
	gotURL, _ := resp["git_url"].(string)
	gotPath, _ := resp["git_subpath"].(string)
	gotRef, _ := resp["git_ref"].(string)
	if gotPath == "" {
		gotPath = "(repo root)"
	}
	fmt.Fprintf(stdout, "Set upstream for %s: %s @ %s (path=%s)\n", slug, gotURL, gotRef, gotPath)
	return nil
}

// runSkillClearUpstream implements `arc-sync skill clear-upstream <slug>`.
func runSkillClearUpstream() {
	args := os.Args[3:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: arc-sync skill clear-upstream <slug>")
		os.Exit(1)
	}
	slug := args[0]
	mgr := newSkillManager()
	if err := clearUpstream(mgr.Client, slug, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}

// clearUpstream is the testable core of runSkillClearUpstream.
func clearUpstream(c upstreamClient, slug string, stdout, stderr io.Writer) error {
	if err := c.ClearUpstream(slug); err != nil {
		printSetUpstreamError(stderr, slug, err)
		return err
	}
	fmt.Fprintf(stdout, "Cleared upstream tracking for %s\n", slug)
	return nil
}

// printSetUpstreamError translates SkillHTTPError statuses into user-facing
// messages. Mirrors printCheckDriftError; consolidating the two would couple
// otherwise-independent commands so we keep them separate.
func printSetUpstreamError(stderr io.Writer, slug string, err error) {
	var httpErr *relay.SkillHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Status {
		case http.StatusBadRequest:
			fmt.Fprintf(stderr, "skill %s: bad request (check --git-url and --ref values)\n", slug)
			return
		case http.StatusForbidden:
			fmt.Fprintf(stderr, "skill %s: forbidden — admin role or skills:write API key required\n", slug)
			return
		case http.StatusNotFound:
			fmt.Fprintf(stderr, "skill %s: not found on relay\n", slug)
			return
		case http.StatusUnsupportedMediaType:
			fmt.Fprintf(stderr, "skill %s: relay rejected request body (likely a client/server mismatch)\n", slug)
			return
		}
	}
	fmt.Fprintf(stderr, "skill %s: %s\n", slug, err)
}

// driftClient narrows the relay.Client surface area used by the check-updates
// rendering helpers so tests can swap in a fake without spinning a real
// httptest server. The real *relay.Client trivially satisfies it.
type driftClient interface {
	CheckDrift(slug string) (*relay.DriftBlock, error)
	ListSkills() ([]*relay.Skill, error)
}

// runSkillCheckUpdates implements `arc-sync skill check-updates [<slug>]`.
// With a slug it prints one of:
//   - "up-to-date" (HTTP 204)
//   - "outdated · <severity>: <summary>" plus the recommended action (HTTP 200)
//   - a friendly error + exit 1 for 404 / 409 / 502
//
// Without a slug it iterates every skill the user can see and prints a
// per-skill line. 409 ("no upstream configured") is treated as a no-op and
// silently skipped — this is the expected case for hand-published skills.
func runSkillCheckUpdates() {
	args := os.Args[3:]
	var slug string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		slug = args[0]
	}
	mgr := newSkillManager()
	if err := checkUpdates(mgr.Client, slug, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}

// checkUpdates is the testable core of runSkillCheckUpdates. It takes a
// driftClient (interface so tests can inject a fake) plus explicit writers
// for stdout/stderr. Returns an error only when the operation should signal
// non-zero exit (single-skill failure or initial ListSkills failure); the
// per-skill iteration mode keeps going past errors and returns nil.
func checkUpdates(c driftClient, slug string, stdout, stderr io.Writer) error {
	if slug != "" {
		drift, err := c.CheckDrift(slug)
		if err != nil {
			printCheckDriftError(stderr, err)
			return err
		}
		if drift == nil {
			fmt.Fprintln(stdout, "up-to-date")
			return nil
		}
		fmt.Fprintf(stdout, "outdated · %s: %s\n", drift.Severity, drift.Summary)
		if drift.RecommendedAction != "" {
			fmt.Fprintln(stdout, drift.RecommendedAction)
		}
		return nil
	}

	// All-skills mode. Admin-scale iteration is fine — the relay caps at a
	// few dozen skills. We swallow 409 (no upstream) since it's not actionable
	// and surface other errors as warnings without aborting the loop.
	skills, err := c.ListSkills()
	if err != nil {
		fmt.Fprintln(stderr, "skill check-updates:", err)
		return err
	}
	for _, s := range skills {
		drift, err := c.CheckDrift(s.Slug)
		if err != nil {
			var httpErr *relay.SkillHTTPError
			if errors.As(err, &httpErr) && httpErr.Status == http.StatusConflict {
				// No upstream tracked for this skill — expected, skip.
				continue
			}
			fmt.Fprintf(stderr, "%s: %s\n", s.Slug, err)
			continue
		}
		if drift == nil {
			fmt.Fprintf(stdout, "%s: up-to-date\n", s.Slug)
			continue
		}
		fmt.Fprintf(stdout, "%s: outdated · %s\n", s.Slug, drift.Severity)
	}
	return nil
}

// printCheckDriftError translates SkillHTTPError statuses into the user-facing
// strings the plan calls out, falling back to the wrapped error for anything
// unexpected.
func printCheckDriftError(stderr io.Writer, err error) {
	var httpErr *relay.SkillHTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Status {
		case http.StatusNotFound:
			fmt.Fprintln(stderr, "skill not found")
			return
		case http.StatusConflict:
			fmt.Fprintln(stderr, "no upstream tracking configured")
			return
		case http.StatusBadGateway:
			fmt.Fprintln(stderr, "upstream fetch failed")
			return
		}
	}
	fmt.Fprintln(stderr, "skill check-updates:", err)
}

// emitJSON marshals v as pretty JSON and writes it to stdout. Used by the
// --json flag on each subcommand. Errors are fatal — the user asked for JSON,
// returning text would be a contract violation.
func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "encode JSON:", err)
		os.Exit(1)
	}
}
