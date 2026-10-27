// Package gitops reads and writes the GitOps repository with the git
// binary. Tack never handles a Git secret: the operator's credential
// helpers and ssh-agent apply.
package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/takodotid/tack/api/v1alpha1"
	"github.com/takodotid/tack/internal/engine"
)

var (
	// ErrUnreachable means git could not reach the repository.
	ErrUnreachable = errors.New("the Git repository is unreachable")

	// ErrNoMapping means a changed field has no spec.git.fields entry and
	// spec.git.find is not set.
	ErrNoMapping = errors.New("no Git mapping for the field")

	// errNoBranch means the remote answered without the branch.
	errNoBranch = errors.New("the branch does not exist")
)

var _ engine.Git = (*Repo)(nil)

// Repo implements engine.Git with the git binary. It keeps one clone per
// repository and branch in its work directory.
type Repo struct {
	workDir string

	mu      sync.Mutex
	ready   bool
	made    bool
	created []string
	clones  map[string]*clone

	// beforePush runs just before a push. Tests use it.
	beforePush func()
}

// clone is a local clone of one branch.
type clone struct {
	dir    string
	url    string
	branch string
	// trees maps a commit to the directory Checkout made for it.
	trees map[string]string
}

// New returns a Repo that works in workDir, a scratch directory. An empty
// workDir means the system's temporary directory.
func New(workDir string) *Repo {
	if workDir == "" {
		workDir = os.TempDir()
	}
	return &Repo{workDir: workDir, clones: map[string]*clone{}}
}

// Close removes every directory Repo created.
func (r *Repo) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for i := len(r.created) - 1; i >= 0; i-- {
		if err := os.RemoveAll(r.created[i]); err != nil {
			errs = append(errs, err)
		}
	}
	if r.made {
		if err := os.RemoveAll(r.workDir); err != nil {
			errs = append(errs, err)
		}
	}
	r.created, r.clones, r.ready, r.made = nil, map[string]*clone{}, false, false
	return errors.Join(errs...)
}

// mkdir creates a new directory in the work directory.
func (r *Repo) mkdir(prefix string) (string, error) {
	if !r.ready {
		_, err := os.Stat(r.workDir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.MkdirAll(r.workDir, 0o700); err != nil {
				return "", err
			}
			r.made = true
		case err != nil:
			return "", err
		}
		r.ready = true
	}
	d, err := os.MkdirTemp(r.workDir, prefix)
	if err != nil {
		return "", err
	}
	r.created = append(r.created, d)
	return d, nil
}

// clone returns the clone of the spec's branch, creating it empty.
func (r *Repo) clone(ctx context.Context, g *v1alpha1.GitSpec) (*clone, error) {
	if g.URL == "" || strings.HasPrefix(g.URL, "-") {
		return nil, fmt.Errorf("spec.git.url %q is not a repository URL", Redact(g.URL))
	}
	key := g.URL + "\x00" + g.Branch
	if c, ok := r.clones[key]; ok {
		return c, nil
	}
	if g.Branch == "" || strings.HasPrefix(g.Branch, "-") {
		return nil, fmt.Errorf("spec.git.branch %q is not a branch name", g.Branch)
	}
	if _, err := r.git(ctx, "", nil, "", "check-ref-format", "refs/heads/"+g.Branch); err != nil {
		return nil, fmt.Errorf("spec.git.branch %q is not a branch name", g.Branch)
	}
	dir, err := r.mkdir("repo-")
	if err != nil {
		return nil, err
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "core.autocrlf", "false"},
		{"remote", "add", "origin", g.URL},
	} {
		if _, err := r.git(ctx, dir, nil, "", args...); err != nil {
			return nil, err
		}
	}
	c := &clone{dir: dir, url: g.URL, branch: g.Branch, trees: map[string]string{}}
	r.clones[key] = c
	return c, nil
}

