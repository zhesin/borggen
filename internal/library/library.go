// Package library persists generated scripts in a local git repository, one
// commit per real change. A save that differs from the currently committed
// version only in its generation timestamp line is a no-op, not a noise
// commit — see generator.StripTimestamp, which both the idempotency test and
// this package use for the same comparison.
//
// Driven entirely by go-git: no git binary is shelled out to, so the
// container image needs nothing beyond the Go runtime.
package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"borggen/internal/generator"
)

// branch is the one local/remote branch name this package ever uses.
// Hardcoded rather than configurable: one repo, one writer (borggen itself),
// no reason for the branch to ever be anything else.
const branch = "main"

// author is the synthetic commit identity — the repository records that
// borggen made the change, not a real person.
var author = &object.Signature{Name: "borggen", Email: "borggen@localhost"}

// Library is a git-backed store of generated scripts, one file per job (and
// its check/restore-drill companions) at the repository root — flat, not
// nested per job.
type Library struct {
	dir  string
	repo *git.Repository
}

// Open initializes a git repository at dir if one does not exist yet, or
// opens the existing one. Safe to call on every server start.
func Open(dir string) (*Library, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("library: create dir: %w", err)
	}
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = git.PlainInitWithOptions(dir, &git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
		})
	}
	if err != nil {
		return nil, fmt.Errorf("library: open repository: %w", err)
	}
	return &Library{dir: dir, repo: repo}, nil
}

// Dir returns the repository's working directory, for callers that need to
// hand it to something else (e.g. wiring a remote in a later phase).
func (l *Library) Dir() string { return l.dir }

// validName rejects anything that is not a plain filename: no path
// separators (the library is intentionally flat), no traversal.
func validName(name string) error {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return fmt.Errorf("library: invalid filename %q", name)
	}
	return nil
}

// Save writes content to filename at the repository root and commits it,
// unless the only difference from what is already there is the generation
// timestamp line — in that case neither the file nor the repository is
// touched, and committed reports false. filename must be a plain name (no
// path separators): the library is flat by design (spec §3.9).
func (l *Library) Save(filename, content string) (committed bool, err error) {
	if err := validName(filename); err != nil {
		return false, err
	}
	full := filepath.Join(l.dir, filename)

	existing, readErr := os.ReadFile(full)
	isNew := os.IsNotExist(readErr)
	if readErr != nil && !isNew {
		return false, fmt.Errorf("library: read %s: %w", filename, readErr)
	}
	if !isNew && generator.StripTimestamp(string(existing)) == generator.StripTimestamp(content) {
		return false, nil
	}

	// Every file in the library is a generated .sh script meant to be run
	// directly (from cron or by hand) — 0o755 so a fresh checkout of the
	// library, or a pull onto another host, does not need a manual chmod +x
	// before the script can be scheduled. go-git picks up the executable bit
	// from the file's mode on disk when it is staged below. WriteFile's own
	// perm argument only takes effect when the file does not exist yet (the
	// open(2) mode is ignored without O_CREAT actually creating it), so an
	// already-committed file re-saved with new content needs an explicit
	// Chmod too, or it would keep whatever mode it already had on disk.
	if err := os.WriteFile(full, []byte(content), 0o755); err != nil {
		return false, fmt.Errorf("library: write %s: %w", filename, err)
	}
	if err := os.Chmod(full, 0o755); err != nil {
		return false, fmt.Errorf("library: chmod %s: %w", filename, err)
	}

	wt, err := l.repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("library: worktree: %w", err)
	}
	if _, err := wt.Add(filename); err != nil {
		return false, fmt.Errorf("library: stage %s: %w", filename, err)
	}

	verb := "update"
	if isNew {
		verb = "add"
	}
	sig := *author
	sig.When = time.Now()
	if _, err := wt.Commit(fmt.Sprintf("%s %s", verb, filename), &git.CommitOptions{Author: &sig}); err != nil {
		return false, fmt.Errorf("library: commit %s: %w", filename, err)
	}
	return true, nil
}

// List returns every job script in the library, sorted by name — flat, so
// this is a plain directory listing, not a tree walk.
func (l *Library) List() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("library: read dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Read returns filename's current content from the working tree (always the
// same as HEAD's — Save is the only writer this package exposes).
func (l *Library) Read(filename string) (string, error) {
	if err := validName(filename); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(l.dir, filename))
	if err != nil {
		return "", fmt.Errorf("library: read %s: %w", filename, err)
	}
	return string(data), nil
}

// Commit is one entry of a file's history.
type Commit struct {
	Hash    string    `json:"hash"`
	Message string    `json:"message"`
	When    time.Time `json:"when"`
}

// Log returns filename's commit history, newest first. An empty repository
// (nothing committed yet) is not an error — it has no history yet.
func (l *Library) Log(filename string) ([]Commit, error) {
	if err := validName(filename); err != nil {
		return nil, err
	}
	head, err := l.repo.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("library: head: %w", err)
	}

	iter, err := l.repo.Log(&git.LogOptions{From: head.Hash(), FileName: &filename})
	if err != nil {
		return nil, fmt.Errorf("library: log %s: %w", filename, err)
	}
	var out []Commit
	err = iter.ForEach(func(c *object.Commit) error {
		out = append(out, Commit{Hash: c.Hash.String(), Message: c.Message, When: c.Author.When})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("library: iterate log for %s: %w", filename, err)
	}
	return out, nil
}

