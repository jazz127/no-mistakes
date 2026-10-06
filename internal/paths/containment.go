package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Canonical resolves a path to the form used to compare two spellings of the
// same directory. macOS reports /private/var for a /var path, so a single
// spelling is not enough to recognize the same checkout in a config key and a
// repository record. It is the one canonicalization every worktree_roots
// consumer uses, so a path that matches in one place matches in all of them.
func Canonical(path string) string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		cleaned = abs
	}
	// EvalSymlinks fails outright on a path that does not exist yet, and a
	// configured worktree root usually does not until its first run. Resolve
	// the deepest existing ancestor and keep the remainder, so a root and the
	// checkout it belongs to are still compared in the same spelling.
	current, rest := cleaned, ""
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Clean(filepath.Join(resolved, rest))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

// Contains reports whether path is dir itself or sits below it. It is how the
// pathological placements are recognized: a worktree root inside the checkout
// whose runs it would hold, or inside the directory no-mistakes already owns.
//
// Comparing spellings answers this for paths that do not exist yet, which a
// configured worktree root usually does not. It is not the whole answer,
// because on a case-insensitive volume - the macOS and Windows default -
// <NM_HOME>/logs and <nm_home>/Logs are ONE directory that no spelling
// comparison equates, and every placement guard would wave through a root that
// then collides with the daemon's own state. So a spelling that says "outside"
// is checked against the filesystem, which is the authority on whether two
// names are one directory.
func Contains(dir, path string) bool {
	return containsBySpelling(dir, path) || containsByIdentity(dir, path)
}

func containsBySpelling(dir, path string) bool {
	rel, err := filepath.Rel(Canonical(dir), Canonical(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// containsByIdentity asks the filesystem whether any ancestor of path IS dir,
// which settles case-insensitive volumes and any other aliasing a name cannot
// express. It walks upward because the interesting paths do not exist yet: the
// leaf is a root that no run has created, and the component whose case differs
// is one of its ancestors.
//
// A dir that does not exist has nothing to be identical to, so the spelling
// answer stands alone for it.
func containsByIdentity(dir, path string) bool {
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return false
	}
	current := Canonical(path)
	for {
		if info, err := os.Stat(current); err == nil && os.SameFile(dirInfo, info) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}
