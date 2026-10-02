package steps

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The fixtures below are the three run shapes the live-validation contract has
// to render, taken from the shapes the scout's audit found on real pipeline
// runs: a run that drove everything live and shipped, a run that drove part of
// its list and had to report the rest untested because this machine lacked the
// capability, and a run whose scenario failed.

func liveValidatedFindingsJSON(t *testing.T, scenarios []types.TestScenario, verdict string, testedHead ...string) string {
	t.Helper()
	headSHA := testPipelineHeadSHA
	if len(testedHead) > 0 {
		headSHA = testedHead[0]
	}
	raw, err := json.Marshal(types.Findings{
		Summary:        "",
		Tested:         []string{"`npm run e2e -- checkout`"},
		TestingSummary: "drove the checkout scenarios against a running app",
		Scenarios:      scenarios,
		Verdict:        verdict,
		TestedHeadSHA:  headSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func testStepWithFindings(t *testing.T, findingsJSON string) ([]*db.StepResult, map[string][]*db.StepRound) {
	t.Helper()
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findingsJSON}}
	rounds := map[string][]*db.StepRound{
		"s1": {{Round: 1, Trigger: "initial", DurationMS: 400, FindingsJSON: &findingsJSON}},
	}
	return steps, rounds
}

func TestBuildTestingSummary_RendersScenarioTableAndVerdict(t *testing.T) {
	t.Parallel()
	findingsJSON := liveValidatedFindingsJSON(t, []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Surface: types.ScenarioSurfaceProduct, Evidence: "checkout.png"},
		{Name: "declined payment shows the retry copy", Result: types.ScenarioResultUntested, Surface: types.ScenarioSurfaceNone, Reason: "no card sandbox credential on this machine"},
	}, types.TestVerdictGo)
	steps, rounds := testStepWithFindings(t, findingsJSON)

	md := BuildTestingSummary(steps, rounds)

	for _, want := range []string{
		"Live validation: ✅ go - 1 of 2 scenarios driven live against the product",
		"| Scenario | Result | Live | Evidence |",
		"| user reaches the success screen | ✅ pass | live | checkout.png |",
		// An untested scenario appears with the capability that stopped it,
		// which is the whole point of reporting it instead of hiding it.
		"| declined payment shows the retry copy | ⏸️ untested | no | no card sandbox credential on this machine |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("expected Testing section to contain %q, got:\n%s", want, md)
		}
	}
}

func TestBuildTestingSummary_NoGoVerdictIsVisible(t *testing.T) {
	t.Parallel()
	findingsJSON := liveValidatedFindingsJSON(t, []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultFail, Live: true, Surface: types.ScenarioSurfaceProduct, Evidence: "checkout.png"},
	}, types.TestVerdictNoGo)
	steps, rounds := testStepWithFindings(t, findingsJSON)

	md := BuildTestingSummary(steps, rounds)
	if !strings.Contains(md, "Live validation: ❌ no-go - 1 of 1 scenarios driven live against the product") {
		t.Errorf("expected the no-go verdict rendered, got:\n%s", md)
	}
	if !strings.Contains(md, "| user reaches the success screen | ❌ fail | live | checkout.png |") {
		t.Errorf("expected the failing scenario row, got:\n%s", md)
	}
}

func TestBuildTestingSummary_NoSurfaceVerdictIsVisible(t *testing.T) {
	t.Parallel()
	findingsJSON := liveValidatedFindingsJSON(t, []types.TestScenario{
		{Name: "Windows git-heavy shard runs the git-backed packages", Result: types.ScenarioResultUntested, Surface: types.ScenarioSurfaceNone, Reason: "CI workflow YAML has no running product no-mistakes can drive"},
	}, types.TestVerdictNoSurface)
	steps, rounds := testStepWithFindings(t, findingsJSON)

	md := BuildTestingSummary(steps, rounds)
	if !strings.Contains(md, "Live validation: ⚠️ no-surface - 0 of 1 scenarios driven live against the product") {
		t.Errorf("expected the no-surface verdict rendered, got:\n%s", md)
	}
	if !strings.Contains(md, "| Windows git-heavy shard runs the git-backed packages | ⏸️ untested | no | CI workflow YAML has no running product no-mistakes can drive |") {
		t.Errorf("expected the untested no-surface row, got:\n%s", md)
	}
}

// A run recorded before the contract existed carries neither scenarios nor a
// verdict. It must render exactly as it always did rather than growing an
// empty table or claiming "no verdict recorded" where there is nothing to say.
func TestBuildTestingSummary_PreContractRunRendersUnchanged(t *testing.T) {
	t.Parallel()
	legacy := `{"findings":[],"summary":"","tested":["` + "`go test ./...`" + `"],"testing_summary":"unit tests passed"}`
	steps, rounds := testStepWithFindings(t, legacy)

	md := BuildTestingSummary(steps, rounds)
	if strings.Contains(md, "Live validation") || strings.Contains(md, "| Scenario |") {
		t.Errorf("pre-contract run must render no live-validation surface, got:\n%s", md)
	}
	if !strings.Contains(md, "unit tests passed") {
		t.Errorf("expected the recorded testing summary, got:\n%s", md)
	}
}

