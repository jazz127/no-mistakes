package types

import "testing"

func TestLiveScenarioCounts_RequiresProductSurface(t *testing.T) {
	t.Parallel()
	scenarios := []TestScenario{
		{Name: "read upstream", Result: ScenarioResultPass, Live: true, Surface: ScenarioSurfaceProduct, Evidence: "captured API response"},
		{Name: "failed product operation", Result: ScenarioResultFail, Live: true, Surface: ScenarioSurfaceProduct, Evidence: "captured failure"},
		{Name: "fake gh", Result: ScenarioResultPass, Live: true, Surface: ScenarioSurfaceSimulated, Evidence: "fake output"},
		{Name: "legacy claim", Result: ScenarioResultPass, Live: true, Evidence: "output"},
		{Name: "unknown provenance", Result: ScenarioResultPass, Live: true, Surface: "other", Evidence: "output"},
		{Name: "missing evidence", Result: ScenarioResultPass, Live: true, Surface: ScenarioSurfaceProduct},
		{Name: "never executed", Result: ScenarioResultUntested, Live: true, Surface: ScenarioSurfaceProduct, Evidence: "inspection"},
	}
	if live, total := LiveScenarioCounts(scenarios); live != 2 || total != 7 {
		t.Fatalf("counts = %d/%d, want 2/7", live, total)
	}
}

func TestParseFindingsJSON_SurfaceProvenanceSurvivesRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, surface, wantVerdict string
		wantLive                   bool
	}{
		{"actual service", ScenarioSurfaceProduct, TestVerdictGo, true},
		{"substituted service", ScenarioSurfaceSimulated, TestVerdictInconclusive, false},
		{"no execution", ScenarioSurfaceNone, TestVerdictInconclusive, false},
		{"legacy missing provenance", "", TestVerdictInconclusive, false},
		{"unknown provenance", "other", TestVerdictInconclusive, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := MarshalFindingsJSON(Findings{
				Scenarios: []TestScenario{{Name: "operation", Result: ScenarioResultPass, Live: true, Surface: tc.surface, Evidence: "captured output"}},
				Verdict:   TestVerdictGo,
			})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseFindingsJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Verdict != tc.wantVerdict || parsed.Scenarios[0].Live != tc.wantLive || parsed.Scenarios[0].Surface != tc.surface {
				t.Fatalf("parsed = %+v, want verdict %s live %t surface %q", parsed, tc.wantVerdict, tc.wantLive, tc.surface)
			}
			rewritten, err := MarshalFindingsJSON(FindingsMetadata(parsed))
			if err != nil {
				t.Fatal(err)
			}
			again, err := ParseFindingsJSON(rewritten)
			if err != nil {
				t.Fatal(err)
			}
			if again.Verdict != parsed.Verdict || again.Scenarios[0] != parsed.Scenarios[0] {
				t.Fatalf("rewrite changed provenance: %+v", again)
			}
		})
	}
}

func TestLiveValidationVerdict_PreservesNoGoAndOrdinaryUntested(t *testing.T) {
	t.Parallel()
	simulated := []TestScenario{{Result: ScenarioResultUntested, Surface: ScenarioSurfaceSimulated}}
	if got := LiveValidationVerdict(simulated, TestVerdictNoGo); got != TestVerdictNoGo {
		t.Fatalf("no-go = %q", got)
	}
	ordinary := []TestScenario{{Result: ScenarioResultUntested, Surface: ScenarioSurfaceNone, Reason: "unavailable optional credential"}}
	if got := LiveValidationVerdict(ordinary, TestVerdictGo); got != TestVerdictGo {
		t.Fatalf("ordinary untested = %q", got)
	}
}
