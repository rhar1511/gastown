package refinery

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

const forgejoTestHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const forgejoTestMerge = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type forgejoFixture struct {
	p                 *forgejoPRProvider
	server            *httptest.Server
	pr                forgejoPR
	protection        forgejoProtection
	statuses          []forgejoStatus
	reviews           []forgejoReview
	posts             int
	reads             int
	merged            bool
	changeAfterChecks bool
	postCode          int
	badReceipt        bool
	freeze            bool
	failPath          string
	verified          int
	freezeCredential  string
}

func boolPointer(v bool) *bool { return &v }
func newForgejoFixture(t *testing.T) *forgejoFixture {
	t.Helper()
	f := &forgejoFixture{postCode: 200, statuses: []forgejoStatus{{ID: 1, Context: "CI / Gate (pull_request)", Status: "success"}, {ID: 2, Context: "Guarded Lane", Status: "success"}, {ID: 3, Context: "Queue Open", Status: "success"}}, reviews: []forgejoReview{}}
	f.pr = forgejoPR{Number: 42, State: "open", Draft: boolPointer(false), Merged: boolPointer(false), Mergeable: boolPointer(true), Head: forgejoRef{Ref: "feat/recovery", SHA: forgejoTestHead}, Base: forgejoRef{Ref: "main", SHA: strings.Repeat("c", 40)}}
	f.pr.Head.Repo.FullName = "team/project"
	f.pr.Base.Repo.FullName = "team/project"
	f.protection = forgejoProtection{ApplyToAdmins: boolPointer(true), EnableStatusCheck: boolPointer(true), StatusCheckContexts: []string{"CI / Gate*", "Guarded Lane", "Queue Open"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		wantAuth := "token fixture-secret"
		if f.freezeCredential != "" && r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/team/project/actions/variables/QUEUE_FREEZE" {
			wantAuth = "token " + f.freezeCredential
		}
		if r.Header.Get("Authorization") != wantAuth {
			t.Errorf("missing configured credential")
			w.WriteHeader(401)
			return
		}
		if f.failPath != "" && strings.Contains(r.URL.Path, f.failPath) {
			w.WriteHeader(403)
			fmt.Fprint(w, "fixture-secret must never appear in errors")
			return
		}
		switch r.URL.Path {
		case "/api/v1/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "refinery-controller"})
		case "/api/v1/repos/team/project/actions/variables/QUEUE_FREEZE":
			if !f.freeze {
				w.WriteHeader(404)
			} else {
				json.NewEncoder(w).Encode(map[string]string{"data": "true"})
			}
		case "/api/v1/repos/team/project/branches/main":
			json.NewEncoder(w).Encode(map[string]any{"name": "main", "protected": true, "effective_branch_protection_name": "main", "user_can_merge": true})
		case "/api/v1/repos/team/project/branch_protections/main":
			json.NewEncoder(w).Encode(f.protection)
		case "/api/v1/repos/team/project/statuses/" + forgejoTestHead:
			json.NewEncoder(w).Encode(f.statuses)
		case "/api/v1/repos/team/project/pulls/42/reviews":
			json.NewEncoder(w).Encode(f.reviews)
		case "/api/v1/repos/team/project/pulls/42":
			f.reads++
			pr := f.pr
			if f.changeAfterChecks && f.reads >= 2 {
				pr.Head.SHA = strings.Repeat("d", 40)
			}
			if f.merged {
				pr.State = "closed"
				pr.Merged = boolPointer(true)
				pr.MergeCommitSHA = forgejoTestMerge
			}
			if f.badReceipt && f.merged {
				pr.MergeCommitSHA = ""
			}
			json.NewEncoder(w).Encode(pr)
		case "/api/v1/repos/team/project/pulls/42/merge":
			if r.Method != http.MethodPost {
				t.Errorf("unsafe merge inspection method %s", r.Method)
				w.WriteHeader(405)
				return
			}
			f.posts++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["Do"] != "squash" || body["head_commit_id"] != forgejoTestHead || body["force_merge"] != false || body["merge_when_checks_succeed"] != false || body["delete_branch_after_merge"] != false {
				t.Errorf("unsafe payload: %v", body)
			}
			if f.postCode == 200 {
				f.merged = true
			}
			w.WriteHeader(f.postCode)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "remote", "add", "forgejo", f.server.URL+"/team/project.git")
	// A distinct origin must never be used by the provider.
	run(t, dir, "git", "remote", "add", "origin", "https://github.invalid/other/project.git")
	t.Setenv("GT_FORGEJO_TEST_TOKEN", "fixture-secret")
	cfg := &config.ForgejoConfig{Remote: "forgejo", BaseURL: f.server.URL, TargetBranch: "main", TokenEnv: "GT_FORGEJO_TEST_TOKEN", ControllerUser: "refinery-controller", AllowHTTP: true, ServerSideGates: true, LandingEnabled: true}
	var err error
	f.p, err = newForgejoPRProvider(git.NewGit(dir), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.p.verifyCommit = func(target, sha string) error {
		f.verified++
		if target != "main" || sha != forgejoTestMerge {
			return fmt.Errorf("wrong merge proof %s %s", target, sha)
		}
		return nil
	}
	return f
}
func (f *forgejoFixture) info() *git.PullRequestInfo {
	return &git.PullRequestInfo{Number: 42, URL: f.server.URL + "/team/project/pulls/42", HeadSHA: forgejoTestHead, HeadRefName: "feat/recovery"}
}

func TestForgejoProtectedSquash(t *testing.T) {
	f := newForgejoFixture(t)
	pr, err := f.p.FindPullRequest("feat/recovery", "", 42, forgejoTestHead)
	if err != nil {
		t.Fatal(err)
	}
	sha, err := f.p.MergePR(pr, "squash")
	if err != nil {
		t.Fatal(err)
	}
	if sha != forgejoTestMerge || f.posts != 1 || f.verified != 1 {
		t.Fatalf("missing exact receipt: sha=%s posts=%d proof=%d", sha, f.posts, f.verified)
	}
}

func TestForgejoSeparateFreezeCredential(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("freeze-%t", blocked), func(t *testing.T) {
			f := newForgejoFixture(t)
			f.freezeCredential = "readonly-owner-fixture"
			f.p.freezeToken = f.freezeCredential
			f.freeze = blocked
			_, err := f.p.MergePR(f.info(), "squash")
			if blocked && (err == nil || f.posts != 0) {
				t.Fatalf("freeze must refuse before mutation: %v posts=%d", err, f.posts)
			}
			if !blocked && (err != nil || f.posts != 1) {
				t.Fatalf("dedicated controller must merge after read-only freeze check: %v posts=%d", err, f.posts)
			}
		})
	}
}

