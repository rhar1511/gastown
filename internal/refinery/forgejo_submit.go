package refinery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/beads"
)

// SubmitForgejoPR registers an existing canonical Forgejo PR as a native MR.
// It does not push, create or change a PR, claim work, or supersede older MRs.
func (e *Engineer) SubmitForgejoPR(sourceID string, prNumber int) (*MRInfo, error) {
	return e.submitForgejoPR(e.beads, sourceID, prNumber)
}

type forgejoSubmitStore interface {
	Show(string) (*beads.Issue, error)
	ListMergeRequests(beads.ListOptions) ([]*beads.Issue, error)
	Create(beads.CreateOptions) (*beads.Issue, error)
}

func (e *Engineer) submitForgejoPR(store forgejoSubmitStore, sourceID string, prNumber int) (*MRInfo, error) {
	p, ok := e.prProvider.(*forgejoPRProvider)
	if !ok || e.config.MergeStrategy != "pr" || e.config.VCSProvider != "forgejo" {
		return nil, errors.New("protected Forgejo provider is not configured")
	}
	if strings.TrimSpace(sourceID) == "" || prNumber <= 0 {
		return nil, errors.New("source issue and positive Forgejo PR number are required")
	}

	if err := os.MkdirAll(filepath.Dir(p.lockPath), 0700); err != nil {
		return nil, err
	}
	providerLock := flock.New(p.lockPath)
	locked, err := providerLock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("another Forgejo operation owns this rig")
	}
	defer providerLock.Unlock()

	source, err := store.Show(sourceID)
	if err != nil {
		return nil, err
	}
	if source == nil || (source.Status != "open" && source.Status != "in_progress" && source.Status != "hooked") ||
		forgejoBeadHeld(source) || beads.HasUnresolvedBlockers(source) ||
		beads.ConcreteWorkIssueRejectReason(source) != "" || beads.HasUncheckedCriteria(source) > 0 {
		return nil, errors.New("source work is not admitted and dependency-ready")
	}
	if af := beads.ParseAttachmentFields(source); af != nil &&
		(af.NoMerge || af.ReviewOnly || strings.EqualFold(strings.TrimSpace(af.MergeStrategy), "local")) {
		return nil, errors.New("source work prohibits automated landing")
	}

	pr, err := p.readPR(prNumber)
	if err != nil {
		return nil, err
	}
	if err := p.actionable(pr); err != nil {
		return nil, err
	}
	if strings.TrimSpace(pr.Head.Ref) == "" || !forgejoSHA.MatchString(pr.Head.SHA) ||
		pr.Head.Repo.FullName != p.repo || pr.Base.Repo.FullName != p.repo || pr.Base.Ref != p.cfg.TargetBranch {
		return nil, errors.New("Forgejo PR repository or target is not canonical")
	}

	canonicalURL := p.cfg.BaseURL + "/" + p.repo + "/pulls/" + fmt.Sprint(pr.Number)
	fields := &beads.MRFields{
		Branch:      pr.Head.Ref,
		Target:      pr.Base.Ref,
		SourceIssue: sourceID,
		Rig:         e.rig.Name,
		CommitSHA:   pr.Head.SHA,
		PRURL:       canonicalURL,
		PRNumber:    pr.Number,
	}

	openMRs, err := store.ListMergeRequests(beads.ListOptions{Status: "open", Label: "gt:merge-request", Rig: e.rig.Name})
	if err != nil {
		return nil, fmt.Errorf("checking existing merge requests: %w", err)
	}
	for _, existing := range openMRs {
		existingFields := beads.ParseMRFields(existing)
		if existingFields == nil {
			continue
		}
		related := existingFields.SourceIssue == sourceID || existingFields.Branch == fields.Branch ||
			existingFields.PRNumber == fields.PRNumber || existingFields.PRURL == fields.PRURL
		if !related {
			continue
		}
		if sameForgejoSubmission(existingFields, fields) {
			return issueToMRInfo(existing, existingFields), nil
		}
		return nil, fmt.Errorf("conflicting open MR %s already records this source, branch or PR", existing.ID)
	}

	// Re-read immediately before the only mutation. A later head change remains
	// safe: check-pr and land bind to this recorded SHA and will refuse it.
	confirmed, err := p.readPR(prNumber)
	if err != nil {
		return nil, err
	}
	if err := p.actionable(confirmed); err != nil {
		return nil, err
	}
	if confirmed.Number != prNumber || strings.TrimSpace(confirmed.Head.Ref) == "" || !forgejoSHA.MatchString(confirmed.Head.SHA) ||
		confirmed.Head.Ref != fields.Branch || confirmed.Head.SHA != fields.CommitSHA ||
		confirmed.Base.Ref != fields.Target || confirmed.Head.Repo.FullName != p.repo || confirmed.Base.Repo.FullName != p.repo {
		return nil, errors.New("Forgejo PR changed during native MR registration")
	}

	created, err := store.Create(beads.CreateOptions{
		Title:       "Merge: " + sourceID,
		Labels:      []string{"gt:merge-request"},
		Priority:    source.Priority,
		Description: beads.FormatMRFields(fields),
		Ephemeral:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("creating native merge request: %w", err)
	}
	createdFields := beads.ParseMRFields(created)
	if created.ID == "" || !sameForgejoSubmission(createdFields, fields) {
		return nil, errors.New("native merge request read-back is incomplete")
	}
	return issueToMRInfo(created, createdFields), nil
}

func sameForgejoSubmission(a, b *beads.MRFields) bool {
	return a != nil && b != nil && a.Branch == b.Branch && a.Target == b.Target &&
		a.SourceIssue == b.SourceIssue && a.Rig == b.Rig && a.CommitSHA == b.CommitSHA &&
		a.PRURL == b.PRURL && a.PRNumber == b.PRNumber
}
