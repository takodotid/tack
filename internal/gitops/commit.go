package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	failoverv1 "github.com/takodotid/tack/api/v1"
	"github.com/takodotid/tack/internal/driver"
	"github.com/takodotid/tack/internal/engine"
	"github.com/takodotid/tack/internal/stop"
)

// trailer is the commit trailer that names the run of a commit.
const trailer = "Tack-Run"

// Commit writes the changes in one commit on top of req.Base and pushes it
// to the branch, without force. It returns the commit. When the branch
// head is the run's own commit on top of req.Base, as after an interrupted
// run, it returns that commit. When the changes change nothing, it returns
// the base and pushes nothing.
func (r *Repo) Commit(ctx context.Context, w *failoverv1.WorkloadSpec, req engine.CommitRequest) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.Run == "" || strings.ContainsAny(req.Run, "\r\n") || strings.TrimSpace(req.Run) != req.Run {
		return "", fmt.Errorf("run name %q cannot be a commit trailer", req.Run)
	}
	if !commitRE.MatchString(req.Base) {
		return "", fmt.Errorf("base %q is not a commit id", req.Base)
	}
	if strings.TrimSpace(req.Message) == "" {
		return "", errors.New("the commit message is empty")
	}
	g := &w.Git
	c, err := r.clone(ctx, g)
	if err != nil {
		return "", err
	}
	head, err := r.fetch(ctx, c)
	switch {
	case errors.Is(err, ErrUnreachable):
		return "", stop.Wait(failoverv1.ReasonGitUnreachable, "%v", err)
	case errors.Is(err, errNoBranch):
		return "", stop.Ask(failoverv1.ReasonGitChanged, "%v", err)
	case err != nil:
		return "", err
	}
	base := req.Base
	if id, err := r.commitID(ctx, c.dir, base); err == nil {
		base = id
	}
	if head != base {
		if r.ranBy(ctx, c, head, base, req.Run) {
			return head, nil
		}
		return "", stop.AskHead(failoverv1.ReasonGitChanged, head,
			"branch %s of %s is at %s, but the run's base is %s: Git changed during the run", c.branch, Redact(c.url), head, req.Base)
	}

	if err := r.reset(ctx, c, base); err != nil {
		return "", err
	}
	changes := make([]failoverv1.GitChange, len(req.Changes))
	var f *finder
	for i := range req.Changes {
		changes[i] = *req.Changes[i].DeepCopy()
		if changes[i].File != "" {
			if err := r.checkTarget(ctx, c, base, g, &changes[i]); err != nil {
				return "", err
			}
			continue
		}
		if f == nil {
			f = r.newFinder(c, base, g.Find)
		}
		if err := f.resolve(ctx, &changes[i]); err != nil {
			return "", err
		}
	}
	files, err := r.apply(ctx, c, base, changes)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return base, nil
	}

	if _, err := r.git(ctx, c.dir, nil, "", append([]string{"add", "--"}, files...)...); err != nil {
		return "", err
	}
	if err := r.commit(ctx, c, g.Author, req); err != nil {
		return "", err
	}
	commit, err := r.commitID(ctx, c.dir, "HEAD")
	if err != nil {
		return "", err
	}
	if r.beforePush != nil {
		r.beforePush()
	}
	if err := r.push(ctx, c, commit); err != nil {
		return "", err
	}
	return commit, nil
}

// checkTarget refuses a change whose file, path and document are not where
// the Workload's mapping puts its field, or, without a mapping, not the
// field's path in a document of commit that defines the change's object
// ([RFC#0006] Requirement 15). The ledger records them, and anyone who may
// write a ledger could point a removal or a value at any file (#35 row 11).
func (r *Repo) checkTarget(ctx context.Context, c *clone, commit string, g *failoverv1.GitSpec, ch *failoverv1.GitChange) error {
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("%s: the run would write %s%s at %s, but %s. The ledger's Git step does not match its Workload: do not resume the run, see docs/runbooks/abandon-run.md",
			describeChange(ch), ch.File, docString(ch.Document), ch.Path, fmt.Sprintf(format, a...))
	}
	m, err := mapping(g.Fields, driver.FieldChange{Site: ch.Site, Object: ch.Object, Field: ch.Field})
	if err != nil {
		return err
	}
	if m != nil {
		file, err := repoPath(m.File)
		if err != nil {
			return fmt.Errorf("spec.git.fields entry for %s: file %w", describeChange(ch), err)
		}
		if path.Clean(ch.File) != file || ch.Path != m.Path || !sameDocument(ch.Document, m.Document) {
			return refuse("spec.git.fields maps it to %s%s at %s", file, docString(m.Document), m.Path)
		}
		return nil
	}
	if g.Find == nil {
		return refuse("spec.git.fields has no entry for it, and spec.git.find is not set")
	}
	if ch.Path != ch.Field || ch.Document == nil {
		return refuse("a manifest found by spec.git.find is written at the field's own path, %s, in its document", ch.Field)
	}
	name, err := repoPath(ch.File)
	if err != nil {
		return refuse("the file %v", err)
	}
	data, err := r.readFile(ctx, c, commit, name)
	if err != nil {
		return refuse("%v", err)
	}
	docs, err := documents(data)
	if err != nil {
		return refuse("the file is not valid YAML: %v", err)
	}
	if d := int(*ch.Document); d >= 0 && d < len(docs) {
		if got, ok := manifestOf(docs[d]); ok && got.group == groupOf(ch.Object.APIVersion) && got.kind == ch.Object.Kind &&
			got.name == ch.Object.Name && (!got.namespaced || got.namespace == ch.Object.Namespace) {
			return nil
		}
	}
	return refuse("that document does not define %s %s/%s in commit %s", ch.Object.Kind, ch.Object.Namespace, ch.Object.Name, commit)
}

