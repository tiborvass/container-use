package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch git log output",
	Long:  `Watch git log output using the watch command: 'watch --color -n1 git log --color=always --remotes=container-use --oneline --graph --decorate'.`,
	RunE: func(app *cobra.Command, _ []string) error {
		// check if the watch command is available
		if _, err := exec.LookPath("watch"); err != nil {
			return fmt.Errorf("the 'watch' command is not available. Please install it to use this feature: %w", err)
		}

		cmd := exec.CommandContext(app.Context(), "watch", "--color", "-n1", "git", "log", "--color=always", "--remotes=container-use", "--oneline", "--graph", "--decorate")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin

		return cmd.Run()
	},
}

func init() {
	rootCmd.AddCommand(watchCmd)
}
