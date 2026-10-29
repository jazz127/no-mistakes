package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_TimeoutNormalizesRetainedCommitsBeforeRecordingCustody(t *testing.T) {
	for _, state := range []string{"linear", "base-merge", "invalid-template"} {
		t.Run(state, func(t *testing.T) {
			f := newCIRepairFixture(t, false, nil)
			f.sctx.Config.Commit.FixMessage = "retained({{.Step}}): {{.Summary}}"
			baseBranch := "main"
			parent := f.headSHA
			if state == "base-merge" {
				f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
				baseBranch = "release"
				gitCmd(t, f.dir, "checkout", "-b", baseBranch, "main")
				writeFile(t, f.dir, "base.txt", "base advanced\n")
				gitCmd(t, f.dir, "add", "-A")
				gitCmd(t, f.dir, "commit", "-m", "advance the live PR base")
				gitCmd(t, f.dir, "push", "origin", baseBranch)
				gitCmd(t, f.dir, "checkout", "feature")
				gitCmd(t, f.dir, "merge", "--no-ff", "-m", "integrate the PR base", baseBranch)
				parent = f.localHead(t)
			}
			agentCommit(t, f.dir, "repair.txt", "free-form timed-out repair")
			rawHead := f.localHead(t)
			treeAndAuthor := gitCmd(t, f.dir, "show", "-s", "--date=raw", "--format=%T %an %ae %ad", "HEAD")
			writeFile(t, f.dir, "leftover.txt", "staged leftover\n")
			gitCmd(t, f.dir, "add", "leftover.txt")
			writeFile(t, f.dir, "leftover.txt", "unstaged leftover\n")
			staged := gitCmd(t, f.dir, "diff", "--cached", "--binary")
			unstaged := gitCmd(t, f.dir, "diff", "--binary")
			if state == "invalid-template" {
				f.sctx.Config.Commit.FixMessage = "{{.NoSuchField}}"
			}
			outcome := (&CIStep{}).ciFixAgentBudgetOutcome(f.sctx, &scm.PR{BaseBranch: baseBranch}, "test", pipeline.ErrAgentTimeout)
			if outcome == nil || !outcome.NeedsApproval || outcome.RestartFrom != "" {
				t.Fatalf("timeout must park without restarting: %+v", outcome)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil || !types.HasAskUserFindings(findings) {
				t.Fatalf("timeout must retain its operator decision: %s, %v", outcome.Findings, err)
			}
			run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
			if err != nil || run == nil {
				t.Fatalf("load custody record: %v", err)
			}
			if state == "invalid-template" {
				if f.localHead(t) != rawHead || run.HeadSHA != f.headSHA || f.sctx.Run.HeadSHA != f.headSHA {
					t.Fatal("failed normalization changed the commit or advanced its recording anchor")
				}
			} else {
				if f.localHead(t) == rawHead || fullMessage(t, f.dir, "HEAD") != "retained(ci): repair failing checks" {
					t.Fatal("retained commit bypassed its configured message or trailer policy")
				}
				if run.HeadSHA != f.localHead(t) || f.sctx.Run.HeadSHA != run.HeadSHA || run.ReviewApprovedHeadSHA != nil {
					t.Fatal("custody did not record the normalized head with approval revoked")
				}
			}
			if gitCmd(t, f.dir, "show", "-s", "--date=raw", "--format=%T %an %ae %ad", "HEAD") != treeAndAuthor || gitCmd(t, f.dir, "rev-parse", "HEAD~1") != parent {
				t.Fatal("normalization changed the committed tree, author, or parent")
			}
			if gitCmd(t, f.dir, "diff", "--cached", "--binary") != staged || gitCmd(t, f.dir, "diff", "--binary") != unstaged {
				t.Fatal("timeout recording changed staged or unstaged leftovers")
			}
			if f.remoteHead(t) != f.headSHA || run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA {
				t.Fatal("timeout recording published a repair or changed the push binding")
			}
		})
	}
}
