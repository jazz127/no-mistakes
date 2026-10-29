package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type retainedRepairLogHost struct {
	scm.Host
	heads []string
}

func (h *retainedRepairLogHost) FetchFailedCheckTargetLogs(_ context.Context, _ *scm.PR, _, head string, targets []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	h.heads = append(h.heads, head)
	return []scm.FailedCheckLog{{Target: targets[0], Output: "test failed on the published head"}}, nil
}

func TestCIResumedRepairRecoversUnpublishedWork(t *testing.T) {
	for _, revalidate := range []bool{false, true} {
		for _, state := range []string{"committed-crash", "recorded-timeout", "merged-crash"} {
			t.Run(fmt.Sprintf("%s/revalidate=%t", state, revalidate), func(t *testing.T) {
				f := newCIRepairFixture(t, revalidate, nil)
				mergeBase := ""
				if state == "merged-crash" {
					f.sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
					mergeBase = advanceBaseOverFeature(t, f)
				}
				targets := ciTargetsFor([]string{"test"}, mergeBase != "")
				selected, err := types.MarshalFindingsJSON(targets.Findings)
				if err != nil {
					t.Fatal(err)
				}
				f.sctx.PreviousFindings = selected
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
						if mergeBase == "" {
							writeCIFix(f.dir)
							gitCmd(t, f.dir, "add", "-A")
							gitCmd(t, f.dir, "commit", "-m", "retained repair")
						}
						if state == "recorded-timeout" {
							return nil, pipeline.ErrAgentTimeout
						}
						return nil, errors.New("fixer crashed after committing")
					}
					return &agent.Result{Output: []byte(`{"summary":"recover retained repair","code_change_needed":false}`)}, nil
				}}
				host, reason := buildHost(f.sctx, scm.ProviderGitHub)
				if host == nil {
					t.Fatal(reason)
				}
				logs := &retainedRepairLogHost{Host: host}
				pr := &scm.PR{Number: "42", URL: *f.sctx.Run.PRURL, BaseBranch: "main"}
				step := &CIStep{}
				first, err := step.repairFromFindings(f.sctx, logs, pr)
				if err != nil {
					t.Fatal(err)
				}
				if state == "recorded-timeout" {
					if first == nil || !first.NeedsApproval || f.sctx.Run.HeadSHA != f.localHead(t) {
						t.Fatalf("timeout did not retain its head at a gate: %+v", first)
					}
				} else if first != nil {
					t.Fatalf("crash outcome = %+v, want monitoring", first)
				}
				retainedTree := gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}")
				if f.localHead(t) == f.headSHA || f.remoteHead(t) != f.headSHA {
					t.Fatal("failed fixer did not leave an unpublished repair")
				}
				markedRunning := false
				f.sctx.MarkRunning = func() error { markedRunning = true; return nil }
				outcome, err := step.repairFromFindings(f.sctx, logs, pr)
				if err != nil {
					t.Fatal(err)
				}
				if fixCalls != 2 {
					t.Fatalf("fixer calls = %d, want the retained repair to reach recovery", fixCalls)
				}
				if len(logs.heads) != 2 || logs.heads[0] != f.headSHA || logs.heads[1] != f.headSHA {
					t.Fatalf("log request heads = %v, want published head %s on both turns", logs.heads, f.headSHA)
				}
				if got := gitCmd(t, f.dir, "rev-parse", "HEAD^{tree}"); got != retainedTree {
					t.Fatalf("recovery changed the retained tree: %s != %s", got, retainedTree)
				}
				if mergeBase != "" {
					parents := strings.Fields(gitCmd(t, f.dir, "show", "-s", "--format=%P", "HEAD"))
					if len(parents) != 2 || parents[0] != f.headSHA || parents[1] != mergeBase {
						t.Fatalf("recovery lost the proven merge: %v", parents)
					}
				}
				run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if run.HeadSHA != f.localHead(t) {
					t.Fatalf("recorded head = %s, want recovered head %s", run.HeadSHA, f.localHead(t))
				}
				if revalidate || state == "recorded-timeout" {
					if outcome == nil || outcome.RestartFrom != types.StepReview || markedRunning || f.remoteHead(t) != f.headSHA || run.ReviewApprovedHeadSHA != nil {
						t.Fatalf("retained repair must revalidate without publishing: %+v", outcome)
					}
				} else if outcome != nil || !markedRunning || f.remoteHead(t) != run.HeadSHA || run.LastPushedSHA == nil || *run.LastPushedSHA != run.HeadSHA {
					t.Fatalf("continuous repair did not publish and resume monitoring: %+v\n%s", outcome, f.log())
				}
			})
		}
	}
}