// ReadAt returns filename's content as it was at a specific commit — the
// primitive a rollback ("save this old version again, producing a new
// commit — history stays linear") or a history viewer needs. Not a diff:
// producing unified diffs is deliberately left to Gitea's own web UI once
// the remote exists, rather than duplicating that UI here.
func (l *Library) ReadAt(filename, hash string) (string, error) {
	if err := validName(filename); err != nil {
		return "", err
	}
	commit, err := l.repo.CommitObject(plumbing.NewHash(hash))
	if err != nil {
		return "", fmt.Errorf("library: commit %s: %w", hash, err)
	}
	file, err := commit.File(filename)
	if err != nil {
		return "", fmt.Errorf("library: %s not present at %s: %w", filename, hash, err)
	}
	content, err := file.Contents()
	if err != nil {
		return "", fmt.Errorf("library: read %s at %s: %w", filename, hash, err)
	}
	return content, nil
}

// setRemote points "origin" at url, creating it on first use or repointing
// it if the configured URL changed. Safe to call before every push/pull.
func (l *Library) setRemote(url string) error {
	cfg := &config.RemoteConfig{Name: "origin", URLs: []string{url}}
	if _, err := l.repo.CreateRemote(cfg); err == nil {
		return nil
	} else if !errors.Is(err, git.ErrRemoteExists) {
		return fmt.Errorf("library: create remote: %w", err)
	}

	remote, err := l.repo.Remote("origin")
	if err != nil {
		return fmt.Errorf("library: get remote: %w", err)
	}
	if len(remote.Config().URLs) == 1 && remote.Config().URLs[0] == url {
		return nil
	}
	if err := l.repo.DeleteRemote("origin"); err != nil {
		return fmt.Errorf("library: replace remote: %w", err)
	}
	if _, err := l.repo.CreateRemote(cfg); err != nil {
		return fmt.Errorf("library: recreate remote: %w", err)
	}
	return nil
}

// Push publishes the local branch to the Gitea remote at url, authenticating
// with token the same way GitHub/GitLab-style PATs work — as the HTTP basic
// auth password (the borg docs' own recommendation for this token style).
//
// Fast-forward only: a plain (non-force) push already refuses a
// non-fast-forward update server-side, but this checks first — after a
// fetch, local HEAD must contain everything on origin/main — so a
// divergence is reported as a clear "pull first" error instead of a raw,
// possibly cryptic rejection from the remote.
func (l *Library) Push(url, token string) error {
	if err := l.setRemote(url); err != nil {
		return err
	}
	auth := &githttp.BasicAuth{Username: "borggen", Password: token}

	err := l.repo.Fetch(&git.FetchOptions{RemoteName: "origin", Auth: auth})
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		if err := l.refuseIfDiverged(); err != nil {
			return err
		}
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		// A brand new Gitea repo with nothing pushed yet — there is no
		// history to diverge from, so this is the normal first-push case.
	default:
		return fmt.Errorf("library: fetch before push: %w", err)
	}

	err = l.repo.Push(&git.PushOptions{RemoteName: "origin", Auth: auth})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("library: push: %w", err)
	}
	return nil
}

// Pull fetches from the Gitea remote and fast-forwards the local branch.
// go-git's Worktree.Pull only ever resolves as a fast-forward — if that is
// not possible (this repo has commits origin does not), it fails rather
// than merging, matching Push's own rule in the other direction.
func (l *Library) Pull(url, token string) error {
	if err := l.setRemote(url); err != nil {
		return err
	}
	auth := &githttp.BasicAuth{Username: "borggen", Password: token}

	wt, err := l.repo.Worktree()
	if err != nil {
		return fmt.Errorf("library: worktree: %w", err)
	}
	err = wt.Pull(&git.PullOptions{RemoteName: "origin", Auth: auth})
	switch {
	case err == nil, errors.Is(err, git.NoErrAlreadyUpToDate):
		return nil
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		// Nothing has ever been pushed — an empty repo pulls in nothing,
		// which is not an error condition.
		return nil
	case errors.Is(err, git.ErrNonFastForwardUpdate):
		return fmt.Errorf("library: local history has commits origin does not — push first")
	default:
		return fmt.Errorf("library: pull: %w", err)
	}
}

// refuseIfDiverged compares local HEAD against origin/main (after a Fetch):
// if origin holds commits this repo's history does not contain, Push must
// not proceed — no merge is attempted, the caller has to Pull first.
func (l *Library) refuseIfDiverged() error {
	head, err := l.repo.Head()
	if err != nil {
		return fmt.Errorf("library: head: %w", err)
	}
	remoteRef, err := l.repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil // origin has no history yet — nothing to diverge from
	}
	if err != nil {
		return fmt.Errorf("library: remote ref: %w", err)
	}
	if remoteRef.Hash() == head.Hash() {
		return nil
	}

	local, err := l.repo.CommitObject(head.Hash())
	if err != nil {
		return fmt.Errorf("library: local commit: %w", err)
	}
	remote, err := l.repo.CommitObject(remoteRef.Hash())
	if err != nil {
		return fmt.Errorf("library: remote commit: %w", err)
	}
	ancestor, err := remote.IsAncestor(local)
	if err != nil {
		return fmt.Errorf("library: ancestry check: %w", err)
	}
	if !ancestor {
		return fmt.Errorf("library: diverged from origin/%s — pull first", branch)
	}
	return nil
}
