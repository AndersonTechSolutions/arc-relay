package sync

import "testing"

// safeRelPath must reject traversal regardless of which separator the
// platform's filepath.Clean produced. The backslash forms are what Windows
// yields; before slash-normalization they slipped past the "../" checks and
// made TestExtractTarGz_RejectsTraversal fail on the Windows runner alone.
// Testing the backslash inputs directly means any platform's CI catches a
// regression, not just Windows.
//
// Lives in an internal test file because safeRelPath is unexported and
// skills_test.go is an external (package sync_test) file.
func TestSafeRelPath(t *testing.T) {
	rejected := []string{
		"", ".", "..",
		"../escape.txt", `..\escape.txt`,
		"a/../../escape.txt", `a\..\..\escape.txt`,
		"/etc/passwd",
	}
	for _, p := range rejected {
		if safeRelPath(p) {
			t.Errorf("safeRelPath(%q) = true, want false (escapes destination)", p)
		}
	}

	allowed := []string{
		"SKILL.md", "dir/file.txt",
		"a/b/c.txt", "..dots.txt", "a/..b/c.txt",
	}
	for _, p := range allowed {
		if !safeRelPath(p) {
			t.Errorf("safeRelPath(%q) = false, want true (legitimate path)", p)
		}
	}
}
