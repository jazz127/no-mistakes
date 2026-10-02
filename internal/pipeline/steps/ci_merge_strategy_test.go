package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// advanceBaseOverFeature moves origin/main over the same file the fixture's
// feature commit adds, so integrating the base genuinely conflicts, and
// returns the new base tip. The worktree is left on the feature branch.
func advanceBaseOverFeature(t *testing.T, f *ciRepairFixture) string {
	t.Helper()
	gitCmd(t, f.dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(f.dir, "feature.txt"), []byte("base rewrote this line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "advance base over the same line")
	advancedBase := gitCmd(t, f.dir, "rev-parse", "HEAD")
	gitCmd(t, f.dir, "push", "origin", "main")
	gitCmd(t, f.dir, "checkout", "feature")
	return advancedBase
}

// TestCIStep_MergeStrategyConflictRepairMergesTheBaseAndFastForwards is the
// behaviour rebase.strategy: merge exists for on a merge-commits-only base: a
// CI merge-conflict repair merges the moved base into the already-published
// head instead of rebasing it, so the repaired head descends from the pushed
// commits, publishes as a plain fast-forward through the guarded push path, and
// never needs revalidation or a rewrite the push guards would refuse.
func TestCIStep_MergeStrategyConflictRepairMergesTheBaseAndFastForwards(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		failingChecks []string
		// wantFixerPrompts is how many CI fixer turns run after the merge.
		wantFixerPrompts int
	}{
		{name: "conflict_only", wantFixerPrompts: 0},
		{name: "conflict_and_failing_check", failingChecks: []string{"test"}, wantFixerPrompts: 1},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var prompts []string
			f := newCIRepairFixture(t, false, nil)
			f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
			f.sctx.Repo.DefaultBranch = "main"
			f.sctx.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				prompts = append(prompts, opts.Prompt)
				if strings.Contains(opts.Prompt, "Resolve git merge conflicts") {
					// Resolve additively and conclude the merge, as the
					// merge resolver prompt instructs.
					if err := os.WriteFile(filepath.Join(opts.CWD, "feature.txt"), []byte("base rewrote this line\nfeature\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, opts.CWD, "add", "-A")
					gitCmd(t, opts.CWD, "commit", "--no-edit")
					return &agent.Result{Output: []byte(`{"summary":"kept both sides"}`)}, nil
				}
				writeCIFix(opts.CWD)
				return &agent.Result{Output: []byte(`{"summary":"repair the failing check","code_change_needed":true}`)}, nil
			}}
			advancedBase := advanceBaseOverFeature(t, f)
			pushedHead := f.remoteHead(t)
			if pushedHead != f.headSHA {
				t.Fatalf("fixture remote head = %s, want the pushed reviewed head %s", pushedHead, f.headSHA)
			}

			host, skip := buildHost(f.sctx, scm.ProviderGitHub)
			if host == nil {
				t.Fatalf("buildHost returned nil: %s", skip)
			}
			pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42", BaseBranch: "main"}
			repair, err := (&CIStep{}).autoFixCI(f.sctx, host, pr, ciTargetsFor(tc.failingChecks, true))
			if err != nil {
				t.Fatalf("merge-strategy conflict repair failed: %v\nlog:\n%s", err, f.log())
			}
			if !repair.HeadAdvanced {
				t.Fatalf("the conflict repair was not recorded as a real change\nlog:\n%s", f.log())
			}
			if repair.Revalidate {
				t.Fatalf("a merged conflict repair continues the reviewed head and must publish, not revalidate\nlog:\n%s", f.log())
			}

			if len(prompts) != 1+tc.wantFixerPrompts {
				t.Fatalf("agent turns = %d, want the merge resolver plus %d fixer turn(s)", len(prompts), tc.wantFixerPrompts)
			}
			if !strings.Contains(prompts[0], "Resolve git merge conflicts") {
				t.Fatalf("first agent turn is not the merge resolver:\n%s", prompts[0])
			}
			for _, p := range prompts {
				if strings.Contains(p, "Rebase onto the base branch") || strings.Contains(p, "rebase target commit") {
					t.Fatalf("a merge-strategy repair still asked the agent to rebase:\n%s", p)
				}
			}

			// The merge commit keeps the pushed head as its first parent and
			// the moved base as its second, whatever the fixer added on top.
			localHead := f.localHead(t)
			merge := localHead
			if tc.wantFixerPrompts > 0 {
				merge = gitCmd(t, f.dir, "rev-parse", "HEAD~1")
			}
			parents := strings.Fields(gitCmd(t, f.dir, "rev-list", "--parents", "-n", "1", merge))
			if len(parents) != 3 || parents[1] != pushedHead || parents[2] != advancedBase {
				t.Fatalf("merge commit parents = %v, want [%s %s]", parents[1:], pushedHead, advancedBase)
			}

			// Published, and published as a fast-forward of what was already
			// on the remote: the pushed head is an ancestor of the new remote
			// head, which is the repaired head.
			remoteHead := f.remoteHead(t)
			if remoteHead != localHead {
				t.Fatalf("remote head = %s, want the repaired head %s\nlog:\n%s", remoteHead, localHead, f.log())
			}
			gitCmd(t, f.upstream, "merge-base", "--is-ancestor", pushedHead, remoteHead)
			if !strings.Contains(f.log(), "committed and pushed CI repair") {
				t.Fatalf("the repair was not published through the guarded push path:\n%s", f.log())
			}
			if strings.Contains(f.log(), "cannot prove the repaired head continues the reviewed head") {
				t.Fatalf("the merged repair was treated as a rewrite:\n%s", f.log())
			}

			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != f.headSHA {
				t.Errorf("review approval = %v, want it kept on the reviewed head %s", run.ReviewApprovedHeadSHA, f.headSHA)
			}
			if run.HeadSHA != localHead {
				t.Errorf("recorded head = %s, want the repaired head %s", run.HeadSHA, localHead)
			}
		})
	}
}

// Under the default strategy the conflict repair is unchanged: the fixer is
// asked to rebase, and the pipeline never merges on its behalf.
func TestCIStep_DefaultStrategyConflictRepairStillRebases(t *testing.T) {
	t.Parallel()
	var prompts []string
	f := newCIRepairFixture(t, true, nil)
	f.sctx.Repo.DefaultBranch = "main"
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		return &agent.Result{Output: []byte(`{"summary":"nothing","code_change_needed":true}`)}, nil
	}}
	advanceBaseOverFeature(t, f)

	host, skip := buildHost(f.sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatalf("buildHost returned nil: %s", skip)
	}
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42", BaseBranch: "main"}
	if _, err := (&CIStep{}).autoFixCI(f.sctx, host, pr, ciTargetsFor(nil, true)); err != nil {
		t.Fatalf("auto-fix CI: %v", err)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "Rebase onto the base branch and resolve the merge conflicts") {
		t.Fatalf("default strategy must hand the conflict to the fixer as a rebase, got %d prompt(s):\n%s", len(prompts), strings.Join(prompts, "\n---\n"))
	}
	if f.localHead(t) != f.headSHA {
		t.Fatalf("the pipeline integrated the base itself under the default strategy: head %s", f.localHead(t))
	}
}
