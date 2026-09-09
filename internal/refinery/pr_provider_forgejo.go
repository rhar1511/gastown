package refinery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
)

var forgejoSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var forgejoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var forgejoNoAuto = regexp.MustCompile(`(?i)(must\s+not\s+be\s+merged\s+automatically|do\s+not\s+(automatically\s+|auto[- ]?)?merge|no[-_ ]auto([-_ ]merge)?|manual[- ]only)`)

// forgejoPRProvider never approves, rebases, arms auto-merge, deletes branches or
// falls back to a Git push. The server is the final branch-protection authority.
type forgejoPRProvider struct {
	cfg              config.ForgejoConfig
	repo, api, token string
	freezeToken      string
	lockPath         string
	client           *http.Client
	verifyCommit     func(string, string) error
	mu               sync.Mutex
}

type forgejoRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}
type forgejoPR struct {
	Number         int        `json:"number"`
	State          string     `json:"state"`
	Draft          *bool      `json:"draft"`
	Merged         *bool      `json:"merged"`
	Mergeable      *bool      `json:"mergeable"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Head           forgejoRef `json:"head"`
	Base           forgejoRef `json:"base"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	Labels         []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type forgejoStatus struct {
	ID      int64  `json:"id"`
	Context string `json:"context"`
	Status  string `json:"status"`
}
type forgejoReview struct {
	ID        int64  `json:"id"`
	CommitID  string `json:"commit_id"`
	State     string `json:"state"`
	Dismissed bool   `json:"dismissed"`
	Stale     bool   `json:"stale"`
	User      struct {
		ID int64 `json:"id"`
	} `json:"user"`
}
type forgejoProtection struct {
	ApplyToAdmins       *bool    `json:"apply_to_admins"`
	EnableStatusCheck   *bool    `json:"enable_status_check"`
	StatusCheckContexts []string `json:"status_check_contexts"`
	RequiredApprovals   int      `json:"required_approvals"`
}

func newForgejoPRProvider(g *git.Git, cfg *config.ForgejoConfig) (*forgejoPRProvider, error) {
	if g == nil || cfg == nil {
		return nil, errors.New("forgejo requires explicit configuration")
	}
	if !cfg.ServerSideGates {
		return nil, errors.New("forgejo requires server_side_gates=true (required remote CI)")
	}
	if !forgejoName.MatchString(cfg.Remote) || strings.HasPrefix(cfg.Remote, "-") || cfg.TargetBranch == "" || cfg.ControllerUser == "" || cfg.TokenEnv == "" {
		return nil, errors.New("forgejo requires remote, target_branch, controller_user and token_env")
	}
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.RawPath != "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("forgejo base_url must be an explicit HTTP(S) instance URL without credentials")
	}
	if base.Scheme == "http" && !cfg.AllowHTTP {
		return nil, errors.New("forgejo HTTP requires allow_http=true on a trusted transport")
	}
	remote, err := g.RemoteURL(cfg.Remote)
	if err != nil {
		return nil, errors.New("forgejo canonical remote is unavailable")
	}
	push, err := g.GetPushURL(cfg.Remote)
	if err != nil || strings.TrimSuffix(remote, ".git") != strings.TrimSuffix(push, ".git") {
		return nil, errors.New("forgejo split fetch/push destinations are forbidden")
	}
	u, err := url.Parse(remote)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Scheme != base.Scheme || u.Host != base.Host {
		return nil, errors.New("forgejo remote must use the configured HTTP(S) instance")
	}
	prefix := strings.TrimRight(base.Path, "/") + "/"
	if !strings.HasPrefix(u.Path, prefix) {
		return nil, errors.New("forgejo remote path is outside base_url")
	}
	repo := strings.TrimSuffix(strings.TrimPrefix(u.Path, prefix), ".git")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || !forgejoName.MatchString(parts[0]) || !forgejoName.MatchString(parts[1]) || parts[0] == ".." || parts[1] == ".." {
		return nil, errors.New("forgejo remote must name owner/repository")
	}
	token := os.Getenv(cfg.TokenEnv)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("forgejo credential environment is missing or invalid")
	}
	freezeToken := ""
	if cfg.FreezeTokenEnv != "" {
		freezeToken = os.Getenv(cfg.FreezeTokenEnv)
		if freezeToken == "" || strings.ContainsAny(freezeToken, "\r\n") {
			return nil, errors.New("forgejo freeze credential environment is missing or invalid")
		}
	}
	p := &forgejoPRProvider{cfg: *cfg, repo: repo, api: base.String() + "/api/v1", token: token,
		freezeToken: freezeToken,
		client:      &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	p.cfg.BaseURL = base.String()
	p.lockPath = filepath.Join(g.WorkDir(), ".runtime", "forgejo-provider.lock")
	p.verifyCommit = func(target, sha string) error {
		current, fetchErr := g.RemoteURL(cfg.Remote)
		currentPush, pushErr := g.GetPushURL(cfg.Remote)
		if fetchErr != nil || pushErr != nil || current != remote || currentPush != push {
			return errors.New("forgejo canonical remote changed during delivery")
		}

		if err := g.VerifyPushedCommitReachableFromPushTarget(cfg.Remote, target, sha); err != nil {
			return errors.New("forgejo merge commit is not proven reachable on the canonical remote")
		}
		return nil
	}
	return p, nil
}

