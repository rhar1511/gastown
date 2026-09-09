package refinery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

type forgejoSubmitTestStore struct {
	issues map[string]*beads.Issue
}

func (s *forgejoSubmitTestStore) Show(id string) (*beads.Issue, error) { return s.issues[id], nil }
func (s *forgejoSubmitTestStore) ListMergeRequests(beads.ListOptions) ([]*beads.Issue, error) {
	var result []*beads.Issue
	for _, issue := range s.issues {
		if beads.HasLabel(issue, "gt:merge-request") && issue.Status == "open" {
			result = append(result, issue)
		}
	}
	return result, nil
}
func (s *forgejoSubmitTestStore) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	issue := &beads.Issue{ID: "test-mr-created", Title: opts.Title, Description: opts.Description, Status: "open", Priority: opts.Priority, Labels: opts.Labels, Ephemeral: opts.Ephemeral}
	s.issues[issue.ID] = issue
	return issue, nil
}

func submitTestIssue(id, description string, labels ...string) *beads.Issue {
	return &beads.Issue{ID: id, Title: id, Description: description, Status: "open", Priority: 2, Labels: labels}
}

func newForgejoSubmitEngineer(t *testing.T, source *beads.Issue, existing ...*beads.Issue) (*Engineer, *forgejoSubmitTestStore, *forgejoFixture) {
	t.Helper()
	f := newForgejoFixture(t)
	store := &forgejoSubmitTestStore{issues: map[string]*beads.Issue{source.ID: source}}
	for _, issue := range existing {
		store.issues[issue.ID] = issue
	}
	e := newTestEngineer(t, t.TempDir(), newTestGit(t, t.TempDir()))
	e.rig.Name = "test-rig"
	e.config.MergeStrategy = "pr"
	e.config.VCSProvider = "forgejo"
	e.config.Forgejo = &f.p.cfg
	e.prProvider = f.p
	f.p.lockPath = filepath.Join(t.TempDir(), "fresh-runtime", "forgejo-provider.lock")
	return e, store, f
}

func TestSubmitForgejoPRRegistersCanonicalExactHead(t *testing.T) {
	source := submitTestIssue("gt-source", "admitted source")
	e, store, f := newForgejoSubmitEngineer(t, source)

	mr, err := e.submitForgejoPR(store, source.ID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if mr.SourceIssue != source.ID || mr.Branch != f.pr.Head.Ref || mr.Target != f.pr.Base.Ref ||
		mr.CommitSHA != forgejoTestHead || mr.PRNumber != 42 || mr.PRURL != f.server.URL+"/team/project/pulls/42" {
		t.Fatalf("wrong registration: %+v", mr)
	}
	if f.posts != 0 || source.Status != "open" || source.Assignee != "" {
		t.Fatal("registration mutated PR or source work")
	}
	registered := store.issues[mr.ID]
	if registered == nil || !registered.Ephemeral || !hasSubmitTestLabel(registered.Labels, "gt:merge-request") {
		t.Fatalf("native MR was not created as an ephemeral merge request: %+v", registered)
	}
	if _, err := os.Stat(filepath.Dir(f.p.lockPath)); err != nil {
		t.Fatalf("fresh runtime directory was not created: %v", err)
	}
}

func hasSubmitTestLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func TestSubmitForgejoPRIsIdempotentButNeverSupersedes(t *testing.T) {
	source := submitTestIssue("gt-source", "admitted source")
	exact := submitTestIssue("gt-existing", "", "gt:merge-request")
	exact.Description = beads.FormatMRFields(&beads.MRFields{Branch: "feat/recovery", Target: "main", SourceIssue: source.ID, Rig: "test-rig", CommitSHA: forgejoTestHead, PRURL: "placeholder", PRNumber: 42})
	e, store, f := newForgejoSubmitEngineer(t, source, exact)
	exact.Description = strings.Replace(exact.Description, "pr_url: placeholder", "pr_url: "+f.server.URL+"/team/project/pulls/42", 1)

	mr, err := e.submitForgejoPR(store, source.ID, 42)
	if err != nil || mr.ID != exact.ID || len(store.issues) != 2 {
		t.Fatalf("exact retry was not idempotent: mr=%+v err=%v issues=%d", mr, err, len(store.issues))
	}

	exact.Description = beads.FormatMRFields(&beads.MRFields{Branch: "other", Target: "main", SourceIssue: source.ID, Rig: "test-rig", CommitSHA: strings.Repeat("d", 40), PRNumber: 99})
	if _, err := e.submitForgejoPR(store, source.ID, 42); err == nil || exact.Status != "open" {
		t.Fatalf("conflicting MR was superseded or accepted: %v", err)
	}
}

func TestSubmitForgejoPRRefusesSourceAndPRHoldsWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*beads.Issue, *forgejoFixture)
	}{
		{name: "source hold", mutate: func(s *beads.Issue, _ *forgejoFixture) { s.Labels = []string{"hold:mayor"} }},
		{name: "source dependency", mutate: func(s *beads.Issue, _ *forgejoFixture) { s.BlockedByCount = 1 }},
		{name: "source opt out", mutate: func(s *beads.Issue, _ *forgejoFixture) { s.Description = "no_merge: true" }},
		{name: "manual PR", mutate: func(_ *beads.Issue, f *forgejoFixture) { f.pr.Body = "manual-only" }},
		{name: "foreign target", mutate: func(_ *beads.Issue, f *forgejoFixture) { f.pr.Base.Ref = "release" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := submitTestIssue("gt-source", "admitted source")
			e, store, f := newForgejoSubmitEngineer(t, source)
			tc.mutate(source, f)
			if _, err := e.submitForgejoPR(store, source.ID, 42); err == nil || len(store.issues) != 1 || f.posts != 0 {
				t.Fatalf("unsafe registration accepted or mutated state: %v", err)
			}
		})
	}
}
