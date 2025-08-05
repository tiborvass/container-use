package main

import (
	"os"

	"github.com/dagger/container-use/cosmos"
	"github.com/spf13/cobra"
)

var claudeCmd = &cobra.Command{
	Use:   "claude [env]",
	Short: "Run Claude Code in a containerized environment",
	Long: `Run Claude Code inside a container with API proxy and isolated environment.

The Claude environment provides:
- Isolated git worktree for the environment
- API proxy for monitoring and control
- Automatic git commits on tool completion
- Persistent state across sessions`,
	Example: `  # Start Claude in a new environment
  cu claude

  # Resume a specific environment
  cu claude glad-lark`,

	RunE:   runClaude,
	Hidden: true,

	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// only suggest environments for the first positional args
		if len(args) == 0 {
			return suggestEnvironments(cmd, args, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
}

var (
	claudeEnv string
)

func init() {
	rootCmd.AddCommand(claudeCmd)
}

func runClaude(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	claudeArgs := args

	var envID string
	if len(args) > 0 && args[0] == "--" {
		claudeArgs = claudeArgs[1:]
	}
	if len(claudeArgs) > 0 {
		envID = claudeArgs[0]
		claudeArgs = claudeArgs[1:]
	}

	// Resume specific environment
	if envID != "" {
		claudeArgs = append([]string{"--resume"}, claudeArgs...)
	}

	repoPath, err := os.Getwd()
	if err != nil {
		return err
	}

	c := cosmos.Cosmos{
		SnapshotCallback: func(string) error {
			return nil
		},
	}
	return c.Run(ctx, repoPath, envID, claudeArgs)
}
