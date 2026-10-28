package pipelines

import (
	"context"
	"regexp"
	"strings"
)

// FetchRequest asks for the commit at one ref of a repository. The fetcher never runs Git hooks, checks out a
// worktree or follows submodules (ADR 0033).
type FetchRequest struct {
	URL                 string
	Ref                 string
	Username, Token     string
	AllowPrivateNetwork bool
	// MaxBytes bounds both the downloaded pack and the archive written.
	MaxBytes int64
	MaxFiles int
}

// Fetched describes the commit written to the archive.
type Fetched struct {
	Commit string
	// Skipped counts tree entries that cannot become snapshot files, such as submodules.
	Skipped int
}

// Fetcher writes the tree of the commit at r.Ref to archive as an uncompressed TAR.
type Fetcher interface {
	Fetch(ctx context.Context, r FetchRequest, archive string) (Fetched, error)
}

// FetchError is a failure the pipeline owner can act on; its message is safe to show and never holds credentials.
type FetchError struct{ Code, Message string }

func (e *FetchError) Error() string { return e.Message }

var (
	commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	refPattern    = regexp.MustCompile(`^refs/(heads|tags)/[A-Za-z0-9._/-]{1,200}$`)
)

// ValidRef accepts branch and tag refs with conservative characters and no path tricks.
func ValidRef(ref string) bool {
	if !refPattern.MatchString(ref) {
		return false
	}
	for _, bad := range []string{"..", "//", "/.", ".lock", "@{"} {
		if strings.Contains(ref, bad) {
			return false
		}
	}
	return ref[len(ref)-1] != '/' && ref[len(ref)-1] != '.'
}

func ValidCommit(c string) bool { return commitPattern.MatchString(c) }