// request suppresses response bodies and transport errors: they may contain
// credentials or private repository content. Redirects are never followed.
func (p *forgejoPRProvider) request(method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, errors.New("forgejo request encoding failed")
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, p.api+path, reader)
	if err != nil {
		return 0, errors.New("forgejo request construction failed")
	}
	// Some Forgejo versions restrict variable reads to the repository owner.
	// A separate repository-scoped read-only token is used ONLY for this exact
	// GET. Identity, protection, PR reads and every mutation retain the dedicated
	// controller credential; policy-reader authority never becomes merge authority.
	if p.freezeToken != "" && method == http.MethodGet && path == p.repoPath()+"/actions/variables/QUEUE_FREEZE" {
		req.Header.Set("Authorization", "token "+p.freezeToken)
	} else if p.cfg.BasicAuth {
		req.SetBasicAuth(p.cfg.ControllerUser, p.token)
	} else {
		req.Header.Set("Authorization", "token "+p.token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, errors.New("forgejo transport failed; outcome may be unknown, inspect before retry")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("forgejo API refused request (HTTP %d)", resp.StatusCode)
	}
	if out != nil {
		b, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
		if err != nil || len(b) > 8<<20 || json.Unmarshal(b, out) != nil {
			return resp.StatusCode, errors.New("forgejo response is invalid or incomplete")
		}
	}
	return resp.StatusCode, nil
}
func (p *forgejoPRProvider) repoPath() string { return "/repos/" + p.repo }
func (p *forgejoPRProvider) prNumber(raw string, n int) (int, error) {
	if raw != "" {
		u, err := url.Parse(raw)
		prefix := p.cfg.BaseURL + "/" + p.repo + "/pulls/"
		if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasPrefix(raw, prefix) {
			return 0, errors.New("forgejo PR URL does not match canonical repository")
		}
		parsed, err := strconv.Atoi(strings.TrimPrefix(raw, prefix))
		if err != nil || parsed <= 0 || (n > 0 && n != parsed) {
			return 0, errors.New("forgejo PR identity is invalid or inconsistent")
		}
		n = parsed
	}
	if n <= 0 {
		return 0, errors.New("forgejo requires a recorded PR number or canonical PR URL")
	}
	return n, nil
}
func (p *forgejoPRProvider) readPR(n int) (*forgejoPR, error) {
	var pr forgejoPR
	_, err := p.request(http.MethodGet, p.repoPath()+"/pulls/"+strconv.Itoa(n), nil, &pr)
	if err != nil {
		return nil, err
	}
	if pr.Number != n || pr.Base.Repo.FullName != p.repo || pr.Base.Ref != p.cfg.TargetBranch || !forgejoSHA.MatchString(pr.Head.SHA) || pr.Merged == nil || pr.Draft == nil {
		return nil, errors.New("forgejo PR identity, target or state is incomplete/mismatched")
	}
	return &pr, nil
}
func (p *forgejoPRProvider) actionable(pr *forgejoPR) error {
	if pr.State != "open" || *pr.Merged || *pr.Draft || pr.Mergeable == nil || !*pr.Mergeable {
		return errors.New("forgejo PR is not open, ready and mergeable")
	}
	if pr.Head.Repo.FullName != p.repo {
		return errors.New("forgejo fork PR requires separate admission")
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(pr.Title)), "wip:") || forgejoNoAuto.MatchString(pr.Title+"\n"+pr.Body) {
		return errors.New("forgejo PR opts out of automated landing")
	}
	for _, l := range pr.Labels {
		name := strings.ToLower(strings.TrimSpace(l.Name))
		if forgejoNoAuto.MatchString(strings.NewReplacer("-", " ", "_", " ").Replace(name)) || name == "no-merge" || name == "hold:mayor" || name == "hold:external" {
			return errors.New("forgejo PR label holds automated landing")
		}
	}
	return nil
}
func (p *forgejoPRProvider) FindPullRequest(branch, raw string, n int, head string) (*git.PullRequestInfo, error) {
	n, err := p.prNumber(raw, n)
	if err != nil {
		return nil, err
	}
	pr, err := p.readPR(n)
	if err != nil {
		return nil, err
	}
	if err = p.actionable(pr); err != nil {
		return nil, err
	}
	if !forgejoSHA.MatchString(head) || pr.Head.SHA != head || pr.Head.Ref != branch {
		return nil, errors.New("forgejo submitted head or branch changed")
	}
	return &git.PullRequestInfo{Number: n, URL: p.cfg.BaseURL + "/" + p.repo + "/pulls/" + strconv.Itoa(n), State: "OPEN", HeadSHA: head, HeadRefName: branch, BaseRepo: p.repo, HeadRepo: pr.Head.Repo.FullName}, nil
}

