package steps

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

// Cached fixtures must not start automatic maintenance that can mutate their
// object directories during copying. Git's Trace2 child_start events are the
// subprocess contract: even an auto-maintenance invocation with no work to do
// violates the fixture's isolation from background writers.
func TestGitRepoTemplates_DoNotStartAutomaticMaintenance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T) (string, string, string)
	}{
		{"steps", setupGitRepo},
		{"citest", stepstest.SetupGitRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _, head := tc.setup(t)
			tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
			cmd := exec.Command("git", "commit", "--allow-empty", "-m", "fixture commit")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GIT_TRACE2_EVENT="+tracePath)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("commit: %v: %s", err, out)
			}
			if got := gitCmd(t, dir, "rev-parse", "HEAD^"); got != head {
				t.Fatalf("commit parent = %s, want fixture head %s", got, head)
			}
			trace, err := os.Open(tracePath)
			if err != nil {
				t.Fatal(err)
			}
			defer trace.Close()
			decoder := json.NewDecoder(trace)
			for {
				var event struct {
					Event string   `json:"event"`
					Argv  []string `json:"argv"`
				}
				if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if event.Event == "child_start" && len(event.Argv) >= 2 &&
					(event.Argv[1] == "maintenance" || event.Argv[1] == "gc") {
					t.Errorf("fixture commit started automatic maintenance: %v", event.Argv)
				}
			}
		})
	}
}
