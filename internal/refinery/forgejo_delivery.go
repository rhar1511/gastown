package refinery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/beads"
)

// CheckForgejoMR validates one existing native MR without changing its claim,
// branch, statuses or PR. It is also the preflight for LandForgejoMR.
func (e *Engineer) CheckForgejoMR(id string) (*MRInfo, error) {
	p, ok := e.prProvider.(*forgejoPRProvider)
	if !ok || e.config.MergeStrategy != "pr" || e.config.VCSProvider != "forgejo" {
		return nil, fmt.Errorf("protected Forgejo provider is not configured")
	}
	issue, err := e.beads.Show(id)
	if err != nil {
		return nil, err
	}
	if issue == nil || issue.Status != "open" || !beads.HasLabel(issue, "gt:merge-request") || beads.HasUnresolvedBlockers(issue) || forgejoBeadHeld(issue) {
		return nil, fmt.Errorf("MR is not open and dependency-ready")
	}
	fields := beads.ParseMRFields(issue)
	if fields == nil {
		return nil, fmt.Errorf("MR fields missing")
	}
	mr := issueToMRInfo(issue, fields)
	// Eligibility checks can close rejected MRs, so the read-only check duplicates
	// only the structural checks and reads source admission without those helpers.
	if mr.SourceIssue == "" || fields.Rig != e.rig.Name || mr.Target != p.cfg.TargetBranch || !forgejoSHA.MatchString(mr.CommitSHA) || beads.HasLabel(issue, "gt:owned-direct") || fields.CloseReason != "" {
		return nil, fmt.Errorf("MR identity/target/admission is invalid")
	}
	source, err := e.beads.Show(mr.SourceIssue)
	if err != nil {
		return nil, err
	}
	if source == nil || (source.Status != "open" && source.Status != "in_progress" && source.Status != "hooked") || forgejoBeadHeld(source) || beads.HasUnresolvedBlockers(source) || beads.ConcreteWorkIssueRejectReason(source) != "" || beads.HasUncheckedCriteria(source) > 0 {
		return nil, fmt.Errorf("source work is not admitted and dependency-ready")
	}
	if af := beads.ParseAttachmentFields(source); af != nil && (af.NoMerge || af.ReviewOnly || strings.EqualFold(strings.TrimSpace(af.MergeStrategy), "local")) {
		return nil, fmt.Errorf("source work prohibits automated landing")
	}
	info, err := p.FindPullRequest(mr.Branch, mr.PRURL, mr.PRNumber, mr.CommitSHA)
	if err != nil {
		return nil, err
	}
	pr, err := p.readPR(info.Number)
	if err != nil {
		return nil, err
	}
	if err = p.actionable(pr); err != nil {
		return nil, err
	}
	if pr.Head.SHA != mr.CommitSHA || pr.Head.Ref != mr.Branch {
		return nil, fmt.Errorf("Forgejo head changed during preflight")
	}
	if err = p.ready(pr); err != nil {
		return nil, err
	}
	if e.config.RequireReview != nil && *e.config.RequireReview {
		approved, err := p.IsPRApproved(info)
		if err != nil {
			return nil, err
		}
		if !approved {
			return nil, fmt.Errorf("current-head approving review is missing")
		}
	}
	if _, err = p.FindPullRequest(mr.Branch, mr.PRURL, mr.PRNumber, mr.CommitSHA); err != nil {
		return nil, err
	}
	return mr, nil
}

// LandForgejoMR is the agent-facing protected delivery entry point. The rig lock
// serializes CLI callers; workflow/credential quiescence is an activation gate.
// A claimed MR stays claimed on ambiguous outcomes so an operator can reconcile
// the exact receipt before any retry. No automatic rebase, approval or cleanup
// of branches/worktrees is performed.
func (e *Engineer) LandForgejoMR(ctx context.Context, id, worker string) (ProcessResult, error) {
	if e.config.Forgejo == nil || !e.config.Forgejo.LandingEnabled {
		return ProcessResult{}, fmt.Errorf("Forgejo landing is disabled until controller handoff is accepted")
	}
	runtimeDir := filepath.Join(e.rig.Path, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		return ProcessResult{}, err
	}
	lock := flock.New(filepath.Join(runtimeDir, "forgejo-landing.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return ProcessResult{}, err
	}
	if !locked {
		return ProcessResult{}, fmt.Errorf("another Forgejo landing owns this rig")
	}
	defer lock.Unlock()
	mr, err := e.CheckForgejoMR(id)
	if err != nil {
		return ProcessResult{}, err
	}
	if worker == "" || mr.Assignee != worker {
		return ProcessResult{}, fmt.Errorf("MR must be explicitly claimed by this refinery worker")
	}
	result := e.ProcessMRInfo(ctx, mr)
	if !result.Success {
		return result, fmt.Errorf("Forgejo landing refused: %s", result.Error)
	}
	if !e.HandleMRInfoSuccess(mr, result) {
		return result, fmt.Errorf("Forgejo merge succeeded but bookkeeping proof/closure failed; reconcile before retry")
	}
	return result, nil
}

// Holds remain authoritative even if a task has no unresolved dependency edge.
func forgejoBeadHeld(issue *beads.Issue) bool {
	for _, label := range issue.Labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if strings.HasPrefix(label, "hold:") || label == "gt:owned-direct" || forgejoNoAuto.MatchString(label) {
			return true
		}
	}
	return false
}
