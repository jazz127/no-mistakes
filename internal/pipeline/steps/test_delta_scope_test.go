package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfiguredTestRunMode_FirstRunIsFullAndFixRerunIsDelta(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID

	mode, since, passed := configuredTestRunMode(sctx)
	if mode != "full" || since != "" || passed {
		t.Fatalf("first run mode = (%q, %q, %t), want (full, empty, false)", mode, since, passed)
	}
	prior := types.Findings{TestMode: "full", TestedHeadSHA: "previous-head", TestCommandPassed: true, TestCommandRan: true}
	raw, _ := json.Marshal(prior)
	if _, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", testStringPtr(string(raw)), nil, 1); err != nil {
		t.Fatal(err)
	}
	mode, since, passed = configuredTestRunMode(sctx)
	if mode != "delta" || since != "previous-head" || passed {
		t.Fatalf("fix rerun mode = (%q, %q, %t), want (delta, previous-head, false)", mode, since, passed)
	}
}

func TestConfiguredTestCommandReceivesModeAndSince(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	modePath := filepath.Join(t.TempDir(), "mode.txt")
	command := "printf '%s|%s' \"$NO_MISTAKES_TEST_MODE\" \"${NO_MISTAKES_TEST_SINCE-unset}\" > " + modePath
	for _, tc := range []struct {
		mode, since, want string
	}{{"full", "", "full|unset"}, {"delta", "abc123", "delta|abc123"}} {
		if _, _, err := runConfiguredChecksForTest(sctx, command, tc.mode, tc.since); err != nil {
			t.Fatalf("run %s command: %v", tc.mode, err)
		}
		got, err := os.ReadFile(modePath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != tc.want {
			t.Errorf("%s environment = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestConfiguredTestRunModeReusesFullPassOnSameHead(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	recordTestModeRound(t, sctx, sr.ID, types.Findings{
		TestMode: "full", TestedHeadSHA: headSHA, TestCommandPassed: true, TestCommandRan: true,
	})
	mode, since, passed := configuredTestRunMode(sctx)
	if mode != "full" || since != "" || !passed {
		t.Fatalf("same-head mode = (%q, %q, %t), want (full, empty, true)", mode, since, passed)
	}
	sctx.ForceFullTest = true
	mode, since, passed = configuredTestRunMode(sctx)
	if mode != "full" || since != "" || !passed {
		t.Fatalf("forced mode = (%q, %q, %t), want full with the existing pass still visible", mode, since, passed)
	}
}

func TestFinalFullTestGateRequiresFullPassOnPushedHead(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	recordTestModeRound(t, sctx, sr.ID, types.Findings{
		TestMode: "delta", TestSinceSHA: baseSHA, TestedHeadSHA: headSHA, TestCommandPassed: true, TestCommandRan: true,
	})
	needsFull, err := finalFullTestRequired(sctx, headSHA)
	if err != nil || !needsFull {
		t.Fatalf("delta-only evidence requires final full = (%t, %v), want (true, nil)", needsFull, err)
	}

	recordTestModeRound(t, sctx, sr.ID, types.Findings{
		TestMode: "full", TestedHeadSHA: headSHA, TestCommandPassed: true, TestCommandRan: true,
	})
	needsFull, err = finalFullTestRequired(sctx, headSHA)
	if err != nil || needsFull {
		t.Fatalf("full pass on pushed head requires another full = (%t, %v), want (false, nil)", needsFull, err)
	}
	needsFull, err = finalFullTestRequired(sctx, "later-head")
	if err != nil || !needsFull {
		t.Fatalf("full pass on earlier head satisfies later head = (%t, %v), want (true, nil)", needsFull, err)
	}
	state, err := configuredTestStateForHead(sctx, "later-head")
	if err != nil || state.ranOnHead || state.lastTestedHead != headSHA {
		t.Fatalf("state for an untested final head = (%+v, %v), want last head %s and no run on target", state, err, headSHA)
	}
}

func TestPushStepRestartsAtTestForFinalFullRun(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	recordTestModeRound(t, sctx, sr.ID, types.Findings{
		TestMode: "delta", TestSinceSHA: baseSHA, TestedHeadSHA: headSHA, TestCommandPassed: true, TestCommandRan: true,
	})

	outcome, err := (&PushStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.RestartFrom != types.StepTest || !outcome.RequireFinalFullTest {
		t.Fatalf("Push outcome = %#v, want a mandatory restart at Test", outcome)
	}
}

func TestBaseAttributionTestEnvironmentIsAlwaysFull(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{"NO_MISTAKES_TEST_MODE=delta", "NO_MISTAKES_TEST_SINCE=stale-head"}
	env := testCommandEnvironment(sctx, "full", "")
	var mode, since string
	for _, entry := range env {
		if strings.HasPrefix(entry, "NO_MISTAKES_TEST_MODE=") {
			mode = strings.TrimPrefix(entry, "NO_MISTAKES_TEST_MODE=")
		}
		if strings.HasPrefix(entry, "NO_MISTAKES_TEST_SINCE=") {
			since = strings.TrimPrefix(entry, "NO_MISTAKES_TEST_SINCE=")
		}
	}
	if mode != "full" || since != "" {
		t.Fatalf("base attribution environment = (%q, %q), want (full, unset)", mode, since)
	}
}

func TestConfiguredTestTimeoutReceiptsDoNotCertifyCommandHistory(t *testing.T) {
	for _, phase := range []string{"initial evidence", "fix evidence", "fix repair"} {
		t.Run(phase, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
				if phase == "fix evidence" && opts.Purpose == "test-fix" {
					return &agent.Result{Output: json.RawMessage(`{"summary":"No changes applied"}`)}, nil
				}
				return nil, errTestAgentTimeout
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			sctx.StepResultID = sr.ID
			wantHead := ""
			wantModes := 0
			if phase != "initial evidence" {
				sctx.Fixing = true
				prior := types.Findings{TestMode: "full", TestedHeadSHA: baseSHA, TestCommandRan: true, TestCommandPassed: true,
					Items: []types.Finding{{ID: "test-1", Severity: types.FindingSeverityError, Description: "repair behavior", Action: types.ActionAutoFix}}}
				raw, err := json.Marshal(prior)
				if err != nil {
					t.Fatal(err)
				}
				sctx.PreviousFindings = string(raw)
				recordTestModeRound(t, sctx, sr.ID, prior)
				wantHead = baseSHA
				wantModes = 1
			}
			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval {
				t.Fatal("timeout must park Test")
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if findings.TestedHeadSHA != wantHead {
				t.Fatalf("timeout changed the live-evidence head: got %q, want %q", findings.TestedHeadSHA, wantHead)
			}
			if findings.TestMode != "" || findings.TestSinceSHA != "" || findings.TestCommandRan || findings.TestCommandPassed || findings.BaseTestMode != "" || findings.BaseTestHeadSHA != "" {
				t.Fatalf("timeout certified a command receipt: %+v", findings)
			}
			recordTestModeRound(t, sctx, sr.ID, findings)
			state, err := configuredTestStateForHead(sctx, headSHA)
			if err != nil || state.lastTestedHead != wantHead || state.ranOnHead || state.hadDelta {
				t.Fatalf("timeout changed command history: %+v, %v", state, err)
			}
			mode, since, passed := configuredTestRunMode(sctx)
			if passed || wantHead == "" && (mode != "full" || since != "") || wantHead != "" && (mode != "delta" || since != wantHead) {
				t.Fatalf("timeout changed retry selection: %q, %q, %t", mode, since, passed)
			}
			rounds, err := sctx.DB.GetRoundsByStep(sr.ID)
			if err != nil {
				t.Fatal(err)
			}
			if modes := collectConfiguredTestModes(sr, rounds); len(modes) != wantModes {
				t.Fatalf("PR ledger included the timeout receipt: %v", modes)
			}
			sctx.Fixing = false
			sctx.ForceFullTest = true
			ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			}
			outcome, err = (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			findings, err = types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			recordTestModeRound(t, sctx, sr.ID, findings)
			if required, err := finalFullTestRequired(sctx, headSHA); err != nil || required {
				t.Fatalf("successful retry left Push blocked: required=%t, error=%v", required, err)
			}
		})
	}
}

func TestPushStepRecordsFormatterHeadBeforeTestRestarts(t *testing.T) {
	for _, mode := range []string{"full", "delta"} {
		t.Run(mode, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0", Format: "echo formatted > feature.txt"})
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			sctx.StepResultID = sr.ID
			recordTestModeRound(t, sctx, sr.ID, types.Findings{TestMode: mode, TestedHeadSHA: headSHA, TestCommandRan: true, TestCommandPassed: true})
			outcome, err := (&PushStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.RestartFrom != types.StepTest || outcome.RequireFinalFullTest {
				t.Fatalf("formatter head needs delta validation first: %+v", outcome)
			}
			formattedHead := gitCmd(t, dir, "rev-parse", "HEAD")
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if formattedHead == headSHA || run.HeadSHA != formattedHead || sctx.Run.HeadSHA != formattedHead {
				t.Fatalf("formatter head not adopted: live=%s recorded=%s memory=%s", formattedHead, run.HeadSHA, sctx.Run.HeadSHA)
			}
			outcome, err = (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if findings.TestMode != "delta" || findings.TestSinceSHA != headSHA || findings.TestedHeadSHA != formattedHead || !findings.TestCommandRan {
				t.Fatalf("Test reused the pre-formatter result: %+v", findings)
			}
			recordTestModeRound(t, sctx, sr.ID, findings)
			outcome, err = (&PushStep{}).Execute(sctx)
			if err != nil || outcome.RestartFrom != types.StepTest || !outcome.RequireFinalFullTest {
				t.Fatalf("Push did not advance to final full: %+v, %v", outcome, err)
			}
		})
	}
}

func TestConfiguredTestFinalFullRequestIsConsumedByOneAttempt(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 7"})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	recordTestModeRound(t, sctx, sr.ID, types.Findings{TestMode: "delta", TestedHeadSHA: headSHA, TestSinceSHA: baseSHA, TestCommandRan: true, TestCommandPassed: true})
	sctx.ForceFullTest = true
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if findings.TestMode != "full" || findings.TestCommandPassed || !outcome.NeedsApproval || sctx.ForceFullTest {
		t.Fatalf("failed final full did not consume its request: %+v, force=%t", findings, sctx.ForceFullTest)
	}
	recordTestModeRound(t, sctx, sr.ID, findings)
	sctx.Config.Commands.Test = "exit 0"
	sctx.Fixing = true
	sctx.PreviousFindings = outcome.Findings
	outcome, err = (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err = types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if findings.TestMode != "delta" || findings.TestSinceSHA != headSHA {
		t.Fatalf("repair inherited the final-full request: %+v", findings)
	}
	recordTestModeRound(t, sctx, sr.ID, findings)
	if required, err := finalFullTestRequired(sctx, headSHA); err != nil || !required {
		t.Fatalf("delta repair must require a new final full: required=%t error=%v", required, err)
	}
}

func TestPushStepRecordsAdvancedHeadBeforeFinalFullRestart(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("formatted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "feature.txt")
	gitCmd(t, dir, "commit", "-m", "formatter correction")
	formattedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	recordTestModeRound(t, sctx, sr.ID, types.Findings{TestMode: "delta", TestedHeadSHA: formattedHead, TestCommandRan: true, TestCommandPassed: true})
	outcome, err := (&PushStep{}).Execute(sctx)
	if err != nil || outcome.RestartFrom != types.StepTest || !outcome.RequireFinalFullTest {
		t.Fatalf("Push outcome = %+v, %v, want final full restart", outcome, err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.HeadSHA != formattedHead || sctx.Run.HeadSHA != formattedHead {
		t.Fatalf("final full restart retained stale head: durable=%s memory=%s want=%s", run.HeadSHA, sctx.Run.HeadSHA, formattedHead)
	}
}

func TestFinalFullTestGateHonorsApprovedFailureOnlyOnItsHead(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 7"})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	recordTestModeRound(t, sctx, sr.ID, types.Findings{TestMode: "delta", TestedHeadSHA: headSHA, TestCommandRan: true, TestCommandPassed: true})
	failed := types.Findings{TestMode: "full", TestedHeadSHA: headSHA, TestCommandRan: true,
		Items: []types.Finding{{Severity: types.FindingSeverityError, Category: types.FindingCategoryTestCommand, Description: "configured test command failed with exit code 7"}}}
	recordTestModeRound(t, sctx, sr.ID, failed)
	raw, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.ParkStepForApproval(sctx.Run.ID, sr.ID, types.StepStatusAwaitingApproval, 7, 1, testStringPtr(string(raw))); err != nil {
		t.Fatal(err)
	}
	if required, err := finalFullTestRequired(sctx, headSHA); err != nil || !required {
		t.Fatalf("unapproved failure satisfied final full: %t, %v", required, err)
	}
	reason, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetTestApprovalReason(sr.ID, "accept known failure"); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepOverrideReason(sr.ID, reason); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStep(sr.ID, 7, 1, "test.log"); err != nil {
		t.Fatal(err)
	}
	if required, err := finalFullTestRequired(sctx, headSHA); err != nil || required {
		t.Fatalf("approved final full failure restarted Test: %t, %v", required, err)
	}
	if required, err := finalFullTestRequired(sctx, "later-head"); err != nil || !required {
		t.Fatalf("approval satisfied a different head: %t, %v", required, err)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := buildPipelineSummaryFor(steps, nil, headSHA, scm.ProviderUnknown, pipelineAttestationPolicy{AllowTestCommandOverride: "known failure"})
	attestation := parsePipelineAttestationForTest(t, md)
	if attestation.AllowTestCommandOverride != "known failure" || len(attestation.Steps) != 1 || attestation.Steps[0].OverrideReason != "configured test command failed with exit code 7" {
		t.Fatalf("final full approval lost its override attestation: %+v", attestation)
	}
}

func TestPushStepRefusesRestartWhenPersistedTestIsSkipped(t *testing.T) {
	for _, validation := range []string{"delta on a new head", "final full after delta"} {
		t.Run(validation, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 7"})
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			failed := types.Findings{TestMode: "full", TestedHeadSHA: baseSHA, TestCommandRan: true}
			if validation == "final full after delta" {
				recordTestModeRound(t, sctx, sr.ID, types.Findings{TestMode: "delta", TestedHeadSHA: headSHA, TestSinceSHA: baseSHA, TestCommandRan: true, TestCommandPassed: true})
				failed.TestedHeadSHA = headSHA
			}
			recordTestModeRound(t, sctx, sr.ID, failed)
			raw, err := json.Marshal(failed)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.ParkStepForApproval(sctx.Run.ID, sr.ID, types.StepStatusAwaitingApproval, 7, 1, testStringPtr(string(raw))); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 7, 1, "test.log"); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				outcome, err := (&PushStep{}).Execute(sctx)
				if err == nil || !strings.Contains(err.Error(), "Test step is skipped") || !strings.Contains(err.Error(), "required validation cannot be restarted") || !strings.Contains(err.Error(), headSHA) {
					t.Fatalf("Push must explain the unsatisfiable Test requirement: outcome=%+v error=%v", outcome, err)
				}
				if outcome != nil {
					t.Fatalf("skipped Test must fail rather than request another restart: %+v", outcome)
				}
				if err := sctx.DB.ResetStepsFrom(sctx.Run.ID, types.StepTest.Order()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestConfiguredTestSkipWithoutOutstandingValidationDoesNotFail(t *testing.T) {
	for _, history := range []string{"no command history", "full pass on final head", "full pass after delta"} {
		t.Run(history, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if history == "full pass after delta" {
				recordTestModeRound(t, sctx, sr.ID, types.Findings{TestMode: "delta", TestedHeadSHA: headSHA, TestSinceSHA: baseSHA, TestCommandRan: true, TestCommandPassed: true})
			}
			if history != "no command history" {
				recordTestModeRound(t, sctx, sr.ID, types.Findings{TestMode: "full", TestedHeadSHA: headSHA, TestCommandRan: true, TestCommandPassed: true})
			}
			if err := sctx.DB.CompleteStepWithStatus(sr.ID, types.StepStatusSkipped, 0, 1, "test.log"); err != nil {
				t.Fatal(err)
			}
			if required, err := finalFullTestRequired(sctx, headSHA); err != nil || required {
				t.Fatalf("skip introduced an unnecessary validation requirement: required=%t error=%v", required, err)
			}
		})
	}
}

func recordTestModeRound(t *testing.T, sctx *pipeline.StepContext, stepID string, findings types.Findings) {
	t.Helper()
	raw, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := sctx.DB.GetRoundsByStep(stepID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(stepID, len(rounds)+1, "initial", testStringPtr(string(raw)), nil, 1); err != nil {
		t.Fatal(err)
	}
}

var _ agent.Agent = (*mockAgent)(nil)

func testStringPtr(value string) *string { return &value }
