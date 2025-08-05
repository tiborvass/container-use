package cosmos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const debug = false

// Built with: docker build --progress plain --push -t tiborvass/claude-code --target claude-code -f environment/resources/Dockerfile.claude .
const agentImage = "tiborvass/claude-code@sha256:2dde950abcc8cfb1b0cf1a40fb8b6bc7745a5d0bed7a8571ccb3feec7481e84b"

// Cosmos handles one dockerized agent (e.g. Claude Code)
type Cosmos struct {
	Workdir          string
	SnapshotCallback func(string) error
	containerID      string
	proxyManager     net.Conn
	managerEncoder   *json.Encoder
}

// Run starts a Docker-based session with Claude Code
func (c *Cosmos) Run(ctx context.Context, worktree, envID string, claudeArgs []string) error {
	if err := c.ensureDockerContainer(ctx, worktree, envID); err != nil {
		return fmt.Errorf("failed to ensure container: %w", err)
	}

	go c.handleProxyMessages(ctx)

	return c.attachToDockerContainer(ctx, claudeArgs)
}

func (c *Cosmos) ensureDockerContainer(ctx context.Context, worktree, envID string) error {
	if c.containerID != "" {
		// Check if container still exists and is running
		cmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", c.containerID)
		if output, err := cmd.Output(); err == nil {
			running := strings.TrimSpace(string(output)) == "true"
			if running {
				return nil
			}
			out, err := exec.CommandContext(ctx, "docker", "start", c.containerID).CombinedOutput()
			if err == nil {
				return nil
			}
			return fmt.Errorf("could not start claude container: %w: %s", err, out)
		}
	}
	return c.createDockerContainer(ctx, worktree, envID)
}

func (c *Cosmos) createDockerContainer(ctx context.Context, worktree, envID string) error {
	imageName := "container-use:" + envID
	// Remove any existing container with same name
	containerName := "cu-" + envID
	exec.CommandContext(ctx, "docker", "rm", "-f", containerName).Run()

	out, err := exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("docker images --format '{{.Repository}}:{{.Tag}}' | grep -q %q", imageName)).CombinedOutput()
	if err != nil {
		if len(out) > 0 {
			return fmt.Errorf("could not check if %q already exists: %w: %s", imageName, err, out)
		}
		// image does not exist
		out, err = exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("docker pull %q && docker tag %q %q", agentImage, agentImage, imageName)).CombinedOutput()
		if err != nil {
			return fmt.Errorf("could not download claude code container: %w: %s", err, out)
		}
	}

	// Build docker run command
	args := []string{
		"run", "-d", "-it",
		"--init",
		"-P",
		"--name", containerName,
		"-h", containerName,
		"-w", worktree,
		"-v", fmt.Sprintf("%s:%s", worktree, worktree),
		"-e", "CU_ENVIRONMENT_ID=" + envID,
	}

	// Add environment variables
	// for _, envVar := range env.State.Config.Env {
	// 	args = append(args, "-e", envVar)
	// }

	// Add secrets as envvars.
	// TODO: use --env-file with pipe
	// for _, secret := range env.State.Config.Secrets {
	// 	k, v, _ := strings.Cut(secret, "=")
	// 	args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	// }

	// Handle Claude auth
	args = setupClaudeAuth(args)

	// TODO: cp ~/.claude/projects/$PROJECT into container. What about TODOS?

	// Merge the Claude image
	// TODO: check if exists already instead of reexporting
	// env.container().ExportImage(ctx, "container-use-claude")
	args = append(args, imageName)

	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create container: %w: %s", err, output)
	}

	c.containerID = strings.TrimSpace(string(output))
	// FIXME(tiborvass): not convinced about the different kinds of container IDs
	// env.State.Container = c.containerID

	output, err = exec.CommandContext(ctx, "docker", "port", c.containerID, "8042/tcp").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to get container port: %w: %s", err, output)
	}
	clientAddr := strings.TrimSpace(string(output))

	// Copy Claude config after container creation
	if err := c.copyClaudeConfig(ctx, envID); err != nil {
		slog.Warn("Failed to copy Claude config", "error", err)
	}

	// Wait for proxy to connect
	return c.waitForProxyConnection(ctx, clientAddr)
}

func (c *Cosmos) waitForProxyConnection(ctx context.Context, clientAddr string) (err error) {
	dialer := &net.Dialer{}
	maxRetries := 50
	backoff := 100 * time.Millisecond
	for range maxRetries {
		c.proxyManager, err = dialer.DialContext(ctx, "tcp", clientAddr)
		if err == nil {
			break
		}
		slog.Info(fmt.Sprintf("unable to connect to cosmos-manager: %v...", err), "container", c.containerID, "addr", clientAddr)
		time.Sleep(backoff)
	}
	if err != nil {
		return fmt.Errorf("failed to connect after %d seconds: %v", maxRetries*int(backoff/time.Second), err)
	}
	slog.Info("connected to client", "addr", c.proxyManager.RemoteAddr(), "container", c.containerID)
	return nil
}

