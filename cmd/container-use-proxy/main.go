package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dagger/container-use/internal/proxy"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Get manager address from environment
	managerAddr := os.Getenv("MANAGER_ADDR")
	log.Printf("Manager address from env: %s", managerAddr)

	// Start proxy server
	proxyServer, err := proxy.Start(ctx, "localhost:8080", managerAddr)
	if err != nil {
		log.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Shutdown(ctx)

	// Run Claude
	claudeArgs := []string{"claude"}

	// Check for CLAUDE_ARGS environment variable (used when resuming)
	if envArgs := os.Getenv("CLAUDE_ARGS"); envArgs != "" {
		// Parse the environment args
		claudeArgs = append(claudeArgs, strings.Fields(envArgs)...)
	} else {
		// Use command line args
		claudeArgs = append(claudeArgs, os.Args[1:]...)
	}

	claudeCmd := exec.CommandContext(ctx, claudeArgs[0], claudeArgs[1:]...)
	claudeCmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL=http://localhost:8080")
	claudeCmd.Stdin = os.Stdin
	claudeCmd.Stdout = os.Stdout
	claudeCmd.Stderr = os.Stderr

	if err := claudeCmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("Claude failed: %v", err)
	}
}
