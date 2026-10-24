//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These canned product claims keep the scratch-staging journey on its success
// path; the fakeagent scenario is synthetic/offline, not product evidence.
const scratchExclusionScenario = `actions:
  - match: "Derive the scenarios this change must satisfy, then run each one against the real running product.\n\nContext:\n- branch: scratch-exclusion"
    text: "tests passed; left a scratch runner behind"
    edits:
      - path: "tests/_all.sh"
        new: "#!/bin/sh\n# scratch runner\n"
    stage:
      - "tests/_all.sh"
    structured:
      findings: []
      summary: "all tests passed"
      tested: ["fakeagent: simulated test run"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          surface: product
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
  - match: "report only what you could not resolve.\n\nContext:\n- branch: scratch-exclusion"
    text: "updated docs; a corepack cache landed in the repo"
    edits:
      - path: "tests/_all.sh"
        new: "#!/bin/sh\n# scratch runner\n# rewritten after staging\n"
      - path: "docs/usage.md"
        new: "usage\n"
      - path: "scripts/_common.sh"
        new: "helper\n"
      - path: "packages/corepack/index.js"
        new: "source\n"
      - path: ".codex-live-check/cache/node/corepack/v1/pnpm/11.1.1/LICENSE"
        new: "cached\n"
      - path: ".codex-live-check/cache/node/corepack/v1/pnpm/11.1.1/README.md"
        new: "cached\n"
      - path: ".codex-live-check/cache/node/corepack/v1/pnpm/11.1.1/package.json"
        new: "{}\n"
    structured:
      findings: []
      summary: "Updated docs"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no remaining risk"
      risk_scope: source-or-external
      tested: ["fakeagent: simulated test run"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          surface: product
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: add feature"
      body: "scratch exclusion journey"
`

// TestScratchExclusionJourney drives a whole run whose Test agent stages a
// tests/_all.sh scratch runner (then rewrites it, leaving it AM) and whose
// Document agent writes a Corepack cache bundle next to real edits, and
// asserts the pushed branch carries the real edits and none of the scratch.
func TestScratchExclusionJourney(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "scratch-exclusion.yaml")
	if err := os.WriteFile(scenario, []byte(scratchExclusionScenario), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "scratch-exclusion"
	h.CommitChange(branch, "feature.txt", "feature\n", "add feature")
	h.PushToGate(branch)
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed (error=%v)", run.Status, deref(run.Error))
	}
	assertPushedHead(t, run.HeadSHA, h.UpstreamBranchSHA(branch))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := h.runGit(ctx, h.UpstreamDir, "ls-tree", "-r", "--name-only", "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("list pushed tree: %v\n%s", err, out)
	}
	tree := strings.Fields(string(out))
	for _, want := range []string{"feature.txt", "docs/usage.md", "scripts/_common.sh", "packages/corepack/index.js"} {
		if !slices.Contains(tree, want) {
			t.Errorf("pushed tree is missing real file %s: %v", want, tree)
		}
	}
	for _, file := range tree {
		if strings.HasPrefix(file, ".codex-live-check/") || file == "tests/_all.sh" {
			t.Errorf("pushed tree carries scratch %s: %v", file, tree)
		}
	}
	subjects, err := h.runGit(ctx, h.UpstreamDir, "log", "--format=%s", "main..refs/heads/"+branch)
	if err != nil {
		t.Fatalf("read pushed commit subjects: %v\n%s", err, subjects)
	}
	// The Test agent's staged runner is still in the index when the Document
	// agent rewrites it, so Document's commit must unstage an AM entry.
	docLog, err := os.ReadFile(filepath.Join(h.NMHome, "logs", run.ID, "document.log"))
	if err != nil {
		t.Fatalf("read document log: %v", err)
	}
	if !strings.Contains(string(docLog), "tests/_all.sh (scratch script)") || !strings.Contains(string(docLog), ".codex-live-check/cache/node/corepack/ (tool cache, 3 files)") {
		t.Fatalf("document step did not leave the scratch out of its commit:\n%s", docLog)
	}
	t.Logf("pushed tree: %v", tree)
	t.Logf("pushed commits:\n%s", strings.TrimSpace(string(subjects)))
}
