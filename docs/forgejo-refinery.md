# Protected Forgejo delivery from Refinery

Forgejo delivery uses an existing native merge request (MR) and an existing PR.
It never creates PRs, rebases branches, approves Guarded Lane, arms deferred
merges, pushes directly to the target, or deletes source branches/worktrees.
Required CI on the submitted remote head is the validation authority. Local
rebase/test/push instructions in the generic patrol do not apply to this mode.

## Configuration and activation

Configure `merge_queue` in the rig's `config.json` (example only):

```json
{
  "merge_queue": {
    "merge_strategy": "pr",
    "vcs_provider": "forgejo",
    "max_concurrent": 1,
    "delete_merged_branches": false,
    "run_tests": false,
    "forgejo": {
      "remote": "forgejo",
      "base_url": "https://forgejo.example.test",
      "target_branch": "main",
      "controller_user": "refinery-controller",
      "token_env": "GT_FORGEJO_TOKEN",
      "server_side_gates": true,
      "landing_enabled": false
    }
  }
}
```

The named Git remote must be the configured instance's HTTP(S) `owner/repo.git`
URL, with identical fetch and push destinations. SSH remotes are not supported
by this initial adapter. `origin` may point elsewhere and is never used for
Forgejo merge proof. Put the credential in the named environment variable;
configuration contains no token. `basic_auth: true` uses controller_user and the
credential as HTTP Basic authentication. Otherwise the token authentication
scheme is used. The API `/user` identity must match controller_user. HTTP requires
explicit `allow_http: true` and a trusted transport; HTTPS verifies certificates.
Redirects are refused so credentials cannot follow a changed destination.

The inspected LAN server requires repository-admin permission and a
`write:repository` token even to read effective protection. Its variable API
requires the repository owner. A restricted, non-site-admin controller therefore
uses repository-admin access on Inktree, with admin protection still enforced.
Configure `freeze_token_env` with a separate owner token limited to this repository
and `read:repository` scope. The adapter sends that read-only token solely on the
exact `GET .../actions/variables/QUEUE_FREEZE` request. All identity, protection,
PR and merge calls still use the dedicated controller. A configured but missing
freeze token refuses initialization; there is no privileged fallback. Keep both
secrets outside configuration and transcripts. Account permission and token scope
are separate controls; this is no site-admin or protection-bypass grant.

`server_side_gates: true` explicitly selects required remote CI. Remove local
`gates` and nonempty enabled `test_command` configuration before selecting this
mode; loading refuses to silently bypass those commands. Local build/test
success and a polecat's pre-verification flag cannot replace required statuses.

Before changing `landing_enabled` to true, the operator must accept the
controller handoff: quiesce old landing writers and their active jobs, reconcile
existing scheduled merges, assign a distinct controller credential, confirm
protected squash permission, and designate one Refinery owner. This config does
not disable Forgejo workflows or establish that external writers are quiescent.
The rig file lock serializes the native `land` command across local processes;
workflow writers do not share it. In multi-host operation an external exclusive
authority mechanism is still required. Keep the previous binary/configuration
for rollback; stop the new owner and reconcile in-flight outcomes before
restoring the previous owner. Never run both owners during rollback.

## Patrol sequence

The owner submits an existing PR with
`gt refinery submit-pr <rig> <source-id> <pr-number>`. This reads the PR from the
configured canonical Forgejo repository and records its exact live branch, SHA,
target, number and URL in a native MR. It checks source admission and dependency
readiness, preserves source ownership, and refuses PR opt-outs. Exact retries
return the same open MR; conflicting source/branch/PR records require explicit
reconciliation and are never silently superseded. Submission changes no Git refs
or PR state. Required-check success is evaluated by `check-pr` and again at
landing; registering an MR does not authorize a merge. The legacy `gt mq submit`
and `gt done` paths do not yet attach Forgejo PR identity; use this explicit
submission command for this provider, rather than changing a crew's `origin` or
hand-editing incomplete MR records.

1. Read existing native MRs and their source/dependency/admission evidence.
   A convoy edge or an open Forgejo PR is not a native MR submission. Each MR
   needs a rig, source issue, exact submitted `commit_sha`, branch, target, and
   recorded `pr_number` or canonical `pr_url`. This adapter does not guess PRs
   from branch names. The target must equal the configured target branch.