func sameDocument(a, b *int32) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// apply writes the changes into the clone's working tree, which holds
// commit. It returns the files whose bytes changed.
func (r *Repo) apply(ctx context.Context, c *clone, commit string, changes []failoverv1.GitChange) ([]string, error) {
	orig := map[string][]byte{}
	edited := map[string][]byte{}
	var order []string
	for i := range changes {
		ch := &changes[i]
		name, err := repoPath(ch.File)
		if err != nil {
			return nil, fmt.Errorf("%s: file %w", describeChange(ch), err)
		}
		data, ok := edited[name]
		if !ok {
			if data, err = r.readFile(ctx, c, commit, name); err != nil {
				return nil, fmt.Errorf("%s: %w", describeChange(ch), err)
			}
			orig[name] = data
			order = append(order, name)
		}
		if data, err = setField(data, ch.Document, ch.Path, ch.Value); err != nil {
			return nil, fmt.Errorf("%s: %s%s: %w", describeChange(ch), name, docString(ch.Document), err)
		}
		edited[name] = data
	}
	var changed []string
	for _, name := range order {
		if bytes.Equal(orig[name], edited[name]) {
			continue
		}
		p := filepath.Join(c.dir, filepath.FromSlash(name))
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, edited[name], info.Mode().Perm()); err != nil {
			return nil, err
		}
		changed = append(changed, name)
	}
	return changed, nil
}

// commit commits the index with the run's trailer. The author is the
// Workload's when it sets one, else git's own configuration's.
func (r *Repo) commit(ctx context.Context, c *clone, author *failoverv1.GitAuthor, req engine.CommitRequest) error {
	msg := strings.TrimRight(req.Message, " \t\r\n") + "\n\n" + trailer + ": " + req.Run + "\n"
	var env, args []string
	if author != nil {
		if author.Name == "" || author.Email == "" {
			return errors.New("spec.git.author needs a name and an email")
		}
		env = []string{
			"GIT_AUTHOR_NAME=" + author.Name, "GIT_AUTHOR_EMAIL=" + author.Email,
			"GIT_COMMITTER_NAME=" + author.Name, "GIT_COMMITTER_EMAIL=" + author.Email,
		}
	} else {
		args = []string{"-c", "user.useConfigOnly=true"}
	}
	args = append(args, "commit", "--quiet", "--no-verify", "--cleanup=whitespace", "--file=-")
	_, err := r.git(ctx, c.dir, env, msg, args...)
	var ge *gitError
	if errors.As(err, &ge) && noIdentity(ge.stderr) {
		return errors.New("git has no identity to commit with: set spec.git.author in the Workload, or user.name and user.email in git's configuration")
	}
	return err
}

func noIdentity(stderr string) bool {
	for _, s := range []string{
		"Please tell me who you are", "auto-detection is disabled",
		"unable to auto-detect email address", "empty ident name",
	} {
		if strings.Contains(stderr, s) {
			return true
		}
	}
	return false
}

