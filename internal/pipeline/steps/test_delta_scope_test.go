package steps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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