func (c *Cosmos) handleProxyMessages(ctx context.Context) {
	dec := json.NewDecoder(c.proxyManager)

	for {
		var msg struct {
			Action string          `json:"Action"`
			Data   json.RawMessage `json:"Data"`
		}

		err := dec.Decode(&msg)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Error("Failed to decode proxy message", "error", err)
			}
			return
		}

		slog.Info("Received proxy message", "action", msg.Action)

		switch msg.Action {
		case "commit":
			var message string
			json.Unmarshal(msg.Data, &message)
			slog.Info("Processing docker commit", "message", message)

			// Create Docker snapshot
			imageID, err := c.commitDockerContainer(ctx, message)
			if err != nil {
				slog.Error("Failed to commit container", "error", err)
				continue
			}

			slog.Info("Docker commit done", "image", imageID)

			// env.Notes.Add("Snapshot created: %s (image: %s)", message, imageID[:12])

			// Trigger callback for git commit
			if c.SnapshotCallback != nil {
				if err := c.SnapshotCallback(message); err != nil {
					slog.Error("Failed to handle snapshot callback", "error", err)
				}
			}
		}
	}
}

func (c *Cosmos) commitDockerContainer(ctx context.Context, message string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "commit", "-m", message, c.containerID)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	imageID := strings.TrimSpace(string(output))

	// Track in state (reusing existing snapshot structure)
	// c.State.UpdatedAt = time.Now()

	return imageID, nil
}

func (c *Cosmos) attachToDockerContainer(ctx context.Context, claudeArgs []string) error {
	args := []string{"attach", c.containerID}

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

// setupClaudeAuth handles Claude authentication setup
func setupClaudeAuth(args []string) []string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return args
	}

	claudeDir := filepath.Join(homeDir, ".claude")
	credentialsFile := filepath.Join(claudeDir, ".credentials.json")

	if _, err := os.Stat(credentialsFile); err == nil {
		args = append(args, "-v", fmt.Sprintf("%s:/home/cu/.claude/.credentials.json", credentialsFile))
	}

	return args
}

func (c *Cosmos) copyClaudeConfig(ctx context.Context, envID string) error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// workdir := c.State.Config.Workdir
	config := map[string]any{}

	claudeJSONPath := filepath.Join(homeDir, ".claude.json")
	genClaudeJSONPath := filepath.Join(os.TempDir(), fmt.Sprintf("cu-%s-claude.json", envID))

	f, err := os.Open(claudeJSONPath)
	if err == nil {
		d := json.NewDecoder(f)
		defer f.Close()
		if err := d.Decode(&config); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	projects, ok := config["projects"].(map[string]any)
	if !ok {
		return fmt.Errorf("expected \"projects\" field of %q to be an object", claudeJSONPath)
	}
	if len(projects) == 0 {
		projects = map[string]any{}
	}
	if _, ok := projects[cwd]; !ok {
		projects[cwd] = map[string]any{}
	}
	project, ok := projects[cwd].(map[string]any)
	if !ok {
		return fmt.Errorf("expected projects[%q] field of %q to be an object", c.Workdir, claudeJSONPath)
	}
	if len(project) == 0 {
		project = map[string]any{
			// claude code needs these keys to not be `null`
			"allowedTools":           []any{},
			"history":                []any{},
			"mcpContextUris":         []any{},
			"mcpServers":             struct{}{},
			"enabledMcpjsonServers":  []any{},
			"disabledMcpjsonServers": []any{},
			// auto-trust the managed git worktree
			"hasTrustDialogAccepted": true,
		}
	}
	config["projects"] = map[string]any{
		c.Workdir: project,
	}

	f.Close()
	f, err = os.Create(genClaudeJSONPath)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	var w io.Writer = f
	if debug {
		w = io.MultiWriter(&buf, f)
	}
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	if err := e.Encode(config); err != nil {
		return err
	}
	f.Close()

	if debug {
		slog.Debug("generated .claude.json:", buf.String())
	}
	containerID := c.containerID
	out, err := exec.CommandContext(ctx, "docker", "cp", genClaudeJSONPath, fmt.Sprintf("%s:/home/cu/.claude.json", containerID)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not copy .claude.json: %w: %s", err, out)
	}
	out, err = exec.CommandContext(ctx, "docker", "exec", "-u", "root", containerID, "chown", "cu", "/home/cu/.claude.json").CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not chown .claude.json: %w: %s", err, out)
	}
	return nil
}