// The Pipeline fold is the per-round story, so it carries the table too.
func TestBuildPipelineSummary_OmitsLiveValidationAfterHeadChanges(t *testing.T) {
	t.Parallel()
	findingsJSON := liveValidatedFindingsJSON(t, []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Surface: types.ScenarioSurfaceProduct, Evidence: "checkout.png"},
	}, types.TestVerdictGo)
	steps, rounds := testStepWithFindings(t, findingsJSON)

	attestation := newPipelineAttestation(steps, rounds, strings.Repeat("ab", 20), pipelineAttestationPolicy{})
	if attestation.LiveValidation != nil {
		t.Fatalf("later head carried stale live validation: %+v", attestation.LiveValidation)
	}
}

func TestBuildPipelineSummary_StepFoldCarriesScenarioTable(t *testing.T) {
	t.Parallel()
	findingsJSON := liveValidatedFindingsJSON(t, []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Surface: types.ScenarioSurfaceProduct, Evidence: "checkout.png"},
	}, types.TestVerdictGo)
	steps, rounds := testStepWithFindings(t, findingsJSON)

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if !strings.Contains(md, "Live validation: ✅ go") {
		t.Errorf("expected the verdict inside the Pipeline fold, got:\n%s", md)
	}
	if !strings.Contains(md, "| user reaches the success screen | ✅ pass | live | checkout.png |") {
		t.Errorf("expected the scenario table inside the Pipeline fold, got:\n%s", md)
	}
}

// A scenario name is agent-authored text landing inside a markdown table, so a
// pipe in it must not invent a column and a newline must not end the row.
func TestBuildTestingSummary_ScenarioCellsCannotBreakTheTable(t *testing.T) {
	t.Parallel()
	findingsJSON := liveValidatedFindingsJSON(t, []types.TestScenario{
		{Name: "user runs `a | b`\nand sees output", Result: types.ScenarioResultPass, Live: true, Surface: types.ScenarioSurfaceProduct, Evidence: "x | y"},
	}, types.TestVerdictGo)
	steps, rounds := testStepWithFindings(t, findingsJSON)

	md := BuildTestingSummary(steps, rounds)
	var row string
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "user runs") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("scenario row missing from:\n%s", md)
	}
	if strings.Count(row, "|")-strings.Count(row, "\\|") != 5 {
		t.Fatalf("scenario row has an unescaped pipe: %q", row)
	}
}

// Bitbucket Cloud PR descriptions carry no HTML; a markdown table is portable,
// so the same contract renders there.
func TestBuildPipelineSummary_ScenarioTableRendersOnBitbucket(t *testing.T) {
	t.Parallel()
	findingsJSON := liveValidatedFindingsJSON(t, []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Surface: types.ScenarioSurfaceProduct, Evidence: "checkout.png"},
	}, types.TestVerdictGo)
	steps, rounds := testStepWithFindings(t, findingsJSON)

	md, _ := BuildPipelineSummaryFor(steps, rounds, testPipelineHeadSHA, scm.ProviderBitbucket)
	if strings.Contains(md, "<details>") {
		t.Fatalf("bitbucket body must carry no HTML, got:\n%s", md)
	}
	if !strings.Contains(md, "| user reaches the success screen | ✅ pass | live | checkout.png |") {
		t.Errorf("expected the scenario table on bitbucket, got:\n%s", md)
	}
}

// Synthetic reproduction of the reported fake-gh evidence shape. The non-empty
// evidence block must not let a substituted upstream count as product validation.
func simulatedGHFindingsJSON(t *testing.T) string {
	t.Helper()
	scenarios := make([]map[string]any, 0, 10)
	for i := 0; i < 9; i++ {
		scenarios = append(scenarios, map[string]any{
			"name":   fmt.Sprintf("fake-gh merge/CLI scenario %d", i+1),
			"result": "pass", "live": true, "surface": "simulated",
			"evidence": "built CLI with offline fake gh fixture; captured output", "reason": "",
		})
	}
	scenarios = append(scenarios, map[string]any{
		"name": "upstream base-ref read", "result": "pass", "live": true,
		"surface": "product", "evidence": "gh api repos/owner/repo/git/ref/heads/main", "reason": "",
	})
	raw, err := json.Marshal(map[string]any{
		"findings": []any{}, "summary": "", "tested": []string{"synthetic fake-gh reproduction"},
		"testing_summary": "nine substituted scenarios and one API read",
		"artifacts":       []any{}, "scenarios": scenarios, "verdict": "go", "tested_head_sha": testPipelineHeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBuildPipelineSummary_SimulatedGHScenariosDoNotAttestGo(t *testing.T) {
	t.Parallel()
	steps, rounds := testStepWithFindings(t, simulatedGHFindingsJSON(t))
	attestation := newPipelineAttestation(steps, rounds, testPipelineHeadSHA, pipelineAttestationPolicy{})
	if got := attestation.LiveValidation; got == nil || got.Verdict != types.TestVerdictInconclusive || got.Live != 1 || got.Total != 10 {
		t.Fatalf("synthetic fake-gh attestation = %+v, want inconclusive, 1 of 10", got)
	}
	for _, md := range []string{BuildTestingSummary(steps, rounds), func() string { body, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA); return body }()} {
		if !strings.Contains(md, "Live validation: ⚠️ inconclusive - 1 of 10 scenarios driven live against the product") {
			t.Fatalf("synthetic fake-gh summary has incorrect verdict/count:\n%s", md)
		}
		if strings.Contains(md, "| fake-gh merge/CLI scenario 1 | ✅ pass | live |") {
			t.Fatalf("substituted scenario rendered live:\n%s", md)
		}
	}
}
