package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// corepackBundle is the shape of the Corepack pnpm cache a Document agent once
// pointed into the repository, which the step then committed.
var corepackBundle = []string{
	".codex-live-check/cache/node/corepack/v1/pnpm/11.1.1/LICENSE",
	".codex-live-check/cache/node/corepack/v1/pnpm/11.1.1/README.md",
	".codex-live-check/cache/node/corepack/v1/pnpm/11.1.1/package.json",
	".codex-live-check/cache/node/corepack/v1/pnpm/11.1.1/dist/pnpm.cjs",
}

func writeRepoFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func headCommitFiles(t *testing.T, dir string) []string {
	t.Helper()
	return strings.Fields(gitCmd(t, dir, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD"))
}

func captureStepLog(sctx *pipeline.StepContext) func() string {
	var mu sync.Mutex
	var lines []string
	sctx.Log = func(s string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, s)
	}
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

func TestDocumentStep_CommitLeavesToolCacheAndScratchOut(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		files := map[string]string{
			"README.md":           "# Updated\n",
			"docs/usage.md":       "usage\n",
			"tests/cli_test.sh":   "echo real test\n",
			"tests/_all.sh":       "scratch runner\n",
			"node_modules/x.js":   "dependency\n",
			"pkg/__pycache__/m":   "bytecode\n",
			"tests/__init__.py":   "",
			"internal/cache/c.go": "package cache\n",
		}
		for _, name := range corepackBundle {
			files[name] = "cached\n"
		}
		writeRepoFiles(t, dir, files)
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update docs"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	logs := captureStepLog(sctx)

	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	got := headCommitFiles(t, dir)
	want := []string{"README.md", "docs/usage.md", "internal/cache/c.go", "tests/__init__.py", "tests/cli_test.sh"}
	if !slices.Equal(got, want) {
		t.Fatalf("document commit = %v, want %v", got, want)
	}
	status := gitStatusPorcelain(t, dir)
	for _, left := range []string{".codex-live-check/", "tests/_all.sh", "node_modules/", "pkg/"} {
		if !strings.Contains(status, left) {
			t.Errorf("excluded %s should stay untracked in the worktree, status %q", left, status)
		}
	}
	log := logs()
	for _, named := range []string{
		".codex-live-check/cache/node/corepack/ (tool cache, 4 files)",
		"tests/_all.sh (scratch script)",
		"node_modules/ (tool cache, 1 files)",
		"pkg/__pycache__/ (tool cache, 1 files)",
	} {
		if !strings.Contains(log, named) {
			t.Errorf("step log does not name %q:\n%s", named, log)
		}
	}
}

func TestTestStep_FixCommitLeavesScratchRunnerOut(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	fixed := false
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if !fixed {
			fixed = true
			writeRepoFiles(t, dir, map[string]string{
				"fix.txt":              "fixed\n",
				"tests/regression.sh":  "echo regression\n",
				"tests/_all.sh":        strings.Repeat("echo scratch\n", 3874),
				".corepack/v1/pnpm.js": "cached\n",
			})
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"fix test failures","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"tests failed"}`
	logs := captureStepLog(sctx)

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}

	got := headCommitFiles(t, dir)
	want := []string{"fix.txt", "tests/regression.sh"}
	if !slices.Equal(got, want) {
		t.Fatalf("test fix commit = %v, want %v", got, want)
	}
	if log := logs(); !strings.Contains(log, "tests/_all.sh (scratch script)") || !strings.Contains(log, ".corepack/ (tool cache, 1 files)") {
		t.Fatalf("step log does not name the excluded scratch:\n%s", log)
	}
}

func TestStagePipelineChanges_TrackedChangeUnderCacheDirIsStillStaged(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	writeRepoFiles(t, dir, map[string]string{"vendor/node_modules/kept.js": "v1\n"})
	gitCmd(t, dir, "add", "vendor/node_modules/kept.js")
	gitCmd(t, dir, "commit", "-m", "vendor a dependency")
	writeRepoFiles(t, dir, map[string]string{
		"vendor/node_modules/kept.js": "v2\n",
		"vendor/node_modules/new.js":  "installed\n",
	})
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})

	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatal(err)
	}
	staged := strings.Fields(gitCmd(t, dir, "diff", "--cached", "--name-only"))
	if !slices.Equal(staged, []string{"vendor/node_modules/kept.js"}) {
		t.Fatalf("staged = %v, want only the tracked edit", staged)
	}
}

func TestStagePipelineChanges_ScratchUnderProtectedPathDoesNotRefuse(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	writeRepoFiles(t, dir, map[string]string{".cache/pnpm.lock": "cached\n", "doc.md": "doc\n"})
	sctx := newTestContext(t, &mockAgent{}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ProtectedPaths = []string{"*.lock"}

	if err := stagePipelineChanges(sctx); err != nil {
		t.Fatalf("a never-staged cache file refused staging: %v", err)
	}
	staged := strings.Fields(gitCmd(t, dir, "diff", "--cached", "--name-only"))
	if !slices.Equal(staged, []string{"doc.md"}) {
		t.Fatalf("staged = %v, want only doc.md", staged)
	}
}
