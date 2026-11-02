package artifacts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is the git-backed revision store for artifacts. It is a bare
// repository ($TANDEM_HOME/artifacts.git) where each artifact is one ref,
// refs/artifacts/<artifactId>, and each revision is a commit on it whose tree
// holds the document as index.html. The commit message is the title followed
// by the revision's metadata as JSON. Only git plumbing is used, so no work
// tree or index is shared between concurrent writers, and a ref update is a
// compare-and-swap against the revision the writer built on.
type Repo struct {
	dir string
	// mu serializes writers in this daemon; update-ref's old-value check
	// still guards the ref against anything else touching the repository.
	mu sync.Mutex
}

// ContentFile is the document's path inside each revision's tree.
const ContentFile = "index.html"

const refPrefix = "refs/artifacts/"

// ErrNotFound reports an unknown artifact or a revision not on its history.
var ErrNotFound = errors.New("not_found")

// ErrConflict reports an update whose base revision is no longer the latest.
var ErrConflict = errors.New("conflict")

var (
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)
	revisionPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
)

// ValidID reports whether id can name an artifact (and its ref).
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ValidRevision reports whether rev is a full commit hash.
func ValidRevision(rev string) bool { return revisionPattern.MatchString(rev) }

// Revision is one stored version of an artifact. It is the metadata recorded
// in the revision's commit, plus the commit hash and position, and doubles as
// the payload of the transcript event announcing it.
type Revision struct {
	Kind           string `json:"kind,omitempty"`
	ArtifactID     string `json:"artifactId"`
	Revision       string `json:"revision"`
	RevisionNumber int    `json:"revisionNumber"`
	Type           string `json:"type"`
	Title          string `json:"title"`
	Filename       string `json:"filename"`
	MIMEType       string `json:"mimeType"`
	Size           int64  `json:"size"`
	ContentSHA256  string `json:"contentSha256"`
	// SessionID is the host-local session that published the revision.
	SessionID  string `json:"sessionId"`
	SourcePath string `json:"sourcePath,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	// SessionName is filled in for listings; it is not stored.
	SessionName string `json:"sessionName,omitempty"`
}

// OpenRepo opens the repository at dir, creating it when missing.
func OpenRepo(dir string) (*Repo, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New("git not found in PATH; artifacts need git")
	}
	r := &Repo{dir: dir}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if _, err := r.git(context.Background(), nil, "init", "--bare", "--quiet"); err != nil {
			return nil, fmt.Errorf("init artifacts repository: %w", err)
		}
		slog.Info("created artifacts repository", "dir", dir)
	} else if err != nil {
		return nil, err
	}
	return r, nil
}

// NewID returns a fresh artifact ID.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "art-" + hex.EncodeToString(b[:])
}

func (r *Repo) git(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return r.gitEnv(ctx, stdin, nil, args...)
}

func (r *Repo) gitEnv(ctx context.Context, stdin []byte, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir", r.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Head returns an artifact's latest revision hash, or "" when it does not exist.
func (r *Repo) Head(ctx context.Context, artifactID string) (string, error) {
	if !ValidID(artifactID) {
		return "", nil
	}
	out, err := r.git(ctx, nil, "for-each-ref", "--format=%(objectname)", refPrefix+artifactID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Commit records content as a new revision of meta.ArtifactID. base is the
// revision the caller built on: "" means "whatever is latest" for an existing
// artifact. A new artifact must not exist yet. The returned revision carries
// the commit hash and number.
func (r *Repo) Commit(ctx context.Context, base string, content []byte, meta Revision) (Revision, error) {
	if !ValidID(meta.ArtifactID) {
		return Revision{}, fmt.Errorf("invalid: bad artifact id %q", meta.ArtifactID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	head, err := r.Head(ctx, meta.ArtifactID)
	if err != nil {
		return Revision{}, err
	}
	if base != "" && base != head {
		return Revision{}, fmt.Errorf("%w: artifact %s is at revision %s, not %s", ErrConflict, meta.ArtifactID, head, base)
	}
	meta.RevisionNumber = 1
	if head != "" {
		parent, err := r.revisionMeta(ctx, head)
		if err != nil {
			return Revision{}, err
		}
		meta.RevisionNumber = parent.RevisionNumber + 1
	}
	digest := sha256.Sum256(content)
	meta.ContentSHA256 = hex.EncodeToString(digest[:])
	meta.Size = int64(len(content))
	meta.Kind, meta.Revision, meta.SessionName = "", "", ""

	blob, err := r.git(ctx, content, "hash-object", "-w", "--stdin")
	if err != nil {
		return Revision{}, err
	}
	tree, err := r.git(ctx, []byte("100644 blob "+strings.TrimSpace(string(blob))+"\t"+ContentFile+"\n"), "mktree")
	if err != nil {
		return Revision{}, err
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return Revision{}, err
	}
	args := []string{"commit-tree", strings.TrimSpace(string(tree))}
	if head != "" {
		args = append(args, "-p", head)
	}
	date := "@" + strconv.FormatInt(meta.CreatedAt/1000, 10) + " +0000"
	env := []string{
		"GIT_AUTHOR_NAME=Tandem", "GIT_AUTHOR_EMAIL=tandem@localhost", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=Tandem", "GIT_COMMITTER_EMAIL=tandem@localhost", "GIT_COMMITTER_DATE=" + date,
	}
	message := oneLine(meta.Title) + "\n\n" + string(metaJSON) + "\n"
	commit, err := r.gitEnv(ctx, []byte(message), env, append(args, "-F", "-")...)
	if err != nil {
		return Revision{}, err
	}
	meta.Revision = strings.TrimSpace(string(commit))
	old := head
	if old == "" {
		// Require that the ref does not exist yet.
		old = strings.Repeat("0", len(meta.Revision))
	}
	if _, err := r.git(ctx, nil, "update-ref", "-m", "artifact revision", refPrefix+meta.ArtifactID, meta.Revision, old); err != nil {
		return Revision{}, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return meta, nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "artifact"
	}
	return s
}

// parseMeta reads the JSON metadata after the commit message's title line.
func parseMeta(message string) (Revision, error) {
	_, body, ok := strings.Cut(message, "\n\n")
	if !ok {
		return Revision{}, errors.New("artifact commit has no metadata")
	}
	var meta Revision
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &meta); err != nil {
		return Revision{}, fmt.Errorf("artifact commit metadata: %w", err)
	}
	return meta, nil
}

func (r *Repo) revisionMeta(ctx context.Context, revision string) (Revision, error) {
	out, err := r.git(ctx, nil, "log", "-1", "--format=%B", revision)
	if err != nil {
		return Revision{}, err
	}
	meta, err := parseMeta(string(out))
	if err != nil {
		return Revision{}, err
	}
	meta.Revision = revision
	return meta, nil
}

// List returns the latest revision of every artifact, newest first.
func (r *Repo) List(ctx context.Context) ([]Revision, error) {
	out, err := r.git(ctx, nil, "for-each-ref", "--format=%(refname:lstrip=2)%1f%(objectname)%1f%(contents)%1e", refPrefix)
	if err != nil {
		return nil, err
	}
	list := make([]Revision, 0)
	for _, record := range strings.Split(string(out), "\x1e") {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\x1f", 3)
		if len(parts) != 3 {
			continue
		}
		meta, err := parseMeta(parts[2])
		if err != nil {
			slog.Warn("skipping artifact with unreadable metadata", "artifact_id", parts[0], "error", err)
			continue
		}
		meta.ArtifactID, meta.Revision = parts[0], parts[1]
		list = append(list, meta)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt > list[j].CreatedAt })
	return list, nil
}

// Revisions returns an artifact's revisions, newest first.
func (r *Repo) Revisions(ctx context.Context, artifactID string) ([]Revision, error) {
	head, err := r.Head(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return nil, fmt.Errorf("%w: no artifact %s", ErrNotFound, artifactID)
	}
	out, err := r.git(ctx, nil, "log", "--first-parent", "--format=%H%x1f%B%x1e", head)
	if err != nil {
		return nil, err
	}
	list := make([]Revision, 0)
	for _, record := range strings.Split(string(out), "\x1e") {
		record = strings.TrimLeft(record, "\n")
		if record == "" {
			continue
		}
		hash, message, ok := strings.Cut(record, "\x1f")
		if !ok {
			continue
		}
		meta, err := parseMeta(message)
		if err != nil {
			slog.Warn("skipping artifact revision with unreadable metadata", "artifact_id", artifactID, "revision", hash, "error", err)
			continue
		}
		meta.ArtifactID, meta.Revision = artifactID, hash
		list = append(list, meta)
	}
	return list, nil
}

// Content returns one revision's document and metadata; revision "" is the
// latest. A revision that is not on the artifact's history is not found.
func (r *Repo) Content(ctx context.Context, artifactID, revision string) ([]byte, Revision, error) {
	head, err := r.Head(ctx, artifactID)
	if err != nil {
		return nil, Revision{}, err
	}
	if head == "" {
		return nil, Revision{}, fmt.Errorf("%w: no artifact %s", ErrNotFound, artifactID)
	}
	if revision == "" {
		revision = head
	} else if !ValidRevision(revision) {
		return nil, Revision{}, fmt.Errorf("%w: revision must be a full commit hash", ErrNotFound)
	} else if revision != head {
		if _, err := r.git(ctx, nil, "merge-base", "--is-ancestor", revision, head); err != nil {
			return nil, Revision{}, fmt.Errorf("%w: revision %s is not a revision of %s", ErrNotFound, revision, artifactID)
		}
	}
	meta, err := r.revisionMeta(ctx, revision)
	if err != nil {
		return nil, Revision{}, err
	}
	meta.ArtifactID = artifactID
	data, err := r.git(ctx, nil, "cat-file", "blob", revision+":"+ContentFile)
	if err != nil {
		return nil, Revision{}, err
	}
	return data, meta, nil
}
