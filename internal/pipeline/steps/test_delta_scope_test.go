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
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfiguredTestRunMode_FirstRunIsFullAndFixRerunIsDelta(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID

	mode, since, passed := configuredTestRunMode(sctx)
	if mode != "full" || since != "" || passed {
		t.Fatalf("first run mode = (%q, %q, %t), want (full, empty, false)", mode, since, passed)
	}
	prior := types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "full", TestedHeadSHA: "previous-head", TestCommandPassed: true, TestCommandRan: true}
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

// The environment contract is the oracle: initial/full, changed-head/delta
// since the last executed head, and one final/full. The real shell's receipt
// rejects stale inherited variables, wrong delta anchors, and duplicate full
// executions. Only the evidence-agent response is simulated.
func TestConfiguredTestCommandLifecycle(t *testing.T) {
	dir, baseSHA, firstHead := setupGitRepo(t)
	receipt := filepath.Join(t.TempDir(), "commands.txt")
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Purpose == "test-fix" {
			return &agent.Result{Output: json.RawMessage(`{"summary":"No changes applied"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"Command environment integration test","tested":["configured shell command"],"testing_summary":"Mode contract exercised with a simulated evidence agent","artifacts":[],"scenarios":[{"name":"evidence agent boundary","result":"untested","live":false,"surface":"simulated","evidence":"mockAgent","reason":"This integration test substitutes only the evidence agent"}],"verdict":"inconclusive"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, firstHead, config.Commands{
		Test: `printf '%s|%s|%s\n' "$NO_MISTAKES_TEST_MODE" "${NO_MISTAKES_TEST_SINCE-unset}" "$(git rev-parse HEAD)" >> "$MODE_RECEIPT"`,
	})
	sctx.Env = []string{"MODE_RECEIPT=" + receipt, "NO_MISTAKES_TEST_MODE=inherited", "NO_MISTAKES_TEST_SINCE=stale-inherited-head"}
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	execute := func(wantRan bool) {
		t.Helper()
		outcome, err := (&TestStep{}).Execute(sctx)
		if err != nil {
			t.Fatal(err)
		}
		findings, err := types.ParseFindingsJSON(outcome.Findings)
		if err != nil {
			t.Fatal(err)
		}
		if findings.TestCommandRan != wantRan || !findings.TestCommandPassed {
			t.Fatalf("command receipt = ran %t, passed %t; want ran %t and passed", findings.TestCommandRan, findings.TestCommandPassed, wantRan)
		}
		recordTestModeRound(t, sctx, sr.ID, findings)
		sctx.PreviousFindings = outcome.Findings
	}
	advance := func(message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte(message+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "feature.txt")
		gitCmd(t, dir, "commit", "-m", message)
		head := gitCmd(t, dir, "rev-parse", "HEAD")
		if err := recordAgentFixHead(sctx, types.StepTest, head); err != nil {
			t.Fatal(err)
		}
		return head
	}
	execute(true)
	execute(false)
	secondHead := advance("review repair")
	sctx.Fixing = true
	execute(true)
	finalHead := advance("post-test correction")
	sctx.Fixing = false
	execute(true)
	sctx.ForceFullTest = true
	execute(true)
	execute(false)
	got, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	want := "full|unset|" + firstHead + "\n" +
		"delta|" + firstHead + "|" + secondHead + "\n" +
		"delta|" + secondHead + "|" + finalHead + "\n" +
		"full|unset|" + finalHead + "\n"
	if string(got) != want {
		t.Fatalf("actual shell command invocations:\n%s\nwant:\n%s", got, want)
	}
	t.Logf("Actual configured-command receipts (mode|since|HEAD):\n%s", got)
	rounds, err := sctx.DB.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	md := BuildTestingSummaryForPR([]*db.StepResult{sr}, map[string][]*db.StepRound{sr.ID: rounds}, "https://github.com/example/disposable", finalHead, dir, "", nil)
	t.Logf("Generated PR Testing markdown from persisted receipts (simulated evidence agent):\n%s", md)
}

func TestConfiguredTestRunModeReusesFullPassOnSameHead(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	recordTestModeRound(t, sctx, sr.ID, types.Findings{
		TestCommand: sctx.Config.Commands.Test, TestMode: "full", TestedHeadSHA: headSHA, TestCommandPassed: true, TestCommandRan: true,
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

func TestConfiguredTestCommandChangeInvalidatesReceipts(t *testing.T) {
	for _, mode := range []string{"full", "delta"} {
		for _, command := range []string{"exit 0", ""} {
			t.Run(mode+"/recorded-command="+command, func(t *testing.T) {
				dir, baseSHA, headSHA := setupGitRepo(t)
				ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if opts.Purpose == "test-fix" {
						return &agent.Result{Output: json.RawMessage(`{"summary":"No changes applied"}`)}, nil
					}
					return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
				}}
				sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{
					Test: `printf '%s|%s\n' "$NO_MISTAKES_TEST_MODE" "${NO_MISTAKES_TEST_SINCE-unset}" >> "$TEST_RECEIPT"`,
				})
				receipt := filepath.Join(t.TempDir(), "commands.txt")
				sctx.Env = []string{"TEST_RECEIPT=" + receipt}
				sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
				if err != nil {
					t.Fatal(err)
				}
				sctx.StepResultID = sr.ID
				recordTestModeRound(t, sctx, sr.ID, types.Findings{
					TestCommand: command, TestMode: mode, TestedHeadSHA: headSHA,
					TestCommandRan: true, TestCommandPassed: true,
				})
				outcome, err := (&PushStep{}).Execute(sctx)
				if err != nil || outcome.RestartFrom != types.StepTest || !outcome.RequireFinalFullTest {
					t.Fatalf("Push accepted a receipt for a different or unknown command: %+v, %v", outcome, err)
				}
				sctx.Fixing = true
				outcome, err = (&TestStep{}).Execute(sctx)
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != "full|unset\n" {
					t.Fatalf("new command must run in full without the old delta anchor: %q", got)
				}
				findings, err := types.ParseFindingsJSON(outcome.Findings)
				if err != nil {
					t.Fatal(err)
				}
				if findings.TestCommand != sctx.Config.Commands.Test || !findings.TestCommandRan || !findings.TestCommandPassed {
					t.Fatalf("new command receipt = %+v", findings)
				}
				recordTestModeRound(t, sctx, sr.ID, findings)
				if required, err := finalFullTestRequired(sctx, headSHA); err != nil || required {
					t.Fatalf("new command's full pass did not satisfy Push: %t, %v", required, err)
				}
				sctx.ForceFullTest = true
				sctx.PreviousFindings = outcome.Findings
				if _, err := (&TestStep{}).Execute(sctx); err != nil {
					t.Fatal(err)
				}
				got, err = os.ReadFile(receipt)
				if err != nil || string(got) != "full|unset\n" {
					t.Fatalf("same command's successful full run was repeated on the same head: %q, %v", got, err)
				}
			})
		}
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
		TestCommand: sctx.Config.Commands.Test, TestMode: "delta", TestSinceSHA: baseSHA, TestedHeadSHA: headSHA, TestCommandPassed: true, TestCommandRan: true,
	})
	needsFull, err := finalFullTestRequired(sctx, headSHA)
	if err != nil || !needsFull {
		t.Fatalf("delta-only evidence requires final full = (%t, %v), want (true, nil)", needsFull, err)
	}

	recordTestModeRound(t, sctx, sr.ID, types.Findings{
		TestCommand: sctx.Config.Commands.Test, TestMode: "full", TestedHeadSHA: headSHA, TestCommandPassed: true, TestCommandRan: true,
	})
	needsFull, err = finalFullTestRequired(sctx, headSHA)
	if err != nil || needsFull {
		t.Fatalf("full pass on pushed head requires another full = (%t, %v), want (false, nil)", needsFull, err)
	}
	needsFull, err = finalFullTestRequired(sctx, "later-head")
	if err != nil || !needsFull {
		t.Fatalf("full pass on earlier head satisfies later head = (%t, %v), want (true, nil)", needsFull, err)
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
		TestCommand: sctx.Config.Commands.Test, TestMode: "delta", TestSinceSHA: baseSHA, TestedHeadSHA: headSHA, TestCommandPassed: true, TestCommandRan: true,
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

func TestBaseAttributionCommandReceivesFullMode(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = []string{"NO_MISTAKES_TEST_MODE=delta", "NO_MISTAKES_TEST_SINCE=stale-head"}
	output, code, err := runTestCommandOnBase(sctx, `printf '%s|%s|%s\n' "$NO_MISTAKES_TEST_MODE" "${NO_MISTAKES_TEST_SINCE-unset}" "$(git rev-parse HEAD)"`, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	// Base attribution must execute the full command at the actual base,
	// independently of the environment inherited from a delta head run.
	if code != 0 || strings.TrimSpace(output) != "full|unset|"+baseSHA {
		t.Fatalf("base command: code=%d output=%q; want full|unset|%s", code, output, baseSHA)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("base attribution moved the run head: got %s, want %s", got, headSHA)
	}
	t.Logf("Actual base command receipt (mode|since|HEAD): %s", strings.TrimSpace(output))
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
			if phase != "initial evidence" {
				sctx.Fixing = true
				prior := types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "full", TestedHeadSHA: baseSHA, TestCommandRan: true, TestCommandPassed: true,
					Items: []types.Finding{{ID: "test-1", Severity: types.FindingSeverityError, Description: "repair behavior", Action: types.ActionAutoFix}}}
				raw, err := json.Marshal(prior)
				if err != nil {
					t.Fatal(err)
				}
				sctx.PreviousFindings = string(raw)
				recordTestModeRound(t, sctx, sr.ID, prior)
				wantHead = baseSHA
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
			if findings.TestMode != "" || findings.TestSinceSHA != "" || findings.TestCommandRan || findings.TestCommandPassed || findings.TestCommand != "" {
				t.Fatalf("timeout certified a command receipt: %+v", findings)
			}
			recordTestModeRound(t, sctx, sr.ID, findings)
			mode, since, passed := configuredTestRunMode(sctx)
			if passed || wantHead == "" && (mode != "full" || since != "") || wantHead != "" && (mode != "delta" || since != wantHead) {
				t.Fatalf("timeout changed retry selection: %q, %q, %t", mode, since, passed)
			}
			pushOutcome, err := (&PushStep{}).Execute(sctx)
			if err != nil || pushOutcome.RestartFrom != types.StepTest || !pushOutcome.RequireFinalFullTest {
				t.Fatalf("timeout must require final full before push: %+v, %v", pushOutcome, err)
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
	for _, mode := range []string{"full", "delta", "missing", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{
				Test:   `printf '%s|%s\n' "$NO_MISTAKES_TEST_MODE" "${NO_MISTAKES_TEST_SINCE-unset}" >> "$TEST_RECEIPT"`,
				Format: "echo formatted > feature.txt",
			})
			receipt := filepath.Join(t.TempDir(), "commands.txt")
			sctx.Env = []string{"TEST_RECEIPT=" + receipt}
			upstream := t.TempDir()
			gitCmd(t, upstream, "init", "--bare")
			gitCmd(t, dir, "remote", "set-url", "origin", upstream)
			gitCmd(t, dir, "push", "origin", "main")
			gitCmd(t, dir, "push", "origin", "feature")
			sctx.Repo.UpstreamURL = upstream
			setupGateMirror(t, sctx)
			recordReviewApproval(t, sctx, headSHA)
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			sctx.StepResultID = sr.ID
			if mode == "timeout" {
				ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return nil, errTestAgentTimeout
				}
				outcome, err := (&TestStep{}).Execute(sctx)
				if err != nil || !outcome.NeedsApproval {
					t.Fatalf("expected evidence timeout: %+v, %v", outcome, err)
				}
				findings, err := types.ParseFindingsJSON(outcome.Findings)
				if err != nil {
					t.Fatal(err)
				}
				recordTestModeRound(t, sctx, sr.ID, findings)
				if err := sctx.DB.CompleteStep(sr.ID, 0, 1, "test.log"); err != nil {
					t.Fatal(err)
				}
				ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
				}
			} else if mode != "missing" {
				recordTestModeRound(t, sctx, sr.ID, types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: mode, TestedHeadSHA: headSHA, TestCommandRan: true, TestCommandPassed: true})
			}
			outcome, err := (&PushStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.RestartFrom != types.StepTest || !outcome.RequireFinalFullTest {
				t.Fatalf("formatter head needs final full validation directly: %+v", outcome)
			}
			if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != headSHA {
				t.Fatalf("Push published an untested formatter head: %s", got)
			}
			formattedHead := gitCmd(t, dir, "rev-parse", "HEAD")
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if formattedHead == headSHA || run.HeadSHA != formattedHead || sctx.Run.HeadSHA != formattedHead {
				t.Fatalf("formatter head not adopted: live=%s recorded=%s memory=%s", formattedHead, run.HeadSHA, sctx.Run.HeadSHA)
			}
			sctx.ForceFullTest = outcome.RequireFinalFullTest
			outcome, err = (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if findings.TestMode != "full" || findings.TestSinceSHA != "" || findings.TestedHeadSHA != formattedHead || !findings.TestCommandRan {
				t.Fatalf("Test reused the pre-formatter result: %+v", findings)
			}
			recordTestModeRound(t, sctx, sr.ID, findings)
			if required, err := finalFullTestRequired(sctx, formattedHead); err != nil || required {
				t.Fatalf("full pass did not satisfy Push: required=%t error=%v", required, err)
			}
			outcome, err = (&PushStep{}).Execute(sctx)
			if err != nil || outcome.RestartFrom != "" {
				t.Fatalf("Push after final full: %+v, %v", outcome, err)
			}
			if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != formattedHead {
				t.Fatalf("Push head = %s, want validated head %s", got, formattedHead)
			}
			got, err := os.ReadFile(receipt)
			if err != nil {
				t.Fatal(err)
			}
			want := "full|unset\n"
			if mode == "timeout" {
				want += "full|unset\n"
			}
			if string(got) != want {
				t.Fatalf("expected one full invocation on the formatter head: got %q, want %q", got, want)
			}
		})
	}
}

func TestConfiguredTestFinalFullRequestIsConsumedByOneAttempt(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: `test -f "$TEST_SUCCESS_FILE"`})
	successFile := filepath.Join(t.TempDir(), "passed")
	sctx.Env = []string{"TEST_SUCCESS_FILE=" + successFile}
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	recordTestModeRound(t, sctx, sr.ID, types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "delta", TestedHeadSHA: headSHA, TestSinceSHA: baseSHA, TestCommandRan: true, TestCommandPassed: true})
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
	if err := os.WriteFile(successFile, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	recordTestModeRound(t, sctx, sr.ID, types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "delta", TestedHeadSHA: formattedHead, TestCommandRan: true, TestCommandPassed: true})
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

func TestFinalFullTestGateRequiresSuccessDespiteApprovedFailure(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 7"})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	recordTestModeRound(t, sctx, sr.ID, types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "delta", TestedHeadSHA: headSHA, TestCommandRan: true, TestCommandPassed: true})
	failed := types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "full", TestedHeadSHA: headSHA, TestCommandRan: true,
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
	if required, err := finalFullTestRequired(sctx, headSHA); err != nil || !required {
		t.Fatalf("approved failure satisfied final full: %t, %v", required, err)
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
	for _, validation := range []string{"no command history", "full on an earlier head", "final full after delta"} {
		t.Run(validation, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 7"})
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			failed := types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "full", TestedHeadSHA: baseSHA, TestCommandRan: true}
			if validation == "final full after delta" {
				recordTestModeRound(t, sctx, sr.ID, types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "delta", TestedHeadSHA: headSHA, TestSinceSHA: baseSHA, TestCommandRan: true, TestCommandPassed: true})
				failed.TestedHeadSHA = headSHA
			}
			if validation != "no command history" {
				recordTestModeRound(t, sctx, sr.ID, failed)
			}
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
	for _, history := range []string{"full pass on final head", "full pass after delta"} {
		t.Run(history, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			if history == "full pass after delta" {
				recordTestModeRound(t, sctx, sr.ID, types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "delta", TestedHeadSHA: headSHA, TestSinceSHA: baseSHA, TestCommandRan: true, TestCommandPassed: true})
			}
			recordTestModeRound(t, sctx, sr.ID, types.Findings{TestCommand: sctx.Config.Commands.Test, TestMode: "full", TestedHeadSHA: headSHA, TestCommandRan: true, TestCommandPassed: true})
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
