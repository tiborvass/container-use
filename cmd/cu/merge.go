package main

import (
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

var mergeCmd = &cobra.Command{
	Use:   "merge <env>",
	Short: "Merges an environment into the current git branch",
	Args:  cobra.ExactArgs(1),
	RunE: func(app *cobra.Command, args []string) error {
		env := args[0]
		// prevent accidental single quotes to mess up command
		env = strings.Trim(env, "'")
		ctx := app.Context()
		err := exec.Command("git", "stash", "--include-untracked", "-q").Run()
		if err == nil {
			defer exec.Command("git", "stash", "pop", "-q").Run()
		}
		cmd := exec.CommandContext(ctx, "git", "merge", "-m", "Merge environment "+env, "--", "container-use/"+env)
		cmd.Stderr = os.Stderr
		return cmd.Run()
	},
}

func init() {
	rootCmd.AddCommand(mergeCmd)
}
