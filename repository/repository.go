package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"dagger.io/dagger"
	"github.com/dagger/container-use/environment"
	petname "github.com/dustinkirkland/golang-petname"
)

const (
	cuGlobalConfigPath = "~/.config/container-use"
	cuRepoPath         = cuGlobalConfigPath + "/repos"
	cuWorktreePath     = cuGlobalConfigPath + "/worktrees"
	containerUseRemote = "container-use"
	gitNotesLogRef     = "container-use"
	gitNotesStateRef   = "container-use-state"
)

type Repository struct {
	userRepoPath string
	forkRepoPath string
	basePath     string // defaults to ~/.config/container-use if empty
}

// getRepoPath returns the path for storing repository data
func (r *Repository) getRepoPath() string {
	return filepath.Join(r.basePath, "repos")
}

// getWorktreePath returns the path for storing worktrees
func (r *Repository) getWorktreePath() string {
	return filepath.Join(r.basePath, "worktrees")
}

func Open(ctx context.Context, repo string) (*Repository, error) {
	return OpenWithBasePath(ctx, repo, cuGlobalConfigPath)
}

// OpenWithBasePath opens a repository with a custom base path for container-use data.
// This is useful for tests that need isolated environments.
func OpenWithBasePath(ctx context.Context, repo string, basePath string) (*Repository, error) {
	output, err := RunGitCommand(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		// Check for exit code 128 which means not a git repository
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			return nil, errors.New("you must be in a git repository to use container-use")
		}
		return nil, err
	}
	userRepoPath := strings.TrimSpace(output)

	forkRepoPath, err := getContainerUseRemote(ctx, userRepoPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// Create a temporary repository to get the normalized fork path
		tempRepo := &Repository{basePath: basePath}
		forkRepoPath, err = tempRepo.normalizeForkPath(ctx, userRepoPath)
		if err != nil {
			return nil, err
		}
	}

	r := &Repository{
		userRepoPath: userRepoPath,
		forkRepoPath: forkRepoPath,
		basePath:     basePath,
	}

	if err := r.ensureFork(ctx); err != nil {
		return nil, fmt.Errorf("unable to fork the repository: %w", err)
	}
	if err := r.ensureUserRemote(ctx); err != nil {
		return nil, fmt.Errorf("unable to set container-use remote: %w", err)
	}

	return r, nil
}

func (r *Repository) ensureFork(ctx context.Context) error {
	// Make sure the fork repo path exists, otherwise create it
	_, err := os.Stat(r.forkRepoPath)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}

	slog.Info("Initializing local remote", "user-repo", r.userRepoPath, "fork-repo", r.forkRepoPath)
	if err := os.MkdirAll(r.forkRepoPath, 0755); err != nil {
		return err
	}
	_, err = RunGitCommand(ctx, r.forkRepoPath, "init", "--bare")
	if err != nil {
		return err
	}
	return nil
}

func (r *Repository) ensureUserRemote(ctx context.Context) error {
	currentForkPath, err := getContainerUseRemote(ctx, r.userRepoPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_, err := RunGitCommand(ctx, r.userRepoPath, "remote", "add", containerUseRemote, r.forkRepoPath)
		return err
	}

	if currentForkPath != r.forkRepoPath {
		_, err := RunGitCommand(ctx, r.userRepoPath, "remote", "set-url", containerUseRemote, r.forkRepoPath)
		return err
	}

	return nil
}

func (r *Repository) SourcePath() string {
	return r.userRepoPath
}

func (r *Repository) exists(ctx context.Context, id string) error {
	if _, err := RunGitCommand(ctx, r.forkRepoPath, "rev-parse", "--verify", id); err != nil {
		if strings.Contains(err.Error(), "Needed a single revision") {
			return fmt.Errorf("environment %q not found", id)
		}
		return err
	}
	return nil
}

