package main

import (
	"fmt"
	"log/slog"

	"github.com/dagger/container-use/environment"
	"github.com/dagger/container-use/repository"
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

	RunE: runClaude,

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

	if len(args) > 0 && args[0] == "--" {
		claudeArgs = claudeArgs[1:]
	}
	if len(claudeArgs) > 0 {
		claudeEnv = claudeArgs[0]
		claudeArgs = claudeArgs[1:]
	}

	var envID string
	if claudeEnv != "" {
		// Resume specific environment
		envID = claudeEnv
		// Add resume flag for Claude CLI
		claudeArgs = append([]string{"--resume"}, claudeArgs...)
	}

	repo, err := repository.Open(ctx, ".")
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	dag, err := daggerConnect(ctx, logWriter)
	if err != nil {
		return err
	}
	defer dag.Close()

	// For Claude, we pass nil as the dagger client to indicate Docker backend
	var env *environment.Environment
	if envID != "" {
		// Get existing environment
		env, err = repo.Get(ctx, dag, envID)
		if err != nil {
			return fmt.Errorf("failed to get environment %s: %w", envID, err)
		}
		slog.Info("Resuming Claude Code environment", "id", envID)
	} else {
		env, err = repo.Create(ctx, dag, "Claude session", "Created for Claude CLI", true)
		if err != nil {
			return fmt.Errorf("failed to create environment: %w", err)
		}
		envID = env.ID
		slog.Info("Created new Claude Code environment", "id", envID)
	}

	// Get worktree path
	worktree, err := repo.WorktreePath(envID)
	if err != nil {
		return fmt.Errorf("failed to get worktree path: %w", err)
	}

	// Set up snapshot callback to commit git changes when tools complete
	env.SetDockerSnapshotCallback(func(message string) error {
		slog.Info("Creating git snapshot", "message", message)
		// This will create a git commit in the worktree and update notes
		if err := repo.Update(ctx, env, message); err != nil {
			slog.Error("Failed to update repository", "error", err)
			return err
		}
		return nil
	})

	// Start Docker-based Claude session
	return env.StartDockerSession(ctx, worktree, claudeArgs)
}
