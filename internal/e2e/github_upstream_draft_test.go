//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestGitHubUpstreamDraftMode drives a real daemon run through the fork-routed
// PR step and reads the gh invocations it made. `upstream` must add --draft
// only when the PR's base repository belongs to a different owner than the
// configured fork, and the legacy boolean keeps drafting everything.
func TestGitHubUpstreamDraftMode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      string
		parentURL string
		parent    string
		forkURL   string
		wantDraft bool
	}{
		{
			name:      "upstream_mode_foreign_owner_drafts",
			mode:      "upstream",
			parentURL: "https://github.com/parent-owner/no-mistakes.git",
			parent:    "parent-owner/no-mistakes",
			forkURL:   "https://github.com/fork-owner/no-mistakes.git",
			wantDraft: true,
		},
		{
			name:      "upstream_mode_same_owner_stays_ready",
			mode:      "upstream",
			parentURL: "https://github.com/house-owner/no-mistakes.git",
			parent:    "house-owner/no-mistakes",
			forkURL:   "https://github.com/HOUSE-OWNER/no-mistakes-fork.git",
			wantDraft: false,
		},
		{
			name:      "upstream_mode_without_fork_stays_ready",
			mode:      "upstream",
			parentURL: "https://github.com/parent-owner/no-mistakes.git",
			parent:    "parent-owner/no-mistakes",
			wantDraft: false,
		},
		{
			name:      "legacy_true_drafts_same_owner",
			mode:      "true",
			parentURL: "https://github.com/house-owner/no-mistakes.git",
			parent:    "house-owner/no-mistakes",
			forkURL:   "https://github.com/house-owner/no-mistakes-fork.git",
			wantDraft: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{
				Agent:             "claude",
				GlobalConfigExtra: "providers:\n  github:\n    draft_pull_requests: " + tc.mode,
			})
			ctx := context.Background()
			branch := "feature/draft-" + strings.ReplaceAll(tc.name, "_", "-")

			configureGitURLRewrite(t, h, tc.parentURL, h.UpstreamDir)
			if out, err := h.runGit(ctx, h.WorkDir, "remote", "set-url", "origin", tc.parentURL); err != nil {
				t.Fatalf("set parent origin: %v\n%s", err, out)
			}
			initArgs := []string{"init"}
			if tc.forkURL != "" {
				forkDir := filepath.Join(filepath.Dir(h.UpstreamDir), "fork.git")
				if err := os.MkdirAll(forkDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if out, err := h.runGit(ctx, forkDir, "init", "--bare", "--initial-branch=main"); err != nil {
					t.Fatalf("init fork: %v\n%s", err, out)
				}
				if out, err := h.runGit(ctx, h.WorkDir, "push", forkDir, "main"); err != nil {
					t.Fatalf("seed fork: %v\n%s", err, out)
				}
				configureGitURLRewrite(t, h, tc.forkURL, forkDir)
				initArgs = append(initArgs, "--fork-url", tc.forkURL)
			}

			ghLog := filepath.Join(filepath.Dir(h.AgentLog), "gh-draft.log")
			t.Setenv("FAKEAGENT_GH_MODE", "fork-pr")
			t.Setenv("FAKEAGENT_GH_LOG", ghLog)
			t.Setenv("FAKEAGENT_GH_PARENT", tc.parent)

			if out, err := h.Run(initArgs...); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			h.CommitChange(branch, "draft.txt", "draft mode\n", "add draft mode change")
			h.PushToGate(branch)

			run := h.WaitForRun(branch, 90*time.Second)
			if run.Status != types.RunCompleted {
				t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
			}
			var creates [][]string
			for _, inv := range readGHStubInvocations(t, ghLog) {
				if len(inv.Args) >= 2 && inv.Args[0] == "pr" && inv.Args[1] == "create" {
					creates = append(creates, inv.Args)
				}
			}
			if len(creates) != 1 {
				t.Fatalf("gh pr create calls = %d, want 1: %v", len(creates), creates)
			}
			gotDraft := false
			for _, a := range creates[0] {
				if a == "--draft" {
					gotDraft = true
				}
			}
			t.Logf("PR URL: %s", deref(run.PRURL))
			t.Logf("gh pr create argv: %q", creates[0])
			if gotDraft != tc.wantDraft {
				t.Fatalf("--draft present = %v, want %v", gotDraft, tc.wantDraft)
			}
		})
	}
}

// TestGitHubDraftModeRejectsUnknownValue proves a typo in the mode fails
// closed at config load instead of silently resolving to non-draft.
func TestGitHubDraftModeRejectsUnknownValue(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	cfg := filepath.Join(h.NMHome, "config.yaml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(data, []byte("providers:\n  github:\n    draft_pull_requests: sometimes\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := h.Run("init")
	t.Logf("init output:\n%s", out)
	if err == nil {
		t.Fatalf("init succeeded with an unknown draft mode")
	}
	if !strings.Contains(out, `draft_pull_requests must be a boolean or "upstream"`) {
		t.Fatalf("init error does not name the draft setting:\n%s", out)
	}
}

// TestGitHubDraftBooleanRepoConfigSurvivesEvalCapture drives the provenance
// round trip: the daemon marshals the effective repo config into the round's
// replay record, and eval capture must be able to re-parse it.
func TestGitHubDraftBooleanRepoConfigSurvivesEvalCapture(t *testing.T) {
	for _, mode := range []string{"true", "false", "upstream"} {
		t.Run(mode, func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude"})
			commitTrustedRepoConfig(t, h, "providers:\n  github:\n    draft_pull_requests: "+mode+"\n")
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			branch := "draft-capture-" + mode
			h.CommitChange(branch, "capture.go", "package e2e\n\nfunc Capture() {}\n", "add capture change")
			h.PushToGate(branch)
			run := h.WaitForRun(branch, 90*time.Second)
			out, err := h.Run("eval", "capture", run.ID)
			t.Logf("run status=%s; eval capture output:\n%s", run.Status, out)
			if err != nil {
				t.Fatalf("eval capture: %v\n%s", err, out)
			}
			if strings.Contains(out, "draft_pull_requests") {
				t.Fatalf("eval capture complained about draft setting:\n%s", out)
			}
		})
	}
}