// All paginated evidence is bounded; hitting the bound is a refusal, not a
// partial success. API status IDs determine latest status per context.
func forgejoPages[T any](p *forgejoPRProvider, path string) ([]T, error) {
	var all []T
	for page := 1; page <= 100; page++ {
		var rows []T
		_, err := p.request(http.MethodGet, path+"?limit=50&page="+strconv.Itoa(page), nil, &rows)
		if err != nil {
			return nil, err
		}
		if rows == nil {
			return nil, errors.New("forgejo evidence page is null")
		}
		all = append(all, rows...)
		if len(rows) < 50 {
			return all, nil
		}
	}
	return nil, errors.New("forgejo evidence pagination limit exceeded")
}
func (p *forgejoPRProvider) approvals(n int, head string) (int, error) {
	rows, err := forgejoPages[forgejoReview](p, p.repoPath()+"/pulls/"+strconv.Itoa(n)+"/reviews")
	if err != nil {
		return 0, err
	}
	latest := map[int64]forgejoReview{}
	for _, r := range rows {
		if r.ID <= 0 || r.User.ID <= 0 {
			return 0, errors.New("forgejo review identity is incomplete")
		}
		// Comments are not review verdicts and must not erase a prior rejection.
		state := strings.ToUpper(r.State)
		if state != "APPROVED" && state != "REQUEST_CHANGES" && state != "CHANGES_REQUESTED" {
			continue
		}
		if r.ID > latest[r.User.ID].ID {
			latest[r.User.ID] = r
		}
	}
	count := 0
	for _, r := range latest {
		if r.Dismissed || r.Stale {
			continue
		}
		if r.State == "REQUEST_CHANGES" || r.State == "CHANGES_REQUESTED" {
			return 0, errors.New("forgejo review requests changes")
		}
		if r.State == "APPROVED" && r.CommitID == head {
			count++
		}
	}
	return count, nil
}
func (p *forgejoPRProvider) IsPRApproved(pr *git.PullRequestInfo) (bool, error) {
	if pr == nil {
		return false, errors.New("forgejo PR is missing")
	}
	n, err := p.approvals(pr.Number, pr.HeadSHA)
	return n > 0, err
}

// Only '*' and '?' patterns are supported initially. Refuse richer patterns
// rather than silently interpreting Forgejo's glob syntax differently.
func forgejoContextMatch(pattern, context string) (bool, error) {
	if pattern == "" || strings.ContainsAny(pattern, "[]{}\\") {
		return false, errors.New("forgejo required status pattern is unsupported")
	}
	s := regexp.QuoteMeta(pattern)
	s = strings.ReplaceAll(s, `\*`, ".*")
	s = strings.ReplaceAll(s, `\?`, ".")
	return regexp.MatchString("^"+s+"$", context)
}
func (p *forgejoPRProvider) ready(pr *forgejoPR) error {
	var user struct {
		Login string `json:"login"`
	}
	if _, err := p.request(http.MethodGet, "/user", nil, &user); err != nil {
		return err
	}
	if user.Login != p.cfg.ControllerUser {
		return errors.New("forgejo credential does not match controller_user")
	}
	var freeze struct {
		Data string `json:"data"`
	}
	code, err := p.request(http.MethodGet, p.repoPath()+"/actions/variables/QUEUE_FREEZE", nil, &freeze)
	if code != http.StatusNotFound {
		if err != nil {
			return err
		}
		if freeze.Data != "false" && freeze.Data != "0" {
			return errors.New("forgejo queue freeze is active or ambiguous")
		}
	}
	// Resolve the effective protection rule, including wildcard branch rules.
	var branch struct {
		Name         string `json:"name"`
		Protected    bool   `json:"protected"`
		Rule         string `json:"effective_branch_protection_name"`
		UserCanMerge bool   `json:"user_can_merge"`
	}
	if _, err := p.request(http.MethodGet, p.repoPath()+"/branches/"+url.PathEscape(pr.Base.Ref), nil, &branch); err != nil {
		return err
	}
	if branch.Name != pr.Base.Ref || !branch.Protected || branch.Rule == "" || !branch.UserCanMerge {
		return errors.New("forgejo effective branch protection/merge permission is unproven")
	}
	var protection forgejoProtection
	if _, err := p.request(http.MethodGet, p.repoPath()+"/branch_protections/"+url.PathEscape(branch.Rule), nil, &protection); err != nil {
		return err
	}
	if protection.ApplyToAdmins == nil || !*protection.ApplyToAdmins || protection.EnableStatusCheck == nil || !*protection.EnableStatusCheck || len(protection.StatusCheckContexts) == 0 {
		return errors.New("forgejo enforced required statuses are unproven")
	}
	rows, err := forgejoPages[forgejoStatus](p, p.repoPath()+"/statuses/"+pr.Head.SHA)
	if err != nil {
		return err
	}
	latest := map[string]forgejoStatus{}
	ids := map[int64]bool{}
	for _, s := range rows {
		if s.ID <= 0 || s.Context == "" || strings.ContainsAny(s.Context, "\r\n") || ids[s.ID] {
			return errors.New("forgejo status evidence is ambiguous")
		}
		ids[s.ID] = true
		if s.ID > latest[s.Context].ID {
			latest[s.Context] = s
		}
	}
	for _, pattern := range protection.StatusCheckContexts {
		matched := false
		for context, s := range latest {
			match, err := forgejoContextMatch(pattern, context)
			if err != nil {
				return err
			}
			if match {
				matched = true
				if s.Status != "success" {
					return errors.New("forgejo required status is not successful")
				}
			}
		}
		if !matched {
			return errors.New("forgejo required status is missing")
		}
	}
	count, err := p.approvals(pr.Number, pr.Head.SHA)
	if err != nil {
		return err
	}
	if protection.RequiredApprovals < 0 || count < protection.RequiredApprovals {
		return errors.New("forgejo required current-head approvals are missing")
	}
	return nil
}