func TestCIResumedRepairRetainedWorkStillRequiresFreshPublishedFailures(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		for _, observation := range []string{"green", "moved-head"} {
			t.Run(fmt.Sprintf("%s/recorded=%t", observation, recorded), func(t *testing.T) {
				f := newCIRepairFixture(t, false, nil)
				writeCIFix(f.dir)
				gitCmd(t, f.dir, "add", "-A")
				gitCmd(t, f.dir, "commit", "-m", "retained repair")
				head := f.localHead(t)
				if recorded {
					if _, err := (&CIStep{}).recordLocalRepair(f.sctx, head); err != nil {
						t.Fatal(err)
					}
				}
				for i, entry := range f.sctx.Env {
					if observation == "green" && strings.HasPrefix(entry, "FAKE_CLI_CHECKS=") {
						f.sctx.Env[i] = `FAKE_CLI_CHECKS=[{"name":"test","state":"SUCCESS","bucket":"pass"}]`
					}
					if observation == "moved-head" && strings.HasPrefix(entry, "FAKE_CLI_PR_HEAD_SHA=") {
						f.sctx.Env[i] = "FAKE_CLI_PR_HEAD_SHA=unrelated-head"
					}
				}
				f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					t.Fatal("fixer ran without a fresh failure at the published head")
					return nil, nil
				}}
				f.sctx.PreviousFindings = ciGateFindingsJSON("test")
				host, reason := buildHost(f.sctx, scm.ProviderGitHub)
				if host == nil {
					t.Fatal(reason)
				}
				outcome, err := (&CIStep{}).repairFromFindings(f.sctx, host, &scm.PR{Number: "42", URL: *f.sctx.Run.PRURL})
				if err != nil || outcome != nil {
					t.Fatalf("repair = (%+v, %v), want monitoring", outcome, err)
				}
				if f.localHead(t) != head || f.remoteHead(t) != f.headSHA {
					t.Fatal("rejected snapshot changed the retained or published head")
				}
			})
		}
	}
}

func TestCIResumedRepairSettlesAPartialPublication(t *testing.T) {
	f := newCIRepairFixture(t, false, nil)
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: []byte(`{"summary":"repair the failing check","code_change_needed":false}`)}, nil
	}}
	f.sctx.PreviousFindings = ciGateFindingsJSON("test")
	writeCIFix(f.dir)
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "retained repair")
	gitCmd(t, f.dir, "push", "origin", "feature")
	repairHead := f.localHead(t)
	if repairHead == f.headSHA || f.remoteHead(t) != repairHead || f.sctx.Run.HeadSHA != f.headSHA {
		t.Fatal("repair did not reach the remote with its publication still unrecorded")
	}
	for i, entry := range f.sctx.Env {
		if strings.HasPrefix(entry, "FAKE_CLI_PR_HEAD_SHA=") {
			f.sctx.Env[i] = "FAKE_CLI_PR_HEAD_SHA=" + repairHead
		}
	}
	host, reason := buildHost(f.sctx, scm.ProviderGitHub)
	if host == nil {
		t.Fatal(reason)
	}
	logs := &retainedRepairLogHost{Host: host}
	markedRunning := false
	f.sctx.MarkRunning = func() error { markedRunning = true; return nil }
	pr := &scm.PR{Number: "42", URL: *f.sctx.Run.PRURL}
	outcome, err := (&CIStep{}).repairFromFindings(f.sctx, logs, pr)
	if err != nil || outcome != nil || !markedRunning {
		t.Fatalf("recovery = (%+v, %v), running=%t\n%s", outcome, err, markedRunning, f.log())
	}
	if len(logs.heads) != 1 || logs.heads[0] != repairHead {
		t.Fatalf("log request heads = %v, want published repair %s", logs.heads, repairHead)
	}
	run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.localHead(t) != repairHead || f.remoteHead(t) != repairHead || run.HeadSHA != repairHead || run.LastPushedSHA == nil || *run.LastPushedSHA != repairHead {
		t.Fatalf("recovery did not settle the exact published repair: %+v", run)
	}
}