// fetch updates the clone from the remote branch and returns the branch's
// head. It returns an error that wraps ErrUnreachable when git cannot
// reach the remote, and errNoBranch when the branch does not exist.
func (r *Repo) fetch(ctx context.Context, c *clone) (string, error) {
	ref := "refs/remotes/origin/" + c.branch
	if _, err := r.git(ctx, c.dir, nil, "", "fetch", "--quiet", "--no-tags", "origin", "+refs/heads/"+c.branch+":"+ref); err != nil {
		var ge *gitError
		switch {
		case !errors.As(err, &ge):
			return "", err
		case strings.Contains(ge.stderr, "couldn't find remote ref"):
			return "", fmt.Errorf("%w: branch %s in %s", errNoBranch, c.branch, Redact(c.url))
		}
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return r.commitID(ctx, c.dir, ref)
}

// commitID returns the full id of the commit rev names in the clone.
func (r *Repo) commitID(ctx context.Context, dir, rev string) (string, error) {
	out, err := r.git(ctx, dir, nil, "", "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("commit %s not found", rev)
	}
	return strings.TrimSpace(out), nil
}

// reset makes the clone's working tree hold commit and nothing else.
func (r *Repo) reset(ctx context.Context, c *clone, commit string) error {
	if _, err := r.git(ctx, c.dir, nil, "", "checkout", "--quiet", "--force", "--detach", commit); err != nil {
		return err
	}
	_, err := r.git(ctx, c.dir, nil, "", "clean", "-ffdxq")
	return err
}

// files lists the regular files of commit under paths, or of the whole
// repository when paths is empty. Symbolic links and submodules are left out.
func (r *Repo) files(ctx context.Context, c *clone, commit string, paths []string) ([]string, error) {
	args := []string{"ls-tree", "-r", "-z", commit}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	out, err := r.git(ctx, c.dir, nil, "", args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, rec := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if ok && len(f) == 3 && f[1] == "blob" && (f[0] == "100644" || f[0] == "100755") {
			files = append(files, name)
		}
	}
	return files, nil
}

// readFile reads a regular file of the clone's working tree, which holds commit.
func (r *Repo) readFile(ctx context.Context, c *clone, commit, name string) ([]byte, error) {
	files, err := r.files(ctx, c, commit, []string{name})
	if err != nil {
		return nil, err
	}
	if !slices.Contains(files, name) {
		return nil, fmt.Errorf("file %s does not exist in commit %s, or is not a regular file", name, commit)
	}
	return os.ReadFile(filepath.Join(c.dir, filepath.FromSlash(name)))
}

// repoPath cleans a path of the repository. It must stay inside the repository.
func repoPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", fmt.Errorf("%q is not a path inside the repository", p)
	}
	return path.Clean(p), nil
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// gitError is a failed git command. Its text never holds credentials.
type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return Redact("git " + subcommand(e.args) + ": " + msg)
}

func (e *gitError) Unwrap() error { return e.err }

// subcommand returns the git command in args, after options such as -c.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}

// repoVars point git at another repository. Tack runs git in its own clones only.
var repoVars = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX",
}

// gitEnv is the environment of every git command: the operator's, without
// prompts, with English messages and with literal pathspecs.
func gitEnv(extra []string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(repoVars, k) || k == "GIT_TERMINAL_PROMPT" || k == "LC_ALL" || k == "GIT_LITERAL_PATHSPECS" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_LITERAL_PATHSPECS=1")
	return append(env, extra...)
}

// git runs git in dir and returns its standard output, also on failure.
func (r *Repo) git(ctx context.Context, dir string, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(env)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	switch {
	case err == nil:
		return stdout.String(), nil
	case ctx.Err() != nil:
		return stdout.String(), fmt.Errorf("git %s: %w", subcommand(args), ctx.Err())
	case errors.Is(err, exec.ErrNotFound):
		return "", errors.New("the git binary was not found in PATH")
	}
	return stdout.String(), &gitError{args: args, stderr: stderr.String(), err: err}
}

var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s]+@`)

// Redact hides the user and password of every URL in s.
func Redact(s string) string { return userinfo.ReplaceAllString(s, "${1}***@") }

// describe names a field for a message.
func describe(site string, obj v1alpha1.ObjectRef, field string) string {
	name := obj.Name
	if obj.Namespace != "" {
		name = obj.Namespace + "/" + obj.Name
	}
	return fmt.Sprintf("site %s, %s %s %s, field %s", site, obj.APIVersion, obj.Kind, name, field)
}

func describeChange(c *v1alpha1.GitChange) string { return describe(c.Site, c.Object, c.Field) }

func docString(d *int32) string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf(" document %d", *d)
}