func (p *forgejoPRProvider) MergePR(info *git.PullRequestInfo, method string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cfg.LandingEnabled {
		return "", errors.New("forgejo landing is disabled until controller handoff is accepted")
	}
	if p.lockPath == "" {
		return "", errors.New("forgejo shared provider lock is missing")
	}
	if err := os.MkdirAll(filepath.Dir(p.lockPath), 0700); err != nil {
		return "", errors.New("forgejo provider lock directory unavailable")
	}
	lock := flock.New(p.lockPath)
	held, err := lock.TryLock()
	if err != nil || !held {
		return "", errors.New("another Forgejo merge owns this rig")
	}
	defer lock.Unlock()
	if info == nil || method != "squash" || !forgejoSHA.MatchString(info.HeadSHA) {
		return "", errors.New("forgejo requires exact-head squash merge")
	}
	n, err := p.prNumber(info.URL, info.Number)
	if err != nil {
		return "", err
	}
	pr, err := p.readPR(n)
	if err != nil {
		return "", err
	}
	if err = p.actionable(pr); err != nil {
		return "", err
	}
	if pr.Head.SHA != info.HeadSHA || pr.Head.Ref != info.HeadRefName {
		return "", errors.New("forgejo submitted head or branch changed")
	}
	if err = p.ready(pr); err != nil {
		return "", err
	}
	// Refresh after evidence collection. HeadCommitID also closes the server-side
	// head race; server protection checks remain enabled on the POST.
	fresh, err := p.readPR(n)
	if err != nil {
		return "", err
	}
	if err = p.actionable(fresh); err != nil {
		return "", err
	}
	if fresh.Head.SHA != info.HeadSHA || fresh.Head.Ref != info.HeadRefName {
		return "", errors.New("forgejo head changed during validation")
	}
	body := map[string]any{"Do": "squash", "head_commit_id": info.HeadSHA, "force_merge": false, "merge_when_checks_succeed": false, "delete_branch_after_merge": false}
	code, err := p.request(http.MethodPost, p.repoPath()+"/pulls/"+strconv.Itoa(n)+"/merge", body, nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", errors.New("forgejo immediate merge was not confirmed; inspect outcome before retry")
	}
	return p.verifyMerged(n, info.HeadSHA)
}
func (p *forgejoPRProvider) verifyMerged(n int, head string) (string, error) {
	pr, err := p.readPR(n)
	if err != nil {
		return "", err
	}
	if pr.State != "closed" || !*pr.Merged || pr.Head.SHA != head || !forgejoSHA.MatchString(pr.MergeCommitSHA) {
		return "", errors.New("forgejo exact-head squash merge receipt is unproven")
	}
	if err = p.verifyCommit(pr.Base.Ref, pr.MergeCommitSHA); err != nil {
		return "", err
	}
	return pr.MergeCommitSHA, nil
}
