package refinery

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

// Explicit opt-in: this test may merge one PR in a dedicated acceptance repo.
// Normal test runs do not read credentials or contact a Forgejo server.
func TestForgejoLiveAcceptance(t *testing.T) {
	file := os.Getenv("GT_FORGEJO_ACCEPTANCE_CONFIG")
	if file == "" {
		t.Skip("isolated live repository acceptance is opt-in")
	}
	var input struct {
		Config  config.ForgejoConfig `json:"config"`
		WorkDir string               `json:"work_dir"`
		PR      int                  `json:"pr"`
		Branch  string               `json:"branch"`
		Head    string               `json:"head"`
		Expect  string               `json:"expect"`
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	p, err := newForgejoPRProvider(git.NewGit(input.WorkDir), &input.Config)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(p.repo, "/")
	if len(parts) != 2 || !strings.HasPrefix(parts[1], "gt67j-acceptance-") {
		t.Fatal("live test requires a dedicated gt67j-acceptance repository")
	}
	if input.Expect != "refuse" && input.Expect != "merge" {
		t.Fatal("explicit expected outcome required")
	}
	pr, err := p.FindPullRequest(input.Branch, "", input.PR, input.Head)
	if err == nil {
		_, err = p.MergePR(pr, "squash")
	}
	if input.Expect == "refuse" {
		if err == nil {
			t.Fatal("unsafe candidate unexpectedly merged")
		}
		state, readErr := p.readPR(input.PR)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if *state.Merged {
			t.Fatal("PR merged despite refusal")
		}
		t.Log("live refusal confirmed; PR remains unmerged")
	} else if err != nil {
		t.Fatal(err)
	} else {
		t.Log("live exact-head squash and canonical reachability confirmed")
	}
}
