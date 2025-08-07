package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dagger/container-use/internal/proxy"
)

func handleProxyMessages(ctx context.Context, managerConn net.Conn) error {
	dec := json.NewDecoder(managerConn)

	for {
		var msg struct {
			Action string          `json:"Action"`
			Data   json.RawMessage `json:"Data"`
		}

		err := dec.Decode(&msg)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("Failed to decode proxy message: %w", err)
			}
			return nil
		}

		slog.Info("Received proxy message", "action", msg.Action)

		switch msg.Action {
		case "commit":
			var message string
			json.Unmarshal(msg.Data, &message)
			slog.Info("Processing commit", "message", message)

			// Create Docker snapshot
			imageID, err := commitDockerContainer(ctx, message)
			if err != nil {
				slog.Error("Failed to commit container", "error", err)
				continue
			}

			_ = imageID

			// env.Notes.Add("Snapshot created: %s (image: %s)", message, imageID[:12])

			// Trigger callback for git commit
			// if env.dockerBackend.snapshotCallback != nil {
			// if err := env.dockerBackend.snapshotCallback(message); err != nil {
			// slog.Error("Failed to handle snapshot callback", "error", err)
			// }
			// }
		}
	}
}

func commitDockerContainer(ctx context.Context, message string) (string, error) {
	return "", nil
}

func proxyHelper(ctx context.Context) {
	l, err := net.Listen("unix", os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	unixConn, err := l.Accept()
	if err != nil {
		log.Fatal(err)
	}
	handleProxyMessages(ctx, unixConn)
	log.Println("exiting proxy helper")
	return

	/*
		dialer := &net.Dialer{}
		maxRetries := 50
		backoff := 100 * time.Millisecond
		// var unixConn net.Conn
		// var err error
		for range maxRetries {
			unixConn, err = dialer.DialContext(ctx, "unix", os.Args[1])
			if err == nil {
				break
			}
			log.Printf("unable to connect to cosmos-manager: %v...", err)
			time.Sleep(backoff)
		}
		if err != nil {
			log.Printf("failed to connect after %d seconds: %v", maxRetries*int(backoff/time.Second), err)
		}
		handleProxyMessages(ctx, unixConn)
		// go io.Copy(tcpConn, unixConn)
		// io.Copy(unixConn, tcpConn)
		log.Println("exiting proxy helper")
	*/
}

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

	if os.Getenv("CU_PROXY_HELPER") != "" {
		log.Print("proxy helper")
		proxyHelper(ctx)
		return
	}
	log.Print("main proxy")

	envID := os.Getenv("CU_ENVIRONMENT_ID")
	if envID == "" {
		log.Fatal("expected CU_ENVIRONMENT_ID to be set")
	}

	// Start proxy server
	cosmosSockPath := fmt.Sprintf("/sockets-%s/cosmos.sock", envID)
	proxyServer, err := proxy.Start(ctx, "localhost:8080", cosmosSockPath)
	if err != nil {
		log.Fatalf("Failed to start proxy: %v", err)
	}
	defer proxyServer.Shutdown(ctx)

	// Run Claude
	claudeArgs := []string{"claude", "--dangerously-skip-permissions"}

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
		log.Fatalf("Running claude code failed: %v", err)
	}
}
