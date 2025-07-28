package environment

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DockerBackend provides Docker-based environment operations
type DockerBackend struct {
	containerID      string
	proxyManager     net.Conn
	managerEncoder   *json.Encoder
	snapshotCallback func(string) error
}

// StartDockerSession starts a Docker-based session for Claude
func (env *Environment) StartDockerSession(ctx context.Context, worktree string, claudeArgs []string) error {
	if env.dockerBackend == nil {
		env.dockerBackend = &DockerBackend{}
	}

	// Ensure container exists
	if err := env.ensureDockerContainer(ctx, worktree); err != nil {
		return fmt.Errorf("failed to ensure container: %w", err)
	}

	return env.attachToDockerContainer(ctx, claudeArgs)
}

func (env *Environment) ensureDockerContainer(ctx context.Context, worktree string) error {
	backend := env.dockerBackend
	
	if backend.containerID != "" {
		// Check if container still exists and is running
		cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", backend.containerID)
		if output, err := cmd.Output(); err == nil {
			running := strings.TrimSpace(string(output)) == "true"
			if running {
				return nil
			}
			// Try to start it
			if err := exec.CommandContext(ctx, "docker", "start", backend.containerID).Run(); err == nil {
				return nil
			}
		}
	}

	// Create new container
	return env.createDockerContainer(ctx, worktree)
}

func (env *Environment) createDockerContainer(ctx context.Context, worktree string) error {
	backend := env.dockerBackend
	
	// Ensure Claude image is built
	if err := EnsureClaudeImage(ctx); err != nil {
		return fmt.Errorf("failed to ensure Claude image: %w", err)
	}
	
	// Remove any existing container with same name
	containerName := fmt.Sprintf("cu-%s", env.ID)
	exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()

	// Start manager server before creating container
	managerAddr, err := env.startManagerServer(ctx)
	if err != nil {
		return fmt.Errorf("failed to start manager server: %w", err)
	}

	// Build docker run command
	args := []string{
		"run", "-d", "-it",
		"--name", containerName,
		"-h", containerName,
		"-w", env.State.Config.Workdir,
		"-v", fmt.Sprintf("%s:%s", worktree, env.State.Config.Workdir),
		"-e", "CU_ENVIRONMENT_ID=" + env.ID,
		"-e", "MANAGER_ADDR=" + managerAddr,
	}

	// Add environment variables
	for _, envVar := range env.State.Config.Env {
		args = append(args, "-e", envVar)
	}

	// Add secrets
	for _, secret := range env.State.Config.Secrets {
		k, v, _ := strings.Cut(secret, "=")
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	// Handle Claude auth
	args = setupClaudeAuth(args)

	// Use the Claude image
	args = append(args, "container-use-claude")

	output, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	backend.containerID = strings.TrimSpace(string(output))
	env.State.Container = backend.containerID

	// Copy Claude config after container creation
	if err := env.copyClaudeConfig(ctx); err != nil {
		slog.Warn("Failed to copy Claude config", "error", err)
	}

	// Wait for proxy to connect
	return env.waitForProxyConnection(ctx)
}

func (env *Environment) startManagerServer(ctx context.Context) (string, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return "", fmt.Errorf("failed to create listener: %w", err)
	}

	addr := listener.Addr().String()
	slog.Info("Manager server listening", "addr", addr)

	// On macOS, Docker containers need to use host.docker.internal
	if runtime.GOOS == "darwin" {
		_, port, _ := net.SplitHostPort(addr)
		addr = fmt.Sprintf("host.docker.internal:%s", port)
	}

	// Accept connection in background
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			slog.Error("Failed to accept proxy connection", "error", err)
			return
		}
		slog.Info("Proxy connected", "remote", conn.RemoteAddr())

		env.mu.Lock()
		env.dockerBackend.proxyManager = conn
		env.dockerBackend.managerEncoder = json.NewEncoder(conn)
		env.mu.Unlock()

		// Handle messages from proxy
		env.handleProxyMessages(ctx)
	}()

	return addr, nil
}

func (env *Environment) waitForProxyConnection(ctx context.Context) error {
	for i := 0; i < 30; i++ {
		env.mu.RLock()
		connected := env.dockerBackend != nil && env.dockerBackend.proxyManager != nil
		env.mu.RUnlock()

		if connected {
			slog.Info("Proxy connected successfully")
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("proxy failed to connect after 15 seconds")
}

func (env *Environment) handleProxyMessages(ctx context.Context) {
	dec := json.NewDecoder(env.dockerBackend.proxyManager)

	for {
		var msg struct {
			Action string          `json:"Action"`
			Data   json.RawMessage `json:"Data"`
		}

		if err := dec.Decode(&msg); err != nil {
			slog.Error("Failed to decode proxy message", "error", err)
			return
		}

		slog.Info("Received proxy message", "action", msg.Action)

		switch msg.Action {
		case "commit":
			var message string
			json.Unmarshal(msg.Data, &message)
			slog.Info("Processing commit", "message", message)

			// Create Docker snapshot
			imageID, err := env.commitDockerContainer(ctx, message)
			if err != nil {
				slog.Error("Failed to commit container", "error", err)
				continue
			}

			env.Notes.Add("Snapshot created: %s (image: %s)", message, imageID[:12])

			// Trigger callback for git commit
			if env.dockerBackend.snapshotCallback != nil {
				if err := env.dockerBackend.snapshotCallback(message); err != nil {
					slog.Error("Failed to handle snapshot callback", "error", err)
				}
			}
		}
	}
}

func (env *Environment) commitDockerContainer(ctx context.Context, message string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "commit", "-m", message, env.dockerBackend.containerID)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	imageID := strings.TrimSpace(string(output))

	// Track in state (reusing existing snapshot structure)
	env.State.UpdatedAt = time.Now()

	return imageID, nil
}

func (env *Environment) attachToDockerContainer(ctx context.Context, claudeArgs []string) error {
	args := []string{"attach", env.dockerBackend.containerID}

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Pass claude args via environment
	if len(claudeArgs) > 0 {
		cmd.Env = append(os.Environ(), fmt.Sprintf("CLAUDE_ARGS=%s", strings.Join(claudeArgs, " ")))
	}

	return cmd.Run()
}

// SetDockerSnapshotCallback sets the callback for when Docker snapshots are created
func (env *Environment) SetDockerSnapshotCallback(callback func(string) error) {
	env.mu.Lock()
	defer env.mu.Unlock()
	
	if env.dockerBackend == nil {
		env.dockerBackend = &DockerBackend{}
	}
	env.dockerBackend.snapshotCallback = callback
}

// setupClaudeAuth handles Claude authentication setup
func setupClaudeAuth(args []string) []string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return args
	}

	claudeDir := filepath.Join(homeDir, ".claude")
	credentialsFile := filepath.Join(claudeDir, ".credentials.json")

	if _, err := os.Stat(credentialsFile); err == nil {
		args = append(args, "-v", fmt.Sprintf("%s:/home/cosmos/.claude", claudeDir))
	}

	return args
}

func (env *Environment) copyClaudeConfig(ctx context.Context) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	claudeJSON := filepath.Join(homeDir, ".claude.json")
	if _, err := os.Stat(claudeJSON); err != nil {
		return nil // No config to copy
	}

	// Copy using docker cp
	containerID := env.dockerBackend.containerID
	cmd := exec.CommandContext(ctx, "docker", "cp", claudeJSON, fmt.Sprintf("%s:/home/cosmos/.claude.json", containerID))
	return cmd.Run()
}