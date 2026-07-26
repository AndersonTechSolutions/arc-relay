// Package subhash computes deterministic SHA256 hashes over directory subtrees
// for skill upstream drift detection.
package subhash

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Hash returns a deterministic SHA256 hex digest of the contents of rootDir.
// The hash is stable across mtime, sort order, and filesystem boundaries.
// Excludes .git/, .DS_Store, and files matching .gitignore patterns at root.
func Hash(rootDir string) (string, error) {
	ignored, err := loadGitignorePatterns(rootDir)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	if err := walk(rootDir, "", h, ignored); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func walk(absRoot, relDir string, h io.Writer, ignored []string) error {
	absDir := filepath.Join(absRoot, relDir)
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, e := range entries {
		name := e.Name()
		if name == ".git" || name == ".DS_Store" {
			continue
		}
		relPath := filepath.ToSlash(filepath.Join(relDir, name))
		if matchAny(ignored, relPath) {
			continue
		}
		if e.IsDir() {
			if err := walk(absRoot, relPath, h, ignored); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		mode := uint32(0o100644)
		if info.Mode()&os.ModeSymlink != 0 {
			mode = 0o120000
			target, err := os.Readlink(filepath.Join(absRoot, relPath))
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(h, "%s\n%o\n%s\n", relPath, mode, target)
			continue
		}
		if info.Mode()&0o111 != 0 {
			mode = 0o100755
		}
		_, _ = fmt.Fprintf(h, "%s\n%o\n", relPath, mode)
		f, err := os.Open(filepath.Join(absRoot, relPath))
		if err != nil {
			return err
		}
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return err
		}
		_ = f.Close()
		_, _ = h.Write([]byte("\n"))
	}
	return nil
}

func loadGitignorePatterns(root string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func matchAny(patterns []string, relPath string) bool {
	base := filepath.Base(relPath)
	for _, p := range patterns {
		ok, _ := filepath.Match(p, relPath)
		if ok {
			return true
		}
		ok, _ = filepath.Match(p, base)
		if ok {
			return true
		}
	}
	return false
}
