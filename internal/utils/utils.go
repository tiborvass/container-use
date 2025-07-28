package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// M panics on error
func M(err error) {
	if err != nil {
		panic(err)
	}
}

// M2 returns the first value and panics on error
func M2[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// R runs a shell command and returns the output
func R(ctx context.Context, format string, args ...interface{}) string {
	cmdStr := fmt.Sprintf(format, args...)
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	output, err := cmd.Output()
	if err != nil {
		panic(fmt.Errorf("command failed: %s: %w", cmdStr, err))
	}
	return strings.TrimSpace(string(output))
}

// NoEOF returns any error except io.EOF
func NoEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// Defer is used for deferred error handling
func Defer(err error) error {
	if r := recover(); r != nil {
		if e, ok := r.(error); ok {
			if err != nil {
				return fmt.Errorf("%w: %v", err, e)
			}
			return e
		}
		return fmt.Errorf("panic: %v", r)
	}
	return err
}