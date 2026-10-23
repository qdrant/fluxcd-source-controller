// Package forksync merges a release of a fork's upstream into the fork, the way
// the sync pull requests are built.
//
// A sync branch gets up to two commits, so that each can be reviewed on its own:
//
//  1. "Merge upstream tag vX.Y.Z": upstream's changes since the last sync,
//     merged automatically, with nothing of the fork's added. The paths the
//     fork owns take upstream's version here.
//  2. "chore: apply Qdrant adaptations ...": the fork's version of those paths
//     put back, plus what the configured rename and adaptations change. Skipped
//     when there is nothing to change.
//
// A merge with conflicts is committed as it is, with a file listing the
// conflicting files (some conflicts leave no markers), so that CI fails until
// someone resolves them. The fork-owned paths are still put back, in a commit of
// their own, so that upstream's versions of them never end up on the branch.
package forksync

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	"golang.org/x/mod/semver"

	"github.com/qdrant/fluxcd-source-controller/tools/fork-sync/internal/config"
	"github.com/qdrant/fluxcd-source-controller/tools/fork-sync/internal/git"
	"github.com/qdrant/fluxcd-source-controller/tools/fork-sync/internal/rename"
)

const (
	// upstreamRefs is where upstream's tags are fetched, apart from the fork's
	// own tags (vX.Y.Z-qdrant.N).
	upstreamRefs = "refs/upstream-tags/"
	// ConflictsFile lists the conflicting files of a merge with conflicts. It is
	// committed, and CI fails while it exists.
	ConflictsFile = ".sync-upstream-conflicts"
	// BranchPrefix starts the name of every sync branch.
	BranchPrefix = "sync/upstream-"
)

// stableTag matches upstream's stable releases. Pre-releases and the tags of
// the api/ module (api/vX.Y.Z) do not match.
var stableTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

// ErrInvalidTag is returned for a tag that is not a stable release (vX.Y.Z).
var ErrInvalidTag = errors.New("not a stable upstream release tag (expected vX.Y.Z)")

// Syncer syncs a fork with its upstream.
type Syncer struct {
	Repo   git.Repo
	Config config.Config
	// Logger reports what the sync does. The zero value logs nothing.
	Logger logr.Logger
}

// Target is the upstream release to sync.
type Target struct {
	Tag string
	// UpToDate is true when the release is already merged into the fork.
	UpToDate bool
}

// Branch is the name of the sync branch for the release.
func (t Target) Branch() string {
	return BranchPrefix + t.Tag
}

// Status is the outcome of a merge.
type Status string

const (
	// StatusMerged means the merge was clean and the adaptations were applied.
	StatusMerged Status = "merged"
	// StatusConflict means the merge has conflicts, committed as they are.
	StatusConflict Status = "conflict"
)

// Result is the outcome of Merge.
type Result struct {
	Tag    string
	Branch string
	// Base is the commit the sync branch starts from.
	Base   string
	Status Status
	// Conflicts are the conflicting files, for StatusConflict.
	Conflicts []string
	// Restored are the fork-owned paths put back after the merge, with their
	// status relative to the merge commit.
	Restored []git.Change
	// FailedAdaptations are the adaptations that failed and were allowed to:
	// their changes are not on the branch.
	FailedAdaptations []string
}

// FindTarget fetches upstream's tags and returns the release to sync: tag, or
// the latest stable release when tag is empty.
func (s *Syncer) FindTarget(tag string) (Target, error) {
	err := s.fetchUpstreamTags()
	if err != nil {
		return Target{}, err
	}
	if tag == "" {
		var err error
		tag, err = s.latestStableTag()
		if err != nil {
			return Target{}, err
		}
	}
	err = s.checkTag(tag)
	if err != nil {
		return Target{}, err
	}
	target := Target{
		Tag:      tag,
		UpToDate: s.Repo.Succeeds("merge-base", "--is-ancestor", upstreamRefs+tag, "HEAD"),
	}
	s.Logger.Info("Found the upstream release", "tag", tag, "upToDate", target.UpToDate)
	return target, nil
}

