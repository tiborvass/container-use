package environment

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// EnsureClaudeImage ensures the Claude Docker image is built
func EnsureClaudeImage(ctx context.Context) error {
	// Check if image already exists
	cmd := exec.CommandContext(ctx, "docker", "images", "-q", "container-use-claude")
	output, err := cmd.Output()
	if err == nil && len(output) > 0 {
		// Image already exists
		return nil
	}

	// Build the image
	resourcesPath := getResourcesPath()
	dockerfilePath := filepath.Join(resourcesPath, "Dockerfile.claude")

	// First, build the proxy binary for Linux
	if err := buildProxyBinary(ctx); err != nil {
		return fmt.Errorf("failed to build proxy binary: %w", err)
	}

	// Build Docker image
	buildArgs := []string{
		"build",
		"-t", "container-use-claude",
		"-f", dockerfilePath,
		resourcesPath,
	}

	cmd = exec.CommandContext(ctx, "docker", buildArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build Claude image: %w", err)
	}

	return nil
}

func buildProxyBinary(ctx context.Context) error {
	// Build the proxy binary for Linux
	cmd := exec.CommandContext(ctx, "go", "build", 
		"-o", filepath.Join(getResourcesPath(), "container-use-proxy-linux"),
		"./cmd/container-use-proxy")
	
	cmd.Env = append(os.Environ(), 
		"GOOS=linux",
		"GOARCH=amd64",
	)

	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to build proxy: %w\n%s", err, output)
	}

	return nil
}

func getResourcesPath() string {
	// Get the path to the resources directory
	// This assumes we're running from the repository root
	return "environment/resources"
}