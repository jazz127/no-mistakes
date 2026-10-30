package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIResumedRepairRetainedMergeDoesNotDependOnNextSelection(t *testing.T) {
	for _, revalidate := range []bool{false, true} {
		for _, next := range []string{"check-only", "integration-again", "protected-retry", "recorder"} {
			t.Run(fmt.Sprintf("%s/revalidate=%t", next, revalidate), func(t *testing.T) {
				f := newCIRepairFixture(t, revalidate, nil)
				f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
				base := advanceBaseOverFeature(t, f)
				f.sctx.PreviousFindings, _ = types.MarshalFindingsJSON(ciTargetsFor([]string{"test"}, true).Findings)
				fixCalls := 0
				f.sctx.Agent = &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if strings.Contains(opts.Prompt, "Resolve git merge conflicts") {
						writeFile(t, f.dir, "feature.txt", "base rewrote this line\nfeature\n")
						gitCmd(t, f.dir, "add", "-A")
						gitCmd(t, f.dir, "commit", "--no-edit")
						return &agent.Result{Output: []byte(`{"summary":"kept both sides"}`)}, nil
					}
					fixCalls++
					if fixCalls == 1 {
						agentCommit(t, f.dir, "retained.txt", "unshaped retained fixer commit")
						return nil, errors.New("fixer crashed after the merge and its own commit")
					}
					return &agent.Result{Output: []byte(`{"summary":"recover retained repair","code_change_needed":false}`)}, nil
				}}
				host, reason := buildHost(f.sctx, scm.ProviderGitHub)
				if host == nil {
					t.Fatal(reason)
				}
				pr := &scm.PR{Number: "42", URL: *f.sctx.Run.PRURL, BaseBranch: "main"}
				step := &CIStep{}
				if outcome, err := step.repairFromFindings(f.sctx, &retainedRepairLogHost{Host: host}, pr); err != nil || outcome != nil {
					t.Fatalf("failed repair = (%+v, %v)", outcome, err)
				}
				merge := gitCmd(t, f.dir, "rev-parse", "HEAD~1")
				tree := gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}")
				if f.remoteHead(t) != f.headSHA || f.sctx.Run.HeadSHA != f.headSHA {
					t.Fatal("failed repair was unexpectedly recorded or published")
				}
				if next != "integration-again" {
					gitCmd(t, f.dir, "checkout", "main")
					writeFile(t, f.dir, "feature.txt", "feature")
					gitCmd(t, f.dir, "add", "-A")
					gitCmd(t, f.dir, "commit", "-m", "base no longer conflicts with the published feature")
					gitCmd(t, f.dir, "push", "origin", "main")
					gitCmd(t, f.dir, "merge-tree", "--write-tree", f.headSHA, "main")
					gitCmd(t, f.dir, "checkout", "feature")
				}
				wantSummary := "recover retained repair"
				var restart types.StepName
				switch next {
				case "protected-retry", "recorder":
					wantSummary = "repair failing checks"
					var repair ciRepairResult
					var err error
					if next == "protected-retry" {
						repair, err = step.retryProtectedPathRepair(f.sctx, "main")
					} else {
						repair, err = step.commitRepair(f.sctx, "", nil)
					}
					if err != nil || !repair.HeadAdvanced {
						t.Fatalf("retained repair = (%+v, %v)", repair, err)
					}
					if repair.Revalidate {
						restart = repair.restartFrom()
					}
				default:
					f.sctx.PreviousFindings, _ = types.MarshalFindingsJSON(ciTargetsFor([]string{"test"}, next == "integration-again").Findings)
					outcome, err := step.repairFromFindings(f.sctx, &retainedRepairLogHost{Host: host}, pr)
					if err != nil {
						t.Fatal(err)
					}
					if outcome != nil {
						restart = outcome.RestartFrom
					}
				}
				if got := fullMessage(t, f.dir, "HEAD"); got != "no-mistakes(ci): "+wantSummary {
					t.Fatalf("retained fixer message = %q, want the configured subject without agent trailers", got)
				}
				if gitCmd(t, f.dir, "rev-parse", "HEAD~1") != merge || gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}") != tree {
					t.Fatal("normalization changed the retained merge or final tree")
				}
				if got := gitCmd(t, f.dir, "show", "-s", "--format=%P", merge); got != f.headSHA+" "+base {
					t.Fatalf("merge parents = %s", got)
				}
				if revalidate {
					if restart != types.StepReview || f.remoteHead(t) != f.headSHA {
						t.Fatal("retained repair did not restart Review without publication")
					}
				} else if restart != "" || f.remoteHead(t) != f.localHead(t) {
					t.Fatalf("retained repair was not published\n%s", f.log())
				}
			})
		}
	}
}

func TestCIStep_CommitRepairNormalizesFixerCommitsBetweenBaseMerges(t *testing.T) {
	f := newCIRepairFixture(t, true, nil)
	f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
	var bases []string
	for i := 0; i < 2; i++ {
		gitCmd(t, f.dir, "checkout", "main")
		writeFile(t, f.dir, fmt.Sprintf("base%d.txt", i), "upstream change")
		gitCmd(t, f.dir, "add", "-A")
		gitCmd(t, f.dir, "commit", "-m", fmt.Sprintf("upstream change %d", i))
		bases = append(bases, gitCmd(t, f.dir, "rev-parse", "HEAD"))
		gitCmd(t, f.dir, "push", "origin", "main")
		gitCmd(t, f.dir, "checkout", "feature")
		gitCmd(t, f.dir, "merge", "--no-ff", "-m", fmt.Sprintf("integrate base %d", i), bases[i])
		agentCommit(t, f.dir, fmt.Sprintf("fix%d.txt", i), fmt.Sprintf("unshaped fix %d", i))
	}
	before := strings.Fields(gitCmd(t, f.dir, "rev-list", "--first-parent", "--reverse", f.headSHA+"..HEAD"))
	repair, err := (&CIStep{}).commitRepair(f.sctx, "recover all retained fixes", nil)
	if err != nil || !repair.HeadAdvanced || !repair.Revalidate {
		t.Fatalf("repair = (%+v, %v)", repair, err)
	}
	after := strings.Fields(gitCmd(t, f.dir, "rev-list", "--first-parent", "--reverse", f.headSHA+"..HEAD"))
	if len(before) != 4 || len(after) != 4 {
		t.Fatalf("commit chains before=%v after=%v", before, after)
	}
	for i, sha := range after {
		if gitCmd(t, f.dir, "show", "-s", "--format=%T %an %ae %ad", "--date=raw", sha) != gitCmd(t, f.dir, "show", "-s", "--format=%T %an %ae %ad", "--date=raw", before[i]) {
			t.Fatalf("normalization changed tree or author at commit %d", i)
		}
		parents := strings.Fields(gitCmd(t, f.dir, "show", "-s", "--format=%P", sha))
		wantParent := f.headSHA
		if i > 0 {
			wantParent = after[i-1]
		}
		if parents[0] != wantParent {
			t.Fatalf("commit %d no longer follows the branch history", i)
		}
		if i%2 == 0 {
			if len(parents) != 2 || parents[1] != bases[i/2] || fullMessage(t, f.dir, sha) != fullMessage(t, f.dir, before[i]) {
				t.Fatalf("base merge %d lost its parent or message", i)
			}
		} else if len(parents) != 1 || fullMessage(t, f.dir, sha) != "no-mistakes(ci): recover all retained fixes" {
			t.Fatalf("fixer commit %d bypassed the commit settings", i)
		}
	}
}

func TestCIStep_CommitRepairMergeStrategyStillRefusesAnUnrelatedMerge(t *testing.T) {
	sctx, dir, _ := agentCommitContext(t, nil)
	sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
	gitCmd(t, dir, "fetch", "origin", "main")
	agentMergeCommit(t, dir, sctx.Run.HeadSHA)
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	_, err := (&CIStep{}).commitRepair(sctx, "repair", nil)
	if !errors.Is(err, errAgentCommitsNotLinear) || gitCmd(t, dir, "rev-parse", "HEAD") != head {
		t.Fatalf("unrelated merge was not refused untouched: %v", err)
	}
}
