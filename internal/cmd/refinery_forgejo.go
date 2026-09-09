package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/refinery"
)

var refineryCheckPRCmd = &cobra.Command{
	Use: "check-pr <rig> <mr-id>", Short: "Read-only protected Forgejo preflight for an existing MR",
	Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		_, r, err := getRig(args[0])
		if err != nil {
			return err
		}
		eng := refinery.NewEngineer(r)
		if err = eng.LoadConfig(); err != nil {
			return err
		}
		mr, err := eng.CheckForgejoMR(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Forgejo preflight passed: %s head=%s target=%s (no mutation)\n", mr.ID, mr.CommitSHA, mr.Target)
		return nil
	},
}
var refineryLandCmd = &cobra.Command{
	Use: "land <rig> <mr-id>", Short: "Land one claimed MR through protected Forgejo squash merging",
	Long: `Land one admitted, dependency-ready MR with a configured Forgejo provider.
Requires explicit landing_enabled and a completed controller handoff. The MR must
be claimed by GT_REFINERY_WORKER (default refinery-1). Uses required current-head
checks, conditional squash merge and authoritative post-merge proof. No direct
push, rebase, approval, deferred auto-merge or source branch deletion is performed.`,
	Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		_, r, err := getRig(args[0])
		if err != nil {
			return err
		}
		eng := refinery.NewEngineer(r)
		if err = eng.LoadConfig(); err != nil {
			return err
		}
		result, err := eng.LandForgejoMR(cmd.Context(), args[1], getWorkerID())
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Verified Forgejo merge: %s commit=%s\n", args[1], result.MergeCommit)
		return nil
	},
}

func init() { refineryCmd.AddCommand(refineryCheckPRCmd, refineryLandCmd) }