// push pushes HEAD to the branch without force. A status line that starts
// with ! is a rejected ref, whatever git's exit status.
func (r *Repo) push(ctx context.Context, c *clone, commit string) error {
	out, err := r.git(ctx, c.dir, nil, "", "push", "--porcelain", "--no-verify", "origin", "HEAD:refs/heads/"+c.branch)
	if err != nil && ctx.Err() != nil {
		return err
	}
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "!") {
			continue
		}
		status := Redact(strings.TrimSpace(l[1:]))
		if strings.Contains(l, "[rejected]") {
			return stop.Ask(failoverv1.ReasonGitChanged,
				"the push of %s to branch %s of %s was rejected because the branch moved: Git changed during the run (%s)", commit, c.branch, Redact(c.url), status)
		}
		return fmt.Errorf("the remote refused the push of %s to branch %s: %s", commit, c.branch, status)
	}
	if err != nil {
		return stop.Wait(failoverv1.ReasonGitUnreachable, "%v: %v", ErrUnreachable, err)
	}
	return nil
}

// ranBy reports whether commit is the run's own commit on top of base.
func (r *Repo) ranBy(ctx context.Context, c *clone, commit, base, run string) bool {
	out, err := r.git(ctx, c.dir, nil, "", "log", "-1", "--no-show-signature", "--format=%(trailers:key="+trailer+",valueonly)", commit)
	if err != nil {
		return false
	}
	found := false
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == run {
			found = true
		}
	}
	if !found {
		return false
	}
	parent, err := r.commitID(ctx, c.dir, commit+"^1")
	return err == nil && parent == base
}

// find returns the full id of revision, a commit id, fetching the branch
// when the clone lacks it.
func (r *Repo) find(ctx context.Context, c *clone, revision string) (string, error) {
	commit, err := r.commitID(ctx, c.dir, revision)
	if err == nil {
		return commit, nil
	}
	_, err = r.fetch(ctx, c)
	switch {
	case errors.Is(err, ErrUnreachable):
		return "", stop.Wait(failoverv1.ReasonGitUnreachable, "%v", err)
	case err != nil:
		return "", err
	}
	if commit, err = r.commitID(ctx, c.dir, revision); err != nil && (len(revision) == 40 || len(revision) == 64) {
		// Some servers send a commit that is no longer on the branch.
		if _, ferr := r.git(ctx, c.dir, nil, "", "fetch", "--quiet", "--no-tags", "origin", revision); ferr == nil {
			commit, err = r.commitID(ctx, c.dir, revision)
		}
	}
	if err != nil {
		return "", fmt.Errorf("commit %s is not in branch %s of %s", revision, c.branch, Redact(c.url))
	}
	return commit, nil
}

// BranchHead implements engine.Git. It fetches the branch.
func (r *Repo) BranchHead(ctx context.Context, w *failoverv1.WorkloadSpec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.clone(ctx, &w.Git)
	if err != nil {
		return "", err
	}
	head, err := r.fetch(ctx, c)
	switch {
	case errors.Is(err, ErrUnreachable):
		return "", stop.Wait(failoverv1.ReasonGitUnreachable, "%v", err)
	case errors.Is(err, errNoBranch):
		return "", stop.Ask(failoverv1.ReasonGitChanged, "%v", err)
	}
	return head, err
}

// Contains implements engine.Git: git merge-base --is-ancestor.
func (r *Repo) Contains(ctx context.Context, w *failoverv1.WorkloadSpec, commit, rev string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range []string{commit, rev} {
		if !commitRE.MatchString(id) {
			return false, fmt.Errorf("revision %q is not a commit id", id)
		}
	}
	c, err := r.clone(ctx, &w.Git)
	if err != nil {
		return false, err
	}
	a, err := r.find(ctx, c, commit)
	if err != nil {
		return false, err
	}
	b, err := r.find(ctx, c, rev)
	if err != nil {
		return false, err
	}
	_, err = r.git(ctx, c.dir, nil, "", "merge-base", "--is-ancestor", a, b)
	var ge *gitError
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ge) && errors.As(ge.err, &ee) && ee.ExitCode() == 1:
		return false, nil
	}
	return false, err
}

// Checkout returns a directory that holds the repository's files at
// revision, a commit id. It fetches the branch when the clone lacks the
// commit. The directory stays until Close.
func (r *Repo) Checkout(ctx context.Context, w *failoverv1.WorkloadSpec, revision string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !commitRE.MatchString(revision) {
		return "", fmt.Errorf("revision %q is not a commit id", revision)
	}
	c, err := r.clone(ctx, &w.Git)
	if err != nil {
		return "", err
	}
	commit, err := r.find(ctx, c, revision)
	if err != nil {
		return "", err
	}
	if dir, ok := c.trees[commit]; ok {
		return dir, nil
	}
	parent, err := r.mkdir("tree-")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(parent, "repo")
	if _, err := r.git(ctx, c.dir, nil, "", "worktree", "add", "--quiet", "--detach", dir, commit); err != nil {
		return "", err
	}
	c.trees[commit] = dir
	return dir, nil
}
