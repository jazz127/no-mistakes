package steps

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// stagePipelineChanges guards every pipeline-owned catch-all staging path,
// including Push's leftover commit. Refusal preserves the index and worktree.
// New tool caches and scratch files stay in the run worktree but out of commits.
func stagePipelineChanges(sctx *pipeline.StepContext) error {
	// Disable renames so both source and destination are checked, and list
	// individual untracked files so protected paths and caches inside a new
	// directory cannot hide behind the directory entry. NULs preserve unusual
	// names.
	status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("check protected_paths and scratch: %w", err)
	}
	var changed, unstage, deleted, excludedFiles []string
	scratch := newScratchExclusions()
	for _, entry := range strings.Split(strings.TrimSuffix(status, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return fmt.Errorf("check protected_paths and scratch: invalid git status entry %q", entry)
		}
		file := entry[3:]
		for _, pattern := range sctx.Config.ProtectedPaths {
			if matchIgnorePattern(file, pattern) {
				return &pipeline.ProtectedPathError{Path: file, Rule: pattern}
			}
		}
		if (entry[:2] == "??" || entry[0] == 'A' || entry[1] == 'A') && scratch.add(file) {
			if entry[:2] != "??" {
				unstage = append(unstage, file)
			}
			excludedFiles = append(excludedFiles, file)
			continue
		}
		changed = append(changed, file)
		if entry[0] == 'D' || (entry[1] == 'D' && entry[0] != 'A') {
			deleted = append(deleted, file)
		}
	}
	// A tracked deletion whose committed content now lives at an excluded new
	// scratch path is a move into scratch; defer it so the move is staged
	// whole or not at all. Every other deletion is staged normally.
	deleted, err = movedIntoScratch(sctx, deleted, excludedFiles)
	if err != nil {
		return fmt.Errorf("check moves into scratch: %w", err)
	}
	if len(deleted) > 0 {
		if _, err := stepGitRunInput(sctx, nulPathspecs(literalPathspecs(deleted)), "reset", "-q", "HEAD", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return fmt.Errorf("unstage scratch move sources: %w", err)
		}
	}
	if len(unstage) > 0 {
		if _, err := stepGitRunInput(sctx, nulPathspecs(literalPathspecs(unstage)), "rm", "--cached", "-f", "-q", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	specs := append([]string{"."}, scratch.pathspecs(changed)...)
	for _, file := range deleted {
		specs = append(specs, ":(exclude,literal)"+file)
	}
	if _, err := stepGitRunInput(sctx, nulPathspecs(specs), "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return err
	}
	if summary := scratch.summary(); summary != "" {
		sctx.Log("left new tool caches and scratch out of the commit (still in the run worktree): " + summary)
	}
	if len(deleted) > 0 {
		sctx.Log("left tracked deletions out of the commit (moved into excluded scratch; still in the run worktree): " + strings.Join(deleted, ", "))
	}
	return unstageSubmodulePointerMoves(sctx)
}

// movedIntoScratch returns the deleted paths whose HEAD blob matches the
// content of an excluded new scratch or cache file. Only excluded regular
// files and symlinks are hashed, regular files with the same clean and EOL
// conversion as the HEAD blob; symlinks are hashed by target, never followed.
// An empty HEAD blob holds no content to lose and is never a move.
func movedIntoScratch(sctx *pipeline.StepContext, deleted, excluded []string) ([]string, error) {
	if len(deleted) == 0 || len(excluded) == 0 {
		return nil, nil
	}
	tree, err := stepGitRunRaw(sctx, append([]string{"--literal-pathspecs", "ls-tree", "-z", "-l", "--full-tree", "HEAD", "--"}, deleted...)...)
	if err != nil {
		return nil, err
	}
	headBlobs := map[string]string{}
	hasRegular := false
	linkSizes := map[int64]bool{}
	for _, entry := range strings.Split(strings.TrimSuffix(tree, "\x00"), "\x00") {
		meta, file, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size == 0 {
			continue
		}
		if fields[0] == "120000" {
			linkSizes[size] = true
		} else {
			hasRegular = true
		}
		headBlobs[file] = fields[2]
	}
	if len(headBlobs) == 0 {
		return nil, nil
	}
	excludedBlobs := map[string]bool{}
	var regular []string
	for _, file := range excluded {
		info, err := os.Lstat(filepath.Join(sctx.WorkDir, filepath.FromSlash(file)))
		if err != nil {
			continue
		}
		switch {
		case info.Mode().IsRegular():
			if hasRegular {
				regular = append(regular, file)
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(filepath.Join(sctx.WorkDir, filepath.FromSlash(file)))
			if err != nil || !linkSizes[int64(len(target))] {
				continue
			}
			blob, err := stepGitRunInput(sctx, strings.NewReader(target), "hash-object", "--stdin")
			if err != nil {
				return nil, err
			}
			excludedBlobs[strings.TrimSpace(blob)] = true
		}
	}
	const hashBatch = 256
	for start := 0; start < len(regular); start += hashBatch {
		out, err := stepGitRun(sctx, append([]string{"hash-object", "--"}, regular[start:min(start+hashBatch, len(regular))]...)...)
		if err != nil {
			return nil, err
		}
		for _, blob := range strings.Fields(out) {
			excludedBlobs[blob] = true
		}
	}
	var moved []string
	for _, file := range deleted {
		if blob, ok := headBlobs[file]; ok && excludedBlobs[blob] {
			moved = append(moved, file)
		}
	}
	return moved, nil
}

func literalPathspecs(files []string) []string {
	specs := make([]string, len(files))
	for i, file := range files {
		specs[i] = ":(literal)" + file
	}
	return specs
}

func nulPathspecs(specs []string) io.Reader {
	return strings.NewReader(strings.Join(specs, "\x00") + "\x00")
}

// scratchCacheDirs are directory names that only ever hold tool caches or
// installed dependencies. A new file under one is never an intended change.
var scratchCacheDirs = map[string]bool{
	"node_modules":  true,
	".cache":        true,
	".corepack":     true,
	".npm":          true,
	".pnpm-store":   true,
	"__pycache__":   true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".ruff_cache":   true,
}

// scratchRoot reports whether a new path is a tool cache or an ad-hoc scratch
// file, and the path to exclude for it. Tracked files are never classified.
func scratchRoot(file string) (root, reason string, ok bool) {
	isDir := strings.HasSuffix(file, "/")
	parts := strings.Split(strings.TrimSuffix(file, "/"), "/")
	dirs := parts
	if !isDir {
		dirs = parts[:len(parts)-1]
	}
	inCache := false
	for i, part := range dirs {
		if i == 0 && part == "scratch" {
			return part, "scratch directory", true
		}
		if scratchCacheDirs[part] || (part == "corepack" && inCache) {
			return strings.Join(parts[:i+1], "/"), "tool cache", true
		}
		inCache = inCache || part == "cache" || strings.HasPrefix(part, ".")
	}
	base := parts[len(parts)-1]
	if !isDir && len(parts) == 2 && (parts[0] == "tests" || parts[0] == "test") && strings.HasPrefix(base, "_") {
		switch path.Ext(base) {
		case ".sh", ".bash", ".zsh":
			return file, "scratch script", true
		}
	}
	return "", "", false
}

type scratchExclusion struct {
	root   string
	reason string
	files  []string
}

type scratchExclusions struct {
	byRoot map[string]*scratchExclusion
	order  []string
}

func newScratchExclusions() *scratchExclusions {
	return &scratchExclusions{byRoot: map[string]*scratchExclusion{}}
}

func (s *scratchExclusions) add(file string) bool {
	root, reason, ok := scratchRoot(file)
	if !ok {
		return false
	}
	ex := s.byRoot[root]
	if ex == nil {
		ex = &scratchExclusion{root: root, reason: reason}
		s.byRoot[root] = ex
		s.order = append(s.order, root)
	}
	ex.files = append(ex.files, file)
	return true
}

// pathspecs excludes each cache directory whole, keeping the pathspec list
// short for large caches, unless a tracked change lies under it; then only its
// untracked files are excluded so the tracked change is still staged.
func (s *scratchExclusions) pathspecs(changed []string) []string {
	var specs []string
	for _, root := range s.order {
		ex := s.byRoot[root]
		excluded := []string{root}
		for _, file := range changed {
			if strings.HasPrefix(file, root+"/") {
				excluded = ex.files
				break
			}
		}
		for _, file := range excluded {
			specs = append(specs, ":(exclude,literal)"+file)
		}
	}
	return specs
}

func (s *scratchExclusions) summary() string {
	const maxNamed = 10
	var named []string
	for _, root := range s.order[:min(len(s.order), maxNamed)] {
		ex := s.byRoot[root]
		if len(ex.files) == 1 && ex.files[0] == root {
			named = append(named, fmt.Sprintf("%s (%s)", root, ex.reason))
		} else {
			named = append(named, fmt.Sprintf("%s/ (%s, %d files)", root, ex.reason, len(ex.files)))
		}
	}
	if len(s.order) > maxNamed {
		named = append(named, fmt.Sprintf("and %d more", len(s.order)-maxNamed))
	}
	return strings.Join(named, ", ")
}

// unstageSubmodulePointerMoves keeps catch-all staging from recording a
// submodule checkout as a new pointer. A rebase moves the recorded pointer but
// not the populated checkout, so `git add -A` would commit the stale checkout
// as a silent revert of the base's submodule bump, and a populated checkout
// whose gitlink the base removed would be re-added as an embedded repository;
// pipeline corrections never move a submodule pointer. A gitlink addition the
// staged .gitmodules registers is a deliberately added submodule and stays.
func unstageSubmodulePointerMoves(sctx *pipeline.StepContext) error {
	raw, err := stepGitRunRaw(sctx, "diff", "--cached", "--raw", "-z", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("check staged submodule pointers: %w", err)
	}
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	var moved []string
	var registered map[string]bool
	for i := 0; i+1 < len(fields); i += 2 {
		modes := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(modes) < 2 || modes[1] != "160000" {
			continue
		}
		if modes[0] == "160000" {
			moved = append(moved, fields[i+1])
			continue
		}
		if modes[0] != "000000" {
			continue
		}
		if registered == nil {
			if registered, err = stagedRegisteredSubmodules(sctx); err != nil {
				return fmt.Errorf("check staged submodule pointers: %w", err)
			}
		}
		if !registered[fields[i+1]] {
			moved = append(moved, fields[i+1])
		}
	}
	if len(moved) == 0 {
		return nil
	}
	if _, err := stepGitRun(sctx, append([]string{"--literal-pathspecs", "reset", "-q", "--"}, moved...)...); err != nil {
		return fmt.Errorf("unstage submodule pointers: %w", err)
	}
	sctx.Log(fmt.Sprintf("left submodule pointers as recorded: %s", strings.Join(moved, ", ")))
	return nil
}

// stagedRegisteredSubmodules returns the submodule paths the staged .gitmodules
// registers.
func stagedRegisteredSubmodules(sctx *pipeline.StepContext) (map[string]bool, error) {
	registered := map[string]bool{}
	if _, err := stepGitRun(sctx, "cat-file", "-e", ":.gitmodules"); err != nil {
		return registered, nil
	}
	out, err := stepGitRunRaw(sctx, "config", "--null", "--blob", ":.gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 1") {
			return registered, nil
		}
		return nil, fmt.Errorf("list staged submodules: %w", err)
	}
	paths, err := parseRegisteredSubmodulePaths([]byte(out))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		registered[filepath.ToSlash(path)] = true
	}
	return registered, nil
}
