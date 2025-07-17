package main

import (
	"context"
	"log/slog"
	"os"

	"dagger.io/dagger"
	"github.com/dagger/container-use/environment"
	"github.com/dagger/container-use/mcpserver"
	"github.com/spf13/cobra"
)

var stdioCmd = &cobra.Command{
	Use:   "stdio",
	Short: "Start MCP server for agent integration",
	Long:  `Start the Model Context Protocol server that enables AI agents to create and manage containerized environments. This is typically used by agents like Claude Code, Cursor, or VSCode.`,
	RunE: func(app *cobra.Command, _ []string) error {
		ctx := app.Context()

		slog.Info("connecting to dagger")

		dag, err := dagger.Connect(ctx, dagger.WithLogOutput(logWriter))
		if err != nil {
			slog.Error("Error starting dagger", "error", err)

			if isDockerDaemonError(err) {
				handleDockerDaemonError()
			}

			os.Exit(1)
		}
		defer dag.Close()

		go warmCache(ctx, dag)

		stdin, stdout := os.Stdin, os.Stdout
		if v := os.Getenv("CU_STDIN"); v != "" {
			stdin, err = os.Open(v)
			if err != nil {
				return err
			}
		}
		if v := os.Getenv("CU_STDOUT"); v != "" {
			stdout, err = os.OpenFile(v, os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
		}
		return mcpserver.RunStdioServer(ctx, dag, stdin, stdout)
	},
}

func warmCache(ctx context.Context, dag *dagger.Client) {
	environment.EditUtil(dag).Sync(ctx)
	environment.GrepUtil(dag).Sync(ctx)
}

func init() {
	rootCmd.AddCommand(stdioCmd)
}
