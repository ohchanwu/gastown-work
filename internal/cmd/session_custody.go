package cmd

import (
	"context"
	"io"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/tmux"
)

var sessionCustodyID string

func init() {
	rootCmd.AddCommand(sessionCustodyCmd)
	rootCmd.AddCommand(sessionCustodyInitCmd)
	sessionCustodyCmd.Flags().StringVar(&sessionCustodyID, "id", "", "generation-bound custody token")
}

var sessionCustodyCmd = &cobra.Command{
	Use:    "session-custody --id <token> -- <command>",
	Short:  "Launch an internal agent command in an OS-owned process container",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		return tmux.RunSessionCustodyCommandWithBrokerExecutorPolicy(sessionCustodyID, args[0], func(args []string) error {
			return IsBrokerSafeCommand(rootCmd, args)
		}, isDetachedSessionBrokerCommand, executeTrustedSessionBrokerCommand)
	},
}

func executeTrustedSessionBrokerCommand(ctx context.Context, args []string, _ io.Reader, stdout, _ io.Writer) (bool, error) {
	if len(args) == 4 && args[0] == "witness" && args[1] == "handle-lifecycle" {
		return true, executeWitnessHandleLifecycleContext(ctx, args[2], args[3], stdout)
	}
	if len(args) >= 2 && args[0] == "mail" && args[1] == "inbox" {
		if err := dispatchWitnessLifecycleInboxContext(ctx); err != nil {
			return true, err
		}
		return false, nil
	}
	return false, nil
}

var sessionCustodyInitCmd = &cobra.Command{
	Use:    "session-custody-init",
	Short:  "Run the trusted Linux session namespace init",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return tmux.RunSessionCustodyInit()
	},
}