// Merge creates the sync branch for the upstream release tag from HEAD and
// commits the merge and the adaptations to it. A merge with conflicts is not
// an error: it is committed as it is and reported with StatusConflict.
func (s *Syncer) Merge(tag string) (Result, error) {
	err := s.fetchUpstreamTags()
	if err != nil {
		return Result{}, err
	}
	err = s.checkTag(tag)
	if err != nil {
		return Result{}, err
	}
	ref := upstreamRefs + tag
	base, err := s.Repo.Run("rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	res := Result{Tag: tag, Branch: BranchPrefix + tag, Base: strings.TrimSpace(base)}

	owned, err := s.ownedChangedUpstream(res.Base, ref)
	if err != nil {
		return Result{}, err
	}
	s.Logger.Info("Merging the upstream release", "tag", tag, "branch", res.Branch, "forkOwnedChangedUpstream", owned)
	_, err = s.Repo.Run("checkout", "--quiet", "-b", res.Branch)
	if err != nil {
		return Result{}, err
	}

	conflicts, err := s.mergeUpstreamChanges(ref, owned)
	if err != nil {
		return Result{}, err
	}
	if len(conflicts) > 0 {
		s.Logger.Info("The merge has conflicts, committing it as it is", "files", conflicts)
		res.Status = StatusConflict
		res.Conflicts = conflicts
		res.Restored, err = s.commitConflicts(tag, res.Base, owned, conflicts)
		if err != nil {
			return Result{}, err
		}
		return res, nil
	}

	_, err = s.Repo.Run("commit", "--quiet", "-m", "Merge upstream tag "+tag)
	if err != nil {
		return Result{}, err
	}
	err = s.commitAdaptations(&res, owned)
	if err != nil {
		return Result{}, err
	}
	res.Status = StatusMerged
	s.Logger.Info("Merged the upstream release", "tag", tag, "failedAdaptations", res.FailedAdaptations)
	return res, nil
}

func (s *Syncer) fetchUpstreamTags() error {
	s.Logger.V(1).Info("Fetching upstream's tags", "upstream", s.Config.Upstream)
	_, err := s.Repo.Run("fetch", "--quiet", "--no-tags", s.Config.Upstream, "+refs/tags/*:"+upstreamRefs+"*")
	return err
}

func (s *Syncer) latestStableTag() (string, error) {
	out, err := s.Repo.Run("for-each-ref", "--format=%(refname:strip=2)", upstreamRefs)
	if err != nil {
		return "", err
	}
	latest := ""
	for tag := range strings.FieldsSeq(out) {
		if stableTag.MatchString(tag) && (latest == "" || semver.Compare(tag, latest) > 0) {
			latest = tag
		}
	}
	if latest == "" {
		return "", fmt.Errorf("no stable release tags found at %s", s.Config.Upstream)
	}
	return latest, nil
}

func (s *Syncer) checkTag(tag string) error {
	if !stableTag.MatchString(tag) {
		return fmt.Errorf("%q: %w", tag, ErrInvalidTag)
	}
	if !s.Repo.Succeeds("rev-parse", "--verify", "--quiet", upstreamRefs+tag) {
		return fmt.Errorf("upstream tag %q not found", tag)
	}
	return nil
}

// ownedChangedUpstream returns the files under the fork-owned paths that
// upstream added, changed, deleted or renamed (both names) since the last sync.
func (s *Syncer) ownedChangedUpstream(head, ref string) ([]string, error) {
	if len(s.Config.OwnedPaths) == 0 {
		return nil, nil
	}
	mergeBase, err := s.Repo.Run("merge-base", head, ref)
	if err != nil {
		return nil, err
	}
	args := append([]string{"diff", "--name-only", "--no-renames", "-z", strings.TrimSpace(mergeBase), ref, "--"}, s.Config.OwnedPaths...)
	return s.Repo.Paths(args...)
}

// mergeUpstreamChanges merges ref without committing, with the fork-owned paths
// set to upstream's version, and returns the files that still conflict.
func (s *Syncer) mergeUpstreamChanges(ref string, owned []string) ([]string, error) {
	_, mergeErr := s.Repo.Run("merge", "--no-ff", "--no-commit", ref) // fails on conflicts, which are handled below
	if !s.Repo.Succeeds("rev-parse", "--quiet", "--verify", "MERGE_HEAD") {
		if mergeErr == nil {
			return nil, fmt.Errorf("nothing to merge from %s", ref)
		}
		return nil, fmt.Errorf("failed to merge %s: %w", ref, mergeErr)
	}
	err := s.takePathsFrom(ref, owned)
	if err != nil {
		return nil, err
	}
	return s.Repo.Paths("diff", "--name-only", "--diff-filter=U", "-z")
}

// commitConflicts commits the unresolved merge, markers and list of conflicting
// files included, then puts the fork-owned paths back in a commit of their own.
func (s *Syncer) commitConflicts(tag, base string, owned, conflicts []string) ([]git.Change, error) {
	list := strings.Join(conflicts, "\n") + "\n"
	err := os.WriteFile(filepath.Join(s.Repo.Dir, ConflictsFile), []byte(list), 0o644)
	if err != nil {
		return nil, err
	}
	_, err = s.Repo.Run("add", "--all")
	if err != nil {
		return nil, err
	}
	_, err = s.Repo.Run("commit", "--quiet", "--no-verify", "-m", "Merge upstream tag "+tag+" (UNRESOLVED CONFLICTS)")
	if err != nil {
		return nil, err
	}

	restored, err := s.restoreOwned(base, owned)
	if err != nil || len(restored) == 0 {
		return restored, err
	}
	_, err = s.Repo.Run("commit", "--quiet", "--no-verify",
		"-m", "chore: keep the fork-owned paths after merging "+tag,
		"-m", "The merge has conflicts, so only the fork-owned paths are put back for now "+
			"(status is relative to the merge commit):\n"+formatChanges(restored))
	return restored, err
}

// Rename applies the configured rename to the working tree, leaving out the
// fork-owned paths, and returns the paths it wrote. Without a rename configured
// it does nothing.
func (s *Syncer) Rename() ([]string, error) {
	if s.Config.Rename == nil {
		return nil, nil
	}
	return rename.Apply(s.Repo, *s.Config.Rename, s.Config.OwnedPaths)
}

// commitAdaptations puts the fork-owned paths back, renames and runs the
// adaptations, then commits all of it, if anything changed.
func (s *Syncer) commitAdaptations(res *Result, owned []string) error {
	var err error
	res.Restored, err = s.restoreOwned(res.Base, owned)
	if err != nil {
		return err
	}
	_, err = s.Rename()
	if err != nil {
		return fmt.Errorf("failed to rename: %w", err)
	}
	// Stage everything, so that a failed adaptation's changes are all that is
	// left unstaged, and can be dropped.
	_, err = s.Repo.Run("add", "--all")
	if err != nil {
		return err
	}
	for _, adaptation := range s.Config.Adaptations {
		err = s.runAdaptation(adaptation, res)
		if err != nil {
			return err
		}
	}

	if s.Repo.Succeeds("diff", "--cached", "--quiet") {
		return nil
	}
	_, err = s.Repo.Run("commit", "--quiet",
		"-m", "chore: apply Qdrant adaptations after merging "+res.Tag,
		"-m", adaptationsMessage(*res))
	return err
}

// runAdaptation runs an adaptation and stages what it changed. If it fails and
// is allowed to, its changes are dropped and it is added to
// res.FailedAdaptations instead. Everything before it must be staged.
func (s *Syncer) runAdaptation(adaptation config.Adaptation, res *Result) error {
	runErr := s.run(adaptation)
	if runErr == nil {
		s.Logger.Info("Adaptation done", "adaptation", adaptation.Name)
		_, err := s.Repo.Run("add", "--all")
		return err
	}
	if !adaptation.AllowFailure {
		return fmt.Errorf("failed to run the adaptation %q: %w", adaptation.Name, runErr)
	}
	s.Logger.Error(runErr, "Adaptation failed, dropping its changes", "adaptation", adaptation.Name)
	res.FailedAdaptations = append(res.FailedAdaptations, adaptation.Name)
	return s.dropUnstaged()
}

// restoreOwned puts the given fork-owned paths back as they are in base, and
// returns their status relative to the current commit.
func (s *Syncer) restoreOwned(base string, owned []string) ([]git.Change, error) {
	err := s.takePathsFrom(base, owned)
	if err != nil {
		return nil, err
	}
	return s.Repo.NameStatus("--cached")
}

// takePathsFrom makes the working tree and index have each path as it is in
// ref, or not have it at all if ref does not.
func (s *Syncer) takePathsFrom(ref string, paths []string) error {
	for _, path := range paths {
		var err error
		if s.Repo.Succeeds("cat-file", "-e", ref+":"+path) {
			_, err = s.Repo.Run("checkout", ref, "--", path)
		} else {
			_, err = s.Repo.Run("rm", "-q", "-f", "--ignore-unmatch", "--", path)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// dropUnstaged reverts the changes that are not staged: changes to tracked
// files, and new untracked files. Ignored files are left alone.
func (s *Syncer) dropUnstaged() error {
	_, err := s.Repo.Run("checkout", "--", ".")
	if err != nil {
		return err
	}
	_, err = s.Repo.Run("clean", "-d", "--force", "--quiet")
	return err
}

// run runs the program of an adaptation in the repository. Its output goes where
// git's does.
func (s *Syncer) run(adaptation config.Adaptation) error {
	cmd := exec.Command(adaptation.Run[0], adaptation.Run[1:]...) //nolint:gosec // the commands come from the fork's own configuration
	cmd.Dir = s.Repo.Dir
	cmd.Stdout = s.Repo.Output
	cmd.Stderr = s.Repo.Output
	return cmd.Run()
}
