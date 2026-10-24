package pipeline

import (
	"context"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_FinalFullTestRequestDoesNotLeakIntoRepairsOrRevalidation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	var requests []bool
	test := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		requests = append(requests, sctx.ForceFullTest)
		sctx.ForceFullTest = false
		if len(requests) == 2 {
			return &StepOutcome{NeedsApproval: true, AutoFixable: true,
				Findings: `{"findings":[{"severity":"error","description":"final full failed","action":"auto-fix"}]}`}, nil
		}
		if len(requests) == 3 && !sctx.Fixing {
			t.Fatal("failure must dispatch a Test repair")
		}
		return &StepOutcome{}, nil
	}}
	pushCalls := 0
	push := &adaptiveCallStep{name: types.StepPush, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if sctx.ForceFullTest {
			t.Fatal("Test request leaked to Push")
		}
		pushCalls++
		if pushCalls <= 2 {
			return &StepOutcome{RestartFrom: types.StepTest, RequireFinalFullTest: true}, nil
		}
		return &StepOutcome{}, nil
	}}
	ciCalls := 0
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
		ciCalls++
		if ciCalls == 1 {
			return &StepOutcome{RestartFrom: types.StepReview}, nil
		}
		return &StepOutcome{}, nil
	}}
	executor := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Test: 1}}, nil,
		[]Step{newPassStep(types.StepReview), test, push, ci}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if want := []bool{false, true, false, true, false}; !reflect.DeepEqual(requests, want) {
		t.Fatalf("final-full requests = %v, want %v", requests, want)
	}
}
