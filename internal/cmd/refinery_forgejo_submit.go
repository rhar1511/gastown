package cmd

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/refinery"
)

var refinerySubmitPRCmd = &cobra.Command{
	Use:   "submit-pr <rig> <source-id> <pr-number>",
	Short: "Register an existing canonical Forgejo PR as a native MR",
	Long: `Register one existing Forgejo PR using its live canonical repository,
target, branch and exact head. The command validates source admission and
dependency readiness, and refuses conflicting native MRs. It does not push,
create or mutate a PR, claim work, supersede an MR, or close/reassign its source.`,
	Args: cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		prNumber, err := strconv.Atoi(args[2])
		if err != nil || prNumber <= 0 {
			return fmt.Errorf("invalid Forgejo PR number %q", args[2])
		}
		_, r, err := getRig(args[0])
		if err != nil {
			return err
		}
		eng := refinery.NewEngineer(r)
		if err = eng.LoadConfig(); err != nil {
			return err
		}
		mr, err := eng.SubmitForgejoPR(args[1], prNumber)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Registered Forgejo MR: %s PR=%d head=%s target=%s\n", mr.ID, mr.PRNumber, mr.CommitSHA, mr.Target)
		return nil
	},
}

func init() { refineryCmd.AddCommand(refinerySubmitPRCmd) }