func TestForgejoMissingSeparateFreezeCredential(t *testing.T) {
	f := newForgejoFixture(t)
	f.p.cfg.FreezeTokenEnv = "GT_FORGEJO_MISSING_FREEZE_TOKEN"
	t.Setenv(f.p.cfg.FreezeTokenEnv, "")
	if _, err := newForgejoPRProvider(git.NewGit(filepath.Dir(filepath.Dir(f.p.lockPath))), &f.p.cfg); err == nil {
		t.Fatal("configured missing freeze credential must fail closed")
	}
}
func TestForgejoRefusesBeforeMutation(t *testing.T) {
	tests := map[string]func(*forgejoFixture){
		"failed required status":  func(f *forgejoFixture) { f.statuses[0].Status = "failure" },
		"pending required status": func(f *forgejoFixture) { f.statuses[0].Status = "pending" },
		"missing required status": func(f *forgejoFixture) { f.statuses = f.statuses[1:] },
		"null status evidence":    func(f *forgejoFixture) { f.statuses = nil },
		"unknown status id":       func(f *forgejoFixture) { f.statuses[0].ID = 0 },
		"duplicate status id":     func(f *forgejoFixture) { f.statuses[0].ID = 2 },
		"protection missing":      func(f *forgejoFixture) { f.protection.ApplyToAdmins = nil },
		"admin bypass":            func(f *forgejoFixture) { f.protection.ApplyToAdmins = boolPointer(false) },
		"checks disabled":         func(f *forgejoFixture) { f.protection.EnableStatusCheck = boolPointer(false) },
		"empty required contexts": func(f *forgejoFixture) { f.protection.StatusCheckContexts = nil },
		"unsupported glob":        func(f *forgejoFixture) { f.protection.StatusCheckContexts = []string{"CI [ab]"} },
		"draft":                   func(f *forgejoFixture) { f.pr.Draft = boolPointer(true) },
		"unknown draft":           func(f *forgejoFixture) { f.pr.Draft = nil },
		"not mergeable":           func(f *forgejoFixture) { f.pr.Mergeable = boolPointer(false) },
		"unknown mergeability":    func(f *forgejoFixture) { f.pr.Mergeable = nil },
		"manual body":             func(f *forgejoFixture) { f.pr.Body = "This PR must not be merged automatically." },
		"manual label": func(f *forgejoFixture) {
			f.pr.Labels = append(f.pr.Labels, struct {
				Name string `json:"name"`
			}{"no-auto-merge"})
		},
		"foreign head":             func(f *forgejoFixture) { f.pr.Head.Repo.FullName = "other/project" },
		"wrong target":             func(f *forgejoFixture) { f.pr.Base.Ref = "release" },
		"head changed":             func(f *forgejoFixture) { f.pr.Head.SHA = strings.Repeat("e", 40) },
		"head race":                func(f *forgejoFixture) { f.changeAfterChecks = true },
		"freeze":                   func(f *forgejoFixture) { f.freeze = true },
		"protection forbidden":     func(f *forgejoFixture) { f.failPath = "branch_protections" },
		"freeze forbidden":         func(f *forgejoFixture) { f.failPath = "QUEUE_FREEZE" },
		"missing required reviews": func(f *forgejoFixture) { f.protection.RequiredApprovals = 1 },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f := newForgejoFixture(t)
			change(f)
			_, err := f.p.MergePR(f.info(), "squash")
			if err == nil || f.posts != 0 {
				t.Fatalf("refusal failed: err=%v posts=%d", err, f.posts)
			}
			if strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("secret leaked")
			}
		})
	}
}
func TestForgejoLatestStatusesAndOptionalFailure(t *testing.T) {
	f := newForgejoFixture(t)
	f.statuses = append(f.statuses, forgejoStatus{ID: 4, Context: "unrequired cancelled job", Status: "failure"}, forgejoStatus{ID: 10, Context: "Guarded Lane", Status: "failure"}, forgejoStatus{ID: 11, Context: "Guarded Lane", Status: "success"})
	if _, err := f.p.MergePR(f.info(), "squash"); err != nil {
		t.Fatal(err)
	}
}
func TestForgejoNewerFailureWins(t *testing.T) {
	f := newForgejoFixture(t)
	f.statuses = append(f.statuses, forgejoStatus{ID: 20, Context: "Guarded Lane", Status: "failure"})
	if _, err := f.p.MergePR(f.info(), "squash"); err == nil || f.posts != 0 {
		t.Fatal("older success accepted")
	}
}
func TestForgejoReviews(t *testing.T) {
	for _, state := range []string{"APPROVED", "REQUEST_CHANGES", "COMMENTED"} {
		t.Run(state, func(t *testing.T) {
			f := newForgejoFixture(t)
			f.protection.RequiredApprovals = 1
			r := forgejoReview{ID: 1, State: state, CommitID: forgejoTestHead}
			r.User.ID = 1
			f.reviews = []forgejoReview{r}
			_, err := f.p.MergePR(f.info(), "squash")
			if (err == nil) != (state == "APPROVED") {
				t.Fatalf("state=%s err=%v", state, err)
			}
		})
	}
	t.Run("stale approval", func(t *testing.T) {
		f := newForgejoFixture(t)
		f.protection.RequiredApprovals = 1
		r := forgejoReview{ID: 1, State: "APPROVED", CommitID: strings.Repeat("f", 40)}
		r.User.ID = 1
		f.reviews = []forgejoReview{r}
		if _, err := f.p.MergePR(f.info(), "squash"); err == nil {
			t.Fatal("stale approval accepted")
		}
	})
}
func TestForgejoOutcomeMustBeProven(t *testing.T) {
	for _, code := range []int{202, 204, 405, 409, 423, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newForgejoFixture(t)
			f.postCode = code
			if _, err := f.p.MergePR(f.info(), "squash"); err == nil {
				t.Fatal("unconfirmed outcome accepted")
			}
			if f.posts != 1 {
				t.Fatal("unexpected retry")
			}
		})
	}
	t.Run("missing receipt", func(t *testing.T) {
		f := newForgejoFixture(t)
		f.badReceipt = true
		if _, err := f.p.MergePR(f.info(), "squash"); err == nil {
			t.Fatal("missing receipt accepted")
		}
	})
	t.Run("unreachable receipt", func(t *testing.T) {
		f := newForgejoFixture(t)
		f.p.verifyCommit = func(string, string) error { return fmt.Errorf("unreachable") }
		if _, err := f.p.MergePR(f.info(), "squash"); err == nil {
			t.Fatal("unreachable merge accepted")
		}
	})
}
func TestForgejoIdentityAndMethod(t *testing.T) {
	f := newForgejoFixture(t)
	for _, raw := range []string{"https://foreign.invalid/team/project/pulls/42", f.server.URL + "/team/other/pulls/42", f.server.URL + "/team/project/pulls/43"} {
		if _, err := f.p.prNumber(raw, 42); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := f.p.prNumber("", 0); err == nil {
		t.Fatal("missing PR identity accepted")
	}
	if _, err := f.p.MergePR(f.info(), "merge"); err == nil || f.posts != 0 {
		t.Fatal("merge commit allowed")
	}
	f.p.cfg.ControllerUser = "other-controller"
	if _, err := f.p.MergePR(f.info(), "squash"); err == nil || f.posts != 0 {
		t.Fatal("wrong credential identity accepted")
	}
}
func TestForgejoConfigSafety(t *testing.T) {
	f := newForgejoFixture(t)
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "remote", "add", "forgejo", f.server.URL+"/team/project.git")
	for name, change := range map[string]func(*config.ForgejoConfig){"HTTP opt in": func(c *config.ForgejoConfig) { c.AllowHTTP = false }, "server gates opt in": func(c *config.ForgejoConfig) { c.ServerSideGates = false }, "credential": func(c *config.ForgejoConfig) { c.TokenEnv = "UNSET_GT67J_SECRET" }, "foreign instance": func(c *config.ForgejoConfig) { c.BaseURL = "https://other.invalid" }} {
		t.Run(name, func(t *testing.T) {
			c := f.p.cfg
			change(&c)
			if _, err := newForgejoPRProvider(git.NewGit(dir), &c); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
	run(t, dir, "git", "remote", "set-url", "--push", "forgejo", "https://other.invalid/team/project.git")
	if _, err := newForgejoPRProvider(git.NewGit(dir), &f.p.cfg); err == nil {
		t.Fatal("split destination accepted")
	}
}
func TestForgejoDoesNotFollowRedirects(t *testing.T) {
	f := newForgejoFixture(t)
	calls := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 302) }))
	defer redirect.Close()
	f.p.api = redirect.URL
	_, err := f.p.request("GET", "/user", nil, &struct{}{})
	if err == nil || calls != 0 {
		t.Fatal("credential redirect followed")
	}
}
func TestForgejoConfigRoundTrip(t *testing.T) {
	cfg := config.MergeQueueConfig{VCSProvider: "forgejo", Forgejo: &config.ForgejoConfig{Remote: "forgejo", ControllerUser: "refinery", TokenEnv: "FORGEJO_TOKEN"}}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored config.MergeQueueConfig
	if err = json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Forgejo == nil || restored.Forgejo.Remote != "forgejo" {
		t.Fatal("provider config lost")
	}
	// This file is intentionally local test data, never an active rig config.
	if err = os.WriteFile(filepath.Join(t.TempDir(), "config.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestForgejoDisabledByDefault(t *testing.T) {
	f := newForgejoFixture(t)
	f.p.cfg.LandingEnabled = false
	if _, err := f.p.MergePR(f.info(), "squash"); err == nil || f.posts != 0 {
		t.Fatal("unactivated provider mutated PR")
	}
}
func TestForgejoPaginatedEvidence(t *testing.T) {
	// A full first page cannot hide a later failing required context.
	f := newForgejoFixture(t)
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Query().Get("page") == "1" {
			rows := make([]forgejoStatus, 50)
			for i := range rows {
				rows[i] = forgejoStatus{ID: int64(i + 1), Context: fmt.Sprint("optional", i), Status: "success"}
			}
			json.NewEncoder(w).Encode(rows)
		} else {
			json.NewEncoder(w).Encode([]forgejoStatus{{ID: 100, Context: "Guarded Lane", Status: "failure"}})
		}
	}))
	defer server.Close()
	f.p.api = server.URL
	rows, err := forgejoPages[forgejoStatus](f.p, "/statuses/head")
	if err != nil || len(rows) != 51 || pages != 2 || rows[50].Status != "failure" {
		t.Fatalf("pagination lost evidence: %d %d %v", len(rows), pages, err)
	}
}
