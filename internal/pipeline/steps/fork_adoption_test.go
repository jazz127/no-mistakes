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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise first attachment to an existing fork PR through both production
// publication steps. Git uses local repositories; GitHub is a synthetic CLI.
func TestForkRouting_UpdatesExistingParentPRAndAttestation(t *testing.T) {
	const (
		parentURL = "https://github.com/parent-owner/repo.git"
		forkURL   = "https://github.com/fork-owner/repo.git"
		prURL     = "https://github.com/parent-owner/repo/pull/42"
	)
	parent, fork := t.TempDir(), t.TempDir()
	gitCmd(t, parent, "init", "--bare")
	gitCmd(t, fork, "init", "--bare")
	dir, baseSHA, oldHead := setupGitRepo(t)
	// Keep the public routing identities while making every Git operation
	// hermetic. No transport contacts GitHub.
	gitCmd(t, dir, "config", "url."+parent+".insteadOf", parentURL)
	gitCmd(t, dir, "config", "url."+fork+".insteadOf", forkURL)
	gitCmd(t, dir, "remote", "add", "origin", parentURL)
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "push", forkURL, "feature")
	if err := os.WriteFile(filepath.Join(dir, "new-work.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "new-work.txt")
	gitCmd(t, dir, "commit", "-m", "fix: update existing fork PR")
	newHead := gitCmd(t, dir, "rev-parse", "HEAD")

	env, logFile := fakeGH(t, prURL)
	// A same-named branch from another fork must not steal the update.
	env = append(env, `FAKE_CLI_PR_LIST_JSON=[{"number":41,"url":"https://github.com/parent-owner/repo/pull/41","baseRefName":"main","headRefName":"feature","headRepositoryOwner":{"login":"another-owner"}},{"number":42,"url":"https://github.com/parent-owner/repo/pull/42","baseRefName":"main","headRefName":"feature","headRepositoryOwner":{"login":"fork-owner"}}]`)
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	oldBody := compliantPipelineBody(t, oldHead)
	if err := os.WriteFile(bodyFile, []byte(oldBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, output := runVerifyPy(t, oldBody, newHead); got != "failure" || !strings.Contains(output, "does not match") {
		t.Fatalf("stale attestation at new head = %s, want head mismatch: %s", got, output)
	}

	ag := &mockAgent{name: "test", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"fix: update fork PR","body":"## What Changed\n\nUpdated the feature."}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, newHead, config.Commands{})
	sctx.Env = env
	sctx.Repo.UpstreamURL, sctx.Repo.ForkURL = parentURL, forkURL
	if sctx.Run.PRURL != nil {
		t.Fatal("test must discover the existing PR without an inherited URL")
	}
	setupGateMirror(t, sctx)
	recordReviewApproval(t, sctx, newHead)
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, fork, "rev-parse", "refs/heads/feature"); got != newHead {
		t.Fatalf("fork head = %s, want %s", got, newHead)
	}
	if got := gitCmd(t, parent, "for-each-ref", "--format=%(refname)"); got != "refs/heads/main" {
		t.Fatalf("parent refs changed: %s", got)
	}
	assertCurrentAttestation := func(stage string) {
		t.Helper()
		body, err := os.ReadFile(bodyFile)
		if err != nil {
			t.Fatal(err)
		}
		if got := parsePipelineAttestationForTest(t, string(body)).HeadSHA; got != newHead {
			t.Fatalf("%s attested head = %s, want %s", stage, got, newHead)
		}
		if got, output := runVerifyPy(t, string(body), newHead); got != "success" {
			t.Fatalf("%s verifier = %s: %s", stage, got, output)
		}
	}
	assertCurrentAttestation("push")
	result, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped || result.PRURL != prURL {
		t.Fatalf("PR outcome = %#v, want existing URL %s", result, prURL)
	}
	assertCurrentAttestation("PR update")
	if body := string(mustRead(t, bodyFile)); !strings.Contains(body, "Updated the feature.") {
		t.Fatalf("PR description was not refreshed: %s", body)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != prURL || run.LastPushedSHA == nil || *run.LastPushedSHA != newHead || run.PushTargetKind == nil || *run.PushTargetKind != "fork" {
		t.Fatalf("published run binding = %#v", run)
	}
	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	transcript := string(logData)
	if !strings.Contains(transcript, "pr list --head feature --repo parent-owner/repo --state open") {
		t.Fatalf("missing parent lookup by bare branch:\n%s", transcript)
	}
	if !strings.Contains(transcript, "--method PATCH repos/parent-owner/repo/pulls/42") {
		t.Fatalf("missing update of the existing parent PR:\n%s", transcript)
	}
	for _, forbidden := range []string{"pr create", "--repo fork-owner/repo", "pulls/41", "pr list --head fork-owner:feature"} {
		if strings.Contains(transcript, forbidden) {
			t.Fatalf("unexpected %q in publication transcript:\n%s", forbidden, transcript)
		}
	}
	t.Log("synthetic fork publication: stale head rejected; fork advanced; existing parent PR reused; current-head attestation accepted after Push and PR")
}