// Create creates a new environment with the given description and explanation.
func (r *Repository) Create(ctx context.Context, dag *dagger.Client, description, explanation string, cosmos bool) (*environment.Environment, error) {
	id := petname.Generate(2, "-")
	worktree, err := r.initializeWorktree(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := r.createInitialCommit(ctx, worktree, id, description); err != nil {
		return nil, fmt.Errorf("failed to create initial commit: %w", err)
	}

	var agentify func(*dagger.Container) *dagger.Container
	config := environment.DefaultConfig()
	if cosmos {
		config.Workdir = worktree
		agentify = func(container *dagger.Container) *dagger.Container {
			return container.
				WithExec([]string{"sh", "-c", "apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/* && apt-get clean"}).
				// Efficient MergeOp
				WithDirectory("/",
					dag.Container().
						From("tiborvass/claude-code:layer@sha256:c25e0e5d1a996615c74b41014c030dcee43ee8cf124929201778d821d8897f2f").
						Rootfs().
						// TODO: remove this, and forward logs to container-use binary
						WithNewDirectory("/cosmos", dagger.DirectoryWithNewDirectoryOpts{Permissions: 0755}),
				).
				WithEnvVariable("ANTHROPIC_BASE_URL", "http://localhost:8080").
				// claude code doesn't like to be root
				WithExec([]string{"useradd", "-ms", "/bin/bash", "cu"}, dagger.ContainerWithExecOpts{NoInit: true}).
				WithDirectory("/home/cu/.claude", dag.Directory().WithNewDirectory(".claude")).
				WithExec([]string{"chown", "-R", "cu:cu", "/usr/local/bin/container-use-proxy", "/home/cu", "/cosmos"}, dagger.ContainerWithExecOpts{NoInit: true}).
				WithUser("cu").
				// healthcheck is currently done by client binary
				WithExposedPort(8042, dagger.ContainerWithExposedPortOpts{ExperimentalSkipHealthcheck: true}).
				WithEntrypoint([]string{"/usr/local/bin/container-use-proxy"})
		}
	}

	worktreeHead, err := RunGitCommand(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	worktreeHead = strings.TrimSpace(worktreeHead)

	baseSourceDir, err := dag.
		Host().
		Directory(r.forkRepoPath, dagger.HostDirectoryOpts{NoCache: true}). // bust cache for each Create call
		AsGit().
		Ref(worktreeHead).
		Tree(dagger.GitRefTreeOpts{DiscardGitDir: true}).
		Sync(ctx) // don't bust cache when loading from state
	if err != nil {
		return nil, fmt.Errorf("failed loading initial source directory: %w", err)
	}

	env, err := environment.New(ctx, dag, id, description, config, baseSourceDir, agentify)
	if err != nil {
		return nil, err
	}

	if err := r.propagateToWorktree(ctx, env, explanation); err != nil {
		return nil, err
	}

	return env, nil
}

// Get retrieves an Environment for container operations.
func (r *Repository) Get(ctx context.Context, dag *dagger.Client, id string) (*environment.Environment, error) {
	if err := r.exists(ctx, id); err != nil {
		return nil, err
	}

	worktree, err := r.initializeWorktree(ctx, id)
	if err != nil {
		return nil, err
	}

	state, err := r.loadState(ctx, worktree)
	if err != nil {
		return nil, err
	}

	env, err := environment.Load(ctx, dag, id, state, worktree)
	if err != nil {
		return nil, err
	}

	return env, nil
}

// This is more efficient than Get() when you only need access to configuration,
// state, and other metadata without performing container operations.
func (r *Repository) Info(ctx context.Context, id string) (*environment.EnvironmentInfo, error) {
	if err := r.exists(ctx, id); err != nil {
		return nil, err
	}

	worktree, err := r.initializeWorktree(ctx, id)
	if err != nil {
		return nil, err
	}

	state, err := r.loadState(ctx, worktree)
	if err != nil {
		return nil, err
	}

	envInfo, err := environment.LoadInfo(ctx, id, state, worktree)
	if err != nil {
		return nil, err
	}

	return envInfo, nil
}

// List returns information about all environments in the repository.
// Returns EnvironmentInfo slice avoiding dagger client initialization.
// Use Get() on individual environments when you need full Environment with container operations.
func (r *Repository) List(ctx context.Context) ([]*environment.EnvironmentInfo, error) {
	branches, err := RunGitCommand(ctx, r.forkRepoPath, "branch", "--format", "%(refname:short)")
	if err != nil {
		return nil, err
	}

	envs := []*environment.EnvironmentInfo{}
	for branch := range strings.SplitSeq(branches, "\n") {
		branch = strings.TrimSpace(branch)

		// FIXME(aluzzardi): This is a hack to make sure the branch is actually an environment.
		// There must be a better way to do this.
		worktree, err := r.WorktreePath(branch)
		if err != nil {
			return nil, err
		}
		state, err := r.loadState(ctx, worktree)
		if err != nil || state == nil {
			continue
		}

		envInfo, err := r.Info(ctx, branch)
		if err != nil {
			return nil, err
		}

		envs = append(envs, envInfo)
	}

	// Sort by most recently updated environments first
	sort.Slice(envs, func(i, j int) bool {
		return envs[i].State.UpdatedAt.After(envs[j].State.UpdatedAt)
	})

	return envs, nil
}

// ListDescendantEnvironments returns environments that are descendants of the given commit
func (r *Repository) ListDescendantEnvironments(ctx context.Context, baseCommit string) ([]*environment.EnvironmentInfo, error) {
	allEnvs, err := r.List(ctx)
	if err != nil {
		return nil, err
	}

	var filteredEnvs []*environment.EnvironmentInfo
	for _, env := range allEnvs {
		// Check if this environment's branch contains the base commit
		_, err := RunGitCommand(ctx, r.userRepoPath, "merge-base", "--is-ancestor", baseCommit, fmt.Sprintf("%s/%s", containerUseRemote, env.ID))
		if err == nil {
			// Exit code 0 means baseCommit is an ancestor
			filteredEnvs = append(filteredEnvs, env)
		} else {
			// Check the actual error - non-zero exit just means it's not an ancestor
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				// Not an ancestor, skip
				continue
			} else {
				// Real error
				slog.Debug("Error checking ancestry", "env", env.ID, "error", err)
			}
		}
	}

	// Sort by update time (most recent first)
	sort.Slice(filteredEnvs, func(i, j int) bool {
		return filteredEnvs[i].State.UpdatedAt.After(filteredEnvs[j].State.UpdatedAt)
	})

	return filteredEnvs, nil
}

// Update saves the provided environment to the repository.
// Writes configuration and source code changes to the worktree and history + state to git notes.
func (r *Repository) Update(ctx context.Context, env *environment.Environment, explanation string) error {
	if err := r.propagateToWorktree(ctx, env, explanation); err != nil {
		return err
	}
	if note := env.Notes.Pop(); note != "" {
		return r.addGitNote(ctx, env, note)
	}

	return nil
}

// Delete removes an environment from the repository.
func (r *Repository) Delete(ctx context.Context, id string) error {
	if err := r.exists(ctx, id); err != nil {
		return err
	}

	if err := r.deleteWorktree(id); err != nil {
		return err
	}
	if err := r.deleteLocalRemoteBranch(id); err != nil {
		return err
	}
	return nil
}

// Checkout changes the user's current branch to that of the identified environment.
// It attempts to get the most recent commit from the environment without discarding any user changes.
func (r *Repository) Checkout(ctx context.Context, id, branch string) (string, error) {
	if err := r.exists(ctx, id); err != nil {
		return "", err
	}

	if branch == "" {
		branch = "cu-" + id
	}

	// set up remote tracking branch if it's not already there
	_, err := RunGitCommand(ctx, r.userRepoPath, "show-ref", "--verify", "--quiet", fmt.Sprintf("refs/heads/%s", branch))
	localBranchExists := err == nil
	if !localBranchExists {
		_, err = RunGitCommand(ctx, r.userRepoPath, "branch", "--track", branch, fmt.Sprintf("%s/%s", containerUseRemote, id))
		if err != nil {
			return "", err
		}
	}

	_, err = RunGitCommand(ctx, r.userRepoPath, "checkout", branch)
	if err != nil {
		return "", err
	}

	if localBranchExists {
		remoteRef := fmt.Sprintf("%s/%s", containerUseRemote, id)

		counts, err := RunGitCommand(ctx, r.userRepoPath, "rev-list", "--left-right", "--count", fmt.Sprintf("HEAD...%s", remoteRef))
		if err != nil {
			return branch, err
		}

		parts := strings.Split(strings.TrimSpace(counts), "\t")
		if len(parts) != 2 {
			return branch, fmt.Errorf("unexpected git rev-list output: %s", counts)
		}
		aheadCount, behindCount := parts[0], parts[1]

		if behindCount != "0" && aheadCount == "0" {
			_, err = RunGitCommand(ctx, r.userRepoPath, "merge", "--ff-only", remoteRef)
			if err != nil {
				return branch, err
			}
		} else if behindCount != "0" {
			return branch, fmt.Errorf("switched to %s, but %s is %s ahead and container-use/ remote has %s additional commits", branch, branch, aheadCount, behindCount)
		}
	}

	return branch, err
}

func (r *Repository) Log(ctx context.Context, id string, patch bool, w io.Writer) error {
	envInfo, err := r.Info(ctx, id)
	if err != nil {
		return err
	}

	logArgs := []string{
		"log",
		fmt.Sprintf("--notes=%s", gitNotesLogRef),
	}

	if patch {
		logArgs = append(logArgs, "--patch")
	} else {
		logArgs = append(logArgs, "--format=%C(yellow)%h%Creset  %s %Cgreen(%cr)%Creset %+N")
	}

	revisionRange, err := r.revisionRange(ctx, envInfo)
	if err != nil {
		return err
	}

	logArgs = append(logArgs, revisionRange)

	return RunInteractiveGitCommand(ctx, r.userRepoPath, w, logArgs...)
}

func (r *Repository) Diff(ctx context.Context, id string, w io.Writer) error {
	envInfo, err := r.Info(ctx, id)
	if err != nil {
		return err
	}

	diffArgs := []string{
		"diff",
	}

	revisionRange, err := r.revisionRange(ctx, envInfo)
	if err != nil {
		return err
	}

	diffArgs = append(diffArgs, revisionRange)

	return RunInteractiveGitCommand(ctx, r.userRepoPath, w, diffArgs...)
}

func (r *Repository) Merge(ctx context.Context, id string, w io.Writer) error {
	envInfo, err := r.Info(ctx, id)
	if err != nil {
		return err
	}

	return RunInteractiveGitCommand(ctx, r.userRepoPath, w, "merge", "--no-ff", "--autostash", "-m", "Merge environment "+envInfo.ID, "--", "container-use/"+envInfo.ID)
}

func (r *Repository) Apply(ctx context.Context, id string, w io.Writer) error {
	envInfo, err := r.Info(ctx, id)
	if err != nil {
		return err
	}

	return RunInteractiveGitCommand(ctx, r.userRepoPath, w, "merge", "--autostash", "--squash", "--", "container-use/"+envInfo.ID)
}
