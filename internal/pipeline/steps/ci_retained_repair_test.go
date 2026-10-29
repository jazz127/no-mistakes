package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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
				if state == "recorded-timeout" {
					f.sctx.Config.Commit.FixMessage = "retained({{.Step}}): {{.Summary}}"
				}
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
							gitCmd(t, f.dir, "commit", "-m", "retained repair\n\n"+agentAttribution)
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
					if got := fullMessage(t, f.dir, "HEAD"); got != "retained(ci): repair failing checks" {
						t.Fatalf("timeout recorded an unnormalized commit message: %q", got)
					}
					run, err := f.sctx.DB.GetRun(f.sctx.Run.ID)
					if err != nil || run == nil || run.HeadSHA != f.localHead(t) || run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA {
						t.Fatalf("timeout custody binding = (%+v, %v)", run, err)
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

func TestCIResumedRepairSettlesRetainedWorkBeforeGreenReadiness(t *testing.T) {
	for _, revalidate := range []bool{false, true} {
		for _, fixing := range []bool{false, true} {
			for _, state := range []string{"crash", "timeout", "partial-publication"} {
				t.Run(fmt.Sprintf("%s/revalidate=%t/fixing=%t", state, revalidate, fixing), func(t *testing.T) {
					f := newCIRepairFixture(t, revalidate, nil)
					f.sctx.Config.Commit.FixMessage = "retained repair"
					f.sctx.Config.CI.ReleaseAfterChecksPass = true
					writeCIFix(f.dir)
					gitCmd(t, f.dir, "add", "-A")
					gitCmd(t, f.dir, "commit", "-m", "retained repair")
					head := f.localHead(t)
					step := &CIStep{}
					if state == "timeout" {
						park := step.ciFixAgentBudgetOutcome(f.sctx, nil, "test", pipeline.ErrAgentTimeout)
						if park == nil || !park.NeedsApproval || f.sctx.Run.HeadSHA != head || f.remoteHead(t) != f.headSHA {
							t.Fatalf("timeout failed to park retained local work: %+v", park)
						}
					}
					if state == "partial-publication" {
						gitCmd(t, f.dir, "push", "origin", "feature")
					}
					for i, entry := range f.sctx.Env {
						if strings.HasPrefix(entry, "FAKE_CLI_CHECKS=") {
							f.sctx.Env[i] = `FAKE_CLI_CHECKS=[{"name":"test","state":"SUCCESS","bucket":"pass"}]`
						}
					}
					f.sctx.Fixing = fixing
					f.sctx.PreviousFindings = ciGateFindingsJSON("test")
					f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
						t.Fatal("green published checks authorized another fixer")
						return nil, nil
					}}
					markedRunning := false
					f.sctx.MarkRunning = func() error { markedRunning = true; return nil }
					f.sctx.CIReadinessChanged = func(ready, _ bool) {
						if ready {
							t.Error("stale published checks made the retained head ready")
						}
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					f.sctx.Ctx = ctx
					step.waitForNextPoll = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
					outcome, err := step.Execute(f.sctx)
					run, readErr := f.sctx.DB.GetRun(f.sctx.Run.ID)
					if readErr != nil || run == nil || run.HeadSHA != head || f.localHead(t) != head || run.PushActive {
						t.Fatalf("retained custody was not settled: (%+v, %v)", run, readErr)
					}
					if revalidate || state == "timeout" {
						if err != nil || outcome == nil || outcome.RestartFrom != types.StepReview || outcome.CIReadyNoCI != nil || markedRunning || run.ReviewApprovedHeadSHA != nil || run.LastPushedSHA == nil || *run.LastPushedSHA != f.headSHA {
							t.Fatalf("retained repair bypassed revalidation: (%+v, %v)\n%s", outcome, err, f.log())
						}
						wantRemote := f.headSHA
						if state == "partial-publication" {
							wantRemote = head
						}
						if f.remoteHead(t) != wantRemote {
							t.Fatal("revalidation changed the published head")
						}
						return
					}
					if !errors.Is(err, context.Canceled) || outcome != nil || !markedRunning || f.remoteHead(t) != head || run.LastPushedSHA == nil || *run.LastPushedSHA != head {
						t.Fatalf("publication was not settled before waiting for current checks: (%+v, %v)\n%s", outcome, err, f.log())
					}
					for i, entry := range f.sctx.Env {
						if strings.HasPrefix(entry, "FAKE_CLI_PR_HEAD_SHA=") {
							f.sctx.Env[i] = "FAKE_CLI_PR_HEAD_SHA=" + head
						}
					}
					f.sctx.Ctx = context.Background()
					f.sctx.Fixing = false
					outcome, err = step.Execute(f.sctx)
					if err != nil || outcome == nil || outcome.CIReadyNoCI == nil || *outcome.CIReadyNoCI {
						t.Fatalf("current green checks did not release the settled head: (%+v, %v)\n%s", outcome, err, f.log())
					}
				})
			}
		}
	}
}

func TestCIResumedRepairRecoveryRefusesMovedRemote(t *testing.T) {
	f := newCIRepairFixture(t, false, nil)
	f.sctx.Config.CI.ReleaseAfterChecksPass = true
	writeCIFix(f.dir)
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "retained repair")
	gitCmd(t, f.dir, "checkout", "-b", "external", f.headSHA)
	writeFile(t, f.dir, "external.txt", "independent remote work\n")
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "external update")
	moved := f.localHead(t)
	gitCmd(t, f.dir, "push", "origin", moved+":refs/heads/feature")
	gitCmd(t, f.dir, "checkout", "feature")
	for i, entry := range f.sctx.Env {
		if strings.HasPrefix(entry, "FAKE_CLI_CHECKS=") {
			f.sctx.Env[i] = `FAKE_CLI_CHECKS=[{"name":"test","state":"SUCCESS","bucket":"pass"}]`
		}
		if strings.HasPrefix(entry, "FAKE_CLI_PR_HEAD_SHA=") {
			f.sctx.Env[i] = "FAKE_CLI_PR_HEAD_SHA=" + moved
		}
	}
	f.sctx.Fixing = true
	f.sctx.PreviousFindings = ciGateFindingsJSON("test")
	f.sctx.Agent = &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("moved remote authorized another fixer")
		return nil, nil
	}}
	outcome, err := (&CIStep{}).Execute(f.sctx)
	if err == nil || outcome != nil || f.remoteHead(t) != moved || f.sctx.Run.HeadSHA != f.headSHA {
		t.Fatalf("recovery bypassed the moved-remote guard: (%+v, %v)\n%s", outcome, err, f.log())
	}
}