2. Inspect candidates oldest-first and run
   `gt refinery check-pr <rig> <mr-id>`. It reads remote and Beads evidence and
   performs no claim, approval, branch or PR mutations. Only admitted candidates
   with successful preflight are eligible. Preserve original age when repairing
   a candidate; a failed/held candidate does not prevent later eligible work.
3. Claim the selected MR using `gt refinery claim <mr-id>` from that rig. Use a
   consistent `GT_REFINERY_WORKER` identity (default `refinery-1`). Process one MR
   at a time and account for previously armed PRs before selecting another.
4. Run `gt refinery land <rig> <mr-id>`. The command requires that exact claim,
   takes a rig lock, rechecks eligibility, and executes protected squash merging.
   It verifies the merged PR's submitted SHA and merge commit, then verifies that
   commit is reachable on the configured canonical remote before closing work.
   Squash success does not require the source head itself to be an ancestor.
5. Only a verified landing and successful bookkeeping permit a completion
   notification. Source branches/worktrees remain for separate owner-approved
   preservation/cleanup. The Forgejo path does not auto-land convoy integration
   branches through the generic direct-push path.

Use this path instead of `gh pr merge`, shell Git pushes, and the legacy
`gt mq post-merge` ancestry check. Changes to an already submitted head require
fresh MR evidence and checks. A genuine stacked child is eligible only after
its owner retargets it to the configured target and validates the new head;
independent PRs are not automatically rebased or treated as stack children.

## Refusal and proof semantics

The adapter requires an open, non-draft, mergeable PR in the configured repository
and target; fork heads require separate admission. It refuses no-auto/manual-only
instructions, `do-not-merge`/`no-auto-merge` labels, and Mayor/external hold labels.
This policy has no unattended override flag. Supervised operator exceptions use
a separate explicitly authorized path.

The effective branch protection must enforce required status checks for admins
as well. Every required pattern must match at least one current context and all
matching latest statuses must be successful. Optional cancelled/failed jobs do
not block. Evidence is paginated with strict bounds; malformed, incomplete or
inaccessible evidence fails closed. Initially patterns support literal text,
`*` and `?`; richer patterns are refused. Required reviews must approve the exact
head. Active change requests block. The Forgejo server remains the final
whitelist/review/protected-file/merge-permission authority.

`QUEUE_FREEZE` absence (404) permits evaluation; an existing value must explicitly
be `false` or `0`. Other values or an unreadable variable refuse landing. The
merge POST specifies `Do=squash`, `head_commit_id`, `force_merge=false`,
`merge_when_checks_succeed=false`, and `delete_branch_after_merge=false`.
It never uses GET on the merge endpoint. A success response alone is insufficient:
a closed/merged exact-head PR receipt and canonical remote reachability are
required. API/transport errors do not expose response bodies or credentials.

After a timeout, a non-confirming response, or a merge followed by failed receipt
verification, retain the claim and reconcile the exact PR on the server. Do not
blindly retry, close work, or infer success from local HEAD. The initial adapter
does not automatically repair ambiguous outcomes. A provider preflight cannot
atomically freeze PR body/labels, repository configuration or external writers;
exclusive controller ownership and server-side protection remain necessary.

## Validation boundary

HTTP fixture tests cover required-status failure/missing/pending evidence,
manual-only refusal, identity/target/head mismatch, changed-head races, approvals,
freeze, protected payload, error/redirect handling, and missing/unreachable merge
receipts. Engineer tests cover the Forgejo route and preserve GitHub merge-commit
behavior. These are isolated tests, not evidence of a live production merge or an
accepted authority transfer. The observed LAN Forgejo API version was
`15.0.7+gitea-1.22.0`; its OpenAPI and matching upstream release source expose the
conditional immediate merge contract. A private synthetic live-repository exercise on that server subsequently passed
native failed-check/manual-only refusal, protected squash with canonical Git
reachability, and server-side stale-head refusal (409). An initial 405 while the
PR head was still synchronizing was retained separately, then the precise
stale-head check was rerun after synchronization. This used the existing operator
identity, not the future dedicated controller credential. Production controller
credentials, native MR/agent registration, writer quiescence and capacity still
need acceptance before activation.

The optional `TestForgejoLiveAcceptance` test runs only with an explicit
`GT_FORGEJO_ACCEPTANCE_CONFIG` file and only against repository names starting
`gt67j-acceptance-`. It may merge the specified synthetic PR when the expected
outcome is `merge`; normal test runs skip it. Never point it at production work.
