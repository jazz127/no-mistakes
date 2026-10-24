package steps

import (
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func runRepositoryCommand(sctx *pipeline.StepContext, name, command string) (string, int, error) {
	declareCommandOverrides(sctx, name)
	return executeRepositoryCommand(sctx, name, command)
}

func declareCommandOverrides(sctx *pipeline.StepContext, name string) {
	if declaration := commandOverrideDeclaration(name, sctx.Config.CommandOverrides[name]); declaration != "" {
		sctx.Log(declaration)
	}
}

// declareStepCommandOverrides declares a Test or Lint step's overrides once per
// step: a fix round re-executes the step into the same step log.
func declareStepCommandOverrides(sctx *pipeline.StepContext, name string) {
	if !sctx.Fixing {
		declareCommandOverrides(sctx, name)
	}
}

func executeRepositoryCommand(sctx *pipeline.StepContext, name, command string) (string, int, error) {
	return runShellCommandWithPriority(sctx.Ctx, sctx.WorkDir, stepEnvironment(sctx), command, sctx.Config.CommandOverrides[name].Nice)
}

func executeRepositoryTestCommand(sctx *pipeline.StepContext, command, mode, since string) (string, int, error) {
	env := testCommandEnvironment(sctx, mode, since)
	return runShellCommandWithPriority(sctx.Ctx, sctx.WorkDir, env, command, sctx.Config.CommandOverrides["test"].Nice)
}

// commandOverrideDeclaration states every machine-local override applied to a
// command so a result produced under one is never presented as a plain run.
func commandOverrideDeclaration(name string, override config.CommandOverride) string {
	var parts []string
	if override.Nice != 0 {
		parts = append(parts, fmt.Sprintf("nice %d", override.Nice))
	}
	if len(override.Additional) != 0 {
		checks := make([]string, len(override.Additional))
		for i, command := range override.Additional {
			checks[i] = fmt.Sprintf("%q", command)
		}
		parts = append(parts, "additional checks "+strings.Join(checks, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("machine-local overrides applied to commands.%s: %s", name, strings.Join(parts, "; "))
}

// checkResult is one configured check's own outcome, so a machine-local
// check's failure is never attributed to the repository's command.
type checkResult struct {
	Command  string
	Local    bool
	ExitCode int
	Output   string
}

func (r checkResult) description(name string) string {
	if r.Local {
		return fmt.Sprintf("machine-local %s check failed with exit code %d: %s", name, r.ExitCode, r.Command)
	}
	return fmt.Sprintf("configured %s command failed with exit code %d", name, r.ExitCode)
}

func failedChecks(results []checkResult) []checkResult {
	var failed []checkResult
	for _, result := range results {
		if result.ExitCode != 0 {
			failed = append(failed, result)
		}
	}
	return failed
}

// runConfiguredChecks runs the repository command (when nonempty) followed by
// the operator's machine-local additional checks for name.
func runConfiguredChecks(sctx *pipeline.StepContext, name, command string) (string, []checkResult, error) {
	return runConfiguredChecksWithTestMode(sctx, name, command, "", "")
}

func runConfiguredChecksForTest(sctx *pipeline.StepContext, command, mode, since string) (string, []checkResult, error) {
	return runConfiguredChecksWithTestMode(sctx, "test", command, mode, since)
}

func testCommandEnvironment(sctx *pipeline.StepContext, mode, since string) []string {
	env := stepEnvironment(sctx)
	if env == nil {
		env = os.Environ()
	}
	filtered := make([]string, 0, len(env)+2)
	for _, entry := range env {
		key := envKey(entry)
		if key == "NO_MISTAKES_TEST_MODE" || key == "NO_MISTAKES_TEST_SINCE" {
			continue
		}
		filtered = append(filtered, entry)
	}
	filtered = append(filtered, "NO_MISTAKES_TEST_MODE="+mode)
	if mode == "delta" && since != "" {
		filtered = append(filtered, "NO_MISTAKES_TEST_SINCE="+since)
	}
	return filtered
}

func runConfiguredChecksWithTestMode(sctx *pipeline.StepContext, name, command, mode, since string) (string, []checkResult, error) {
	override := sctx.Config.CommandOverrides[name]
	checks := []checkResult{}
	if command != "" {
		checks = append(checks, checkResult{Command: command})
	}
	for _, additional := range override.Additional {
		checks = append(checks, checkResult{Command: additional, Local: true})
	}
	var output strings.Builder
	for i := range checks {
		if err := sctx.Ctx.Err(); err != nil {
			return output.String(), checks[:i], err
		}
		if checks[i].Local {
			sctx.Log(fmt.Sprintf("running machine-local %s check: %s", name, checks[i].Command))
			fmt.Fprintf(&output, "\nmachine-local %s check: %s\n", name, checks[i].Command)
		} else if len(override.Additional) > 0 {
			fmt.Fprintf(&output, "\nconfigured %s command: %s\n", name, checks[i].Command)
		}
		var out string
		var code int
		var err error
		if name == "test" && !checks[i].Local && mode != "" {
			out, code, err = executeRepositoryTestCommand(sctx, checks[i].Command, mode, since)
		} else {
			out, code, err = executeRepositoryCommand(sctx, name, checks[i].Command)
		}
		output.WriteString(out)
		if err != nil {
			return output.String(), checks[:i], err
		}
		checks[i].ExitCode = code
		checks[i].Output = out
		if len(override.Additional) > 0 {
			fmt.Fprintf(&output, "\nexit code: %d\n", code)
		}
	}
	return output.String(), checks, nil
}
