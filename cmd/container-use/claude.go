package main

import (
	"fmt"
	"log/slog"

	"github.com/dagger/container-use/environment"
	"github.com/dagger/container-use/repository"
	"github.com/spf13/cobra"
)

var claudeCmd = &cobra.Command{
	Use:   "claude [claude-args...]",
	Short: "Run Claude CLI in a containerized environment",
	Long: `Run Claude CLI inside a container with API proxy and isolated environment.

The Claude environment provides:
- Isolated git worktree for the environment  
- API proxy for monitoring and control
- Automatic git commits on tool completion
- Persistent state across sessions`,
	Example: `  # Start Claude in a new environment
  cu claude

  # Resume a specific environment
  cu claude --env myenv-123

  # Start Claude with specific arguments
  cu claude --model claude-3-opus-20240229`,
	RunE: runClaude,
}

var (
	claudeEnv string
)

func init() {
	rootCmd.AddCommand(claudeCmd)
	claudeCmd.Flags().StringVar(&claudeEnv, "env", "", "Resume a specific environment")
	claudeCmd.RegisterFlagCompletionFunc("env", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return suggestEnvironments(cmd, args, toComplete)
	})
}

func runClaude(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	claudeArgs := args

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

	// For Claude, we pass nil as the dagger client to indicate Docker backend
	var env *environment.Environment
	if envID != "" {
		// Get existing environment
		env, err = repo.Get(ctx, nil, envID)
		if err != nil {
			return fmt.Errorf("failed to get environment %s: %w", envID, err)
		}
		slog.Info("Resuming Claude environment", "id", envID)
	} else {
		// Create new environment with Docker backend (nil dagger client)
		env, err = repo.Create(ctx, nil, "Claude session", "Created for Claude CLI")
		if err != nil {
			return fmt.Errorf("failed to create environment: %w", err)
		}
		envID = env.ID
		slog.Info("Created new Claude environment", "id", envID)
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
