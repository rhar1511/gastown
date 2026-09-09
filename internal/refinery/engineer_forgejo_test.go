package refinery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
)

func TestForgejoEngineerUsesProtectedRouteWithoutLocalBranch(t *testing.T) {
	f := newForgejoFixture(t)
	dir := t.TempDir()
	run(t, dir, "git", "init")
	e := newTestEngineer(t, dir, git.NewGit(dir))
	e.config.MergeStrategy = "pr"
	e.config.VCSProvider = "forgejo"
	e.config.Forgejo = &f.p.cfg
	e.prProvider = f.p
	mr := &MRInfo{ID: "mr-forgejo", Branch: "feat/recovery", Target: "main", CommitSHA: forgejoTestHead, PRNumber: 42}
	result := e.ProcessMRInfo(context.Background(), mr)
	if !result.Success || result.MergeCommit != forgejoTestMerge || f.posts != 1 {
		t.Fatalf("protected route failed: %+v posts=%d", result, f.posts)
	}
	// Squash proof uses authoritative merge receipt, not source ancestry/local HEAD.
	if err := e.verifyMRInfoPostMergeProof(mr, result.MergeCommit); err != nil {
		t.Fatal(err)
	}
	if err := e.verifyMRInfoPostMergeProof(mr, strings.Repeat("d", 40)); err == nil {
		t.Fatal("different merge receipt accepted")
	}
}
func TestForgejoEngineerRefusesWrongTargetAndMissingHead(t *testing.T) {
	for _, scenario := range []string{"target", "head"} {
		t.Run(scenario, func(t *testing.T) {
			f := newForgejoFixture(t)
			dir := t.TempDir()
			run(t, dir, "git", "init")
			e := newTestEngineer(t, dir, git.NewGit(dir))
			e.config.MergeStrategy = "pr"
			e.config.VCSProvider = "forgejo"
			e.config.Forgejo = &f.p.cfg
			e.prProvider = f.p
			mr := &MRInfo{ID: "mr-forgejo", Branch: "feat/recovery", Target: "main", CommitSHA: forgejoTestHead, PRNumber: 42}
			if scenario == "target" {
				mr.Target = "release"
			} else {
				mr.CommitSHA = ""
			}
			if r := e.doMergePR(context.Background(), mr); r.Success || f.posts != 0 {
				t.Fatalf("unsafe MR accepted: %+v", r)
			}
		})
	}
}
func TestForgejoEngineerConfig(t *testing.T) {
	for _, scenario := range []string{"valid", "direct", "parallel", "local gates"} {
		t.Run(scenario, func(t *testing.T) {
			f := newForgejoFixture(t)
			dir := t.TempDir()
			run(t, dir, "git", "init")
			run(t, dir, "git", "remote", "add", "forgejo", f.server.URL+"/team/project.git")
			e := newTestEngineer(t, dir, git.NewGit(dir))
			queue := map[string]any{"merge_strategy": "pr", "vcs_provider": "forgejo", "max_concurrent": 1, "forgejo": f.p.cfg}
			switch scenario {
			case "direct":
				queue["merge_strategy"] = "direct"
			case "parallel":
				queue["max_concurrent"] = 2
			case "local gates":
				queue["gates"] = map[string]any{"test": map[string]string{"cmd": "test-command"}}
			}
			b, _ := json.Marshal(map[string]any{"merge_queue": queue})
			if err := os.WriteFile(filepath.Join(e.rig.Path, "config.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			err := e.LoadConfig()
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("scenario=%s err=%v", scenario, err)
			}
		})
	}
}
func TestForgejoLandingDisabledAndSerialized(t *testing.T) {
	f := newForgejoFixture(t)
	dir := t.TempDir()
	run(t, dir, "git", "init")
	e := newTestEngineer(t, dir, git.NewGit(dir))
	e.config.Forgejo = &f.p.cfg
	e.config.Forgejo.LandingEnabled = false
	if _, err := e.LandForgejoMR(context.Background(), "mr", "worker"); err == nil {
		t.Fatal("unactivated landing allowed")
	}
	e.config.Forgejo.LandingEnabled = true
	runtimeDir := filepath.Join(e.rig.Path, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(runtimeDir, "forgejo-landing.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if _, err := e.LandForgejoMR(context.Background(), "mr", "worker"); err == nil || !strings.Contains(err.Error(), "another Forgejo landing") {
		t.Fatalf("lock not enforced: %v", err)
	}
	if f.posts != 0 {
		t.Fatal("lock refusal still mutated")
	}
}

func TestForgejoPreflightPreservesBeadsAndClaim(t *testing.T) {
	for _, scenario := range []string{"ready", "source opt out", "wrong claim", "head race", "source hold", "source blocked"} {
		t.Run(scenario, func(t *testing.T) {
			f := newForgejoFixture(t)
			dir := t.TempDir()
			run(t, dir, "git", "init")
			issue := prepushMRIssue("gt-mr", "feat/recovery", "main", "gt-source", forgejoTestHead)
			issue.Description = beads.FormatMRFields(&beads.MRFields{Branch: "feat/recovery", Target: "main", SourceIssue: "gt-source", Rig: "test-rig", CommitSHA: forgejoTestHead, PRNumber: 42})
			issue.Assignee = "refinery-1"
			source := prepushIssue("gt-source", "Implement recovery contract")
			if scenario == "source opt out" {
				source.Description = "no_merge: true\nImplement recovery contract"
			}
			if scenario == "source hold" {
				source.Labels = []string{"hold:mayor"}
			}
			if scenario == "source blocked" {
				source.Status = "blocked"
			}
			if scenario == "wrong claim" {
				issue.Assignee = "other-worker"
			}
			if scenario == "head race" {
				f.changeAfterChecks = true
			}
			store := newPrepushStore(issue, source)
			e := newPrepushEngineer(t, dir, store)
			e.config.MergeStrategy = "pr"
			e.config.VCSProvider = "forgejo"
			e.config.Forgejo = &f.p.cfg
			e.prProvider = f.p
			if scenario == "wrong claim" {
				if _, err := e.LandForgejoMR(context.Background(), issue.ID, "refinery-1"); err == nil {
					t.Fatal("foreign claim accepted")
				}
			} else {
				_, err := e.CheckForgejoMR(issue.ID)
				if (err == nil) != (scenario == "ready") {
					t.Fatalf("scenario=%s error=%v", scenario, err)
				}
			}
			if f.posts != 0 || len(store.closeReasons) != 0 || string(issue.Status) != "open" || (scenario != "source blocked" && string(source.Status) != "open") {
				t.Fatal("preflight/refusal mutated state")
			}
		})
	}
}
func TestForgejoProviderLockCoversOtherEntryPoints(t *testing.T) {
	f := newForgejoFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.p.lockPath), 0700); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(f.p.lockPath)
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if _, err := f.p.MergePR(f.info(), "squash"); err == nil || f.posts != 0 {
		t.Fatal("provider bypassed shared lock")
	}
}
