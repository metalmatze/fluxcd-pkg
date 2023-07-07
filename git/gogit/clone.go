/*
Copyright 2022 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gogit

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	extgogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/fluxcd/pkg/git"
	"github.com/fluxcd/pkg/git/repository"
	"github.com/fluxcd/pkg/version"
)

const tagDereferenceSuffix = "^{}"

func (g *Client) cloneBranch(ctx context.Context, url, branch string, opts repository.CloneConfig) (*git.Commit, error) {
	if g.authOpts == nil {
		return nil, fmt.Errorf("unable to checkout repo with an empty set of auth options")
	}
	authMethod, err := transportAuth(g.authOpts, g.useDefaultKnownHosts)
	if err != nil {
		return nil, fmt.Errorf("unable to construct auth method with options: %w", err)
	}

	ref := plumbing.NewBranchReferenceName(branch)
	// check if previous revision has changed before attempting to clone
	if lastObserved := git.TransformRevision(opts.LastObservedCommit); lastObserved != "" {
		head, err := g.getRemoteHEAD(ctx, url, ref, authMethod)
		if err != nil {
			return nil, err
		}
		hash := git.ExtractHashFromRevision(head)
		if head != "" && hash.Digest() == lastObserved {
			// Construct a non-concrete commit with the existing information.
			c := &git.Commit{
				Hash:      hash,
				Reference: plumbing.NewBranchReferenceName(branch).String(),
			}
			return c, nil
		}
	}

	var depth int
	if opts.ShallowClone {
		depth = 1
	}
	cloneOpts := &extgogit.CloneOptions{
		URL:               url,
		Auth:              authMethod,
		RemoteName:        git.DefaultRemote,
		ReferenceName:     plumbing.NewBranchReferenceName(branch),
		SingleBranch:      g.singleBranch,
		NoCheckout:        false,
		Depth:             depth,
		RecurseSubmodules: recurseSubmodules(opts.RecurseSubmodules),
		Progress:          nil,
		Tags:              extgogit.NoTags,
		CABundle:          caBundle(g.authOpts),
		ProxyOptions:      g.proxy,
	}

	repo, err := extgogit.CloneContext(ctx, g.storer, g.worktreeFS, cloneOpts)
	if err != nil {
		if err == transport.ErrRepositoryNotFound || isRemoteBranchNotFoundErr(err, ref.String()) {
			return nil, git.ErrRepositoryNotFound{
				Message: fmt.Sprintf("unable to clone: %s", err),
				URL:     url,
			}
		}
		// Directly cloning an empty Git repo to a directory fails with this error.
		// We check for the error and then init a new Git repo in that directory
		// (which represents an empty repository).
		if err == transport.ErrEmptyRemoteRepository {
			if err = os.RemoveAll(g.path); err == nil {
				if err = g.Init(ctx, url, branch); err == nil {
					return nil, nil
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("unable to clone '%s': %w", url, goGitError(err))
		}
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("unable to resolve HEAD of branch '%s': %w", branch, err)
	}
	cc, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("unable to resolve commit object for HEAD '%s': %w", head.Hash(), err)
	}
	g.repository = repo
	return buildCommitWithRef(cc, ref)
}

func (g *Client) cloneTag(ctx context.Context, url, tag string, opts repository.CloneConfig) (*git.Commit, error) {
	if g.authOpts == nil {
		return nil, fmt.Errorf("unable to checkout repo with an empty set of auth options")
	}

	authMethod, err := transportAuth(g.authOpts, g.useDefaultKnownHosts)
	if err != nil {
		return nil, fmt.Errorf("unable to construct auth method with options: %w", err)
	}

	ref := plumbing.NewTagReferenceName(tag)
	// check if previous revision has changed before attempting to clone
	if lastObserved := git.TransformRevision(opts.LastObservedCommit); lastObserved != "" {
		head, err := g.getRemoteHEAD(ctx, url, ref, authMethod)
		if err != nil {
			return nil, err
		}
		hash := git.ExtractHashFromRevision(head)
		shortRef := fmt.Sprintf("%s@%s", tag, hash.Digest())
		if head != "" && shortRef == lastObserved {
			// Construct a non-concrete commit with the existing information.
			c := &git.Commit{
				Hash:      hash,
				Reference: ref.String(),
			}
			return c, nil
		}
	}

	var depth int
	if opts.ShallowClone {
		depth = 1
	}
	cloneOpts := &extgogit.CloneOptions{
		URL:               url,
		Auth:              authMethod,
		RemoteName:        git.DefaultRemote,
		ReferenceName:     plumbing.NewTagReferenceName(tag),
		SingleBranch:      g.singleBranch,
		NoCheckout:        false,
		Depth:             depth,
		RecurseSubmodules: recurseSubmodules(opts.RecurseSubmodules),
		Progress:          nil,
		Tags:              extgogit.NoTags,
		CABundle:          caBundle(g.authOpts),
		ProxyOptions:      g.proxy,
	}

	repo, err := extgogit.CloneContext(ctx, g.storer, g.worktreeFS, cloneOpts)
	if err != nil {
		if err == transport.ErrEmptyRemoteRepository || err == transport.ErrRepositoryNotFound || isRemoteBranchNotFoundErr(err, ref.String()) {
			return nil, git.ErrRepositoryNotFound{
				Message: fmt.Sprintf("unable to clone: %s", err),
				URL:     url,
			}
		}
		return nil, fmt.Errorf("unable to clone '%s': %w", url, goGitError(err))
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("unable to resolve HEAD of tag '%s': %w", tag, err)
	}
	cc, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("unable to resolve commit object for HEAD '%s': %w", head.Hash(), err)
	}
	g.repository = repo
	return buildCommitWithRef(cc, ref)
}

func (g *Client) cloneCommit(ctx context.Context, url, commit string, opts repository.CloneConfig) (*git.Commit, error) {
	authMethod, err := transportAuth(g.authOpts, g.useDefaultKnownHosts)
	if err != nil {
		return nil, fmt.Errorf("unable to construct auth method with options: %w", err)
	}
	cloneOpts := &extgogit.CloneOptions{
		URL:               url,
		Auth:              authMethod,
		RemoteName:        git.DefaultRemote,
		SingleBranch:      false,
		NoCheckout:        true,
		RecurseSubmodules: recurseSubmodules(opts.RecurseSubmodules),
		Progress:          nil,
		Tags:              extgogit.NoTags,
		CABundle:          caBundle(g.authOpts),
		ProxyOptions:      g.proxy,
	}
	if opts.Branch != "" {
		cloneOpts.SingleBranch = g.singleBranch
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(opts.Branch)
	}

	repo, err := extgogit.CloneContext(ctx, g.storer, g.worktreeFS, cloneOpts)
	if err != nil {
		if err == transport.ErrEmptyRemoteRepository || err == transport.ErrRepositoryNotFound ||
			isRemoteBranchNotFoundErr(err, cloneOpts.ReferenceName.String()) {
			return nil, git.ErrRepositoryNotFound{
				Message: fmt.Sprintf("unable to clone: %s", err),
				URL:     url,
			}
		}
		return nil, fmt.Errorf("unable to clone '%s': %w", url, goGitError(err))
	}

	w, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("unable to open repo worktree: %w", err)
	}
	cc, err := repo.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return nil, fmt.Errorf("unable to resolve commit object for '%s': %w", commit, err)
	}
	err = w.Checkout(&extgogit.CheckoutOptions{
		Hash:  cc.Hash,
		Force: true,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to checkout commit '%s': %w", commit, err)
	}
	g.repository = repo
	if opts.RefName != "" {
		cloneOpts.ReferenceName = plumbing.ReferenceName(opts.RefName)
	}
	return buildCommitWithRef(cc, cloneOpts.ReferenceName)
}

func (g *Client) cloneSemVer(ctx context.Context, url, semverTag string, opts repository.CloneConfig) (*git.Commit, error) {
	verConstraint, err := semver.NewConstraint(semverTag)
	if err != nil {
		return nil, fmt.Errorf("semver parse error: %w", err)
	}

	authMethod, err := transportAuth(g.authOpts, g.useDefaultKnownHosts)
	if err != nil {
		return nil, fmt.Errorf("unable to construct auth method with options: %w", err)
	}
	var depth int
	if opts.ShallowClone {
		depth = 1
	}
	cloneOpts := &extgogit.CloneOptions{
		URL:               url,
		Auth:              authMethod,
		RemoteName:        git.DefaultRemote,
		NoCheckout:        false,
		Depth:             depth,
		RecurseSubmodules: recurseSubmodules(opts.RecurseSubmodules),
		Progress:          nil,
		Tags:              extgogit.AllTags,
		CABundle:          caBundle(g.authOpts),
		ProxyOptions:      g.proxy,
	}

	repo, err := extgogit.CloneContext(ctx, g.storer, g.worktreeFS, cloneOpts)
	if err != nil {
		if err == transport.ErrEmptyRemoteRepository || err == transport.ErrRepositoryNotFound {
			return nil, git.ErrRepositoryNotFound{
				Message: fmt.Sprintf("unable to clone: %s", err),
				URL:     url,
			}
		}
		return nil, fmt.Errorf("unable to clone '%s': %w", url, goGitError(err))
	}

	repoTags, err := repo.Tags()
	if err != nil {
		return nil, fmt.Errorf("unable to list tags: %w", err)
	}

	tags := make(map[string]string)
	tagTimestamps := make(map[string]time.Time)
	if err = repoTags.ForEach(func(t *plumbing.Reference) error {
		revision := plumbing.Revision(t.Name().String())
		hash, err := repo.ResolveRevision(revision)
		if err != nil {
			return fmt.Errorf("unable to resolve tag revision: %w", err)
		}
		commit, err := repo.CommitObject(*hash)
		if err != nil {
			return fmt.Errorf("unable to resolve commit of a tag revision: %w", err)
		}
		tagTimestamps[t.Name().Short()] = commit.Committer.When

		tags[t.Name().Short()] = t.Strings()[1]
		return nil
	}); err != nil {
		return nil, err
	}

	var matchedVersions semver.Collection
	for tag := range tags {
		v, err := version.ParseVersion(tag)
		if err != nil {
			continue
		}
		if !verConstraint.Check(v) {
			continue
		}
		matchedVersions = append(matchedVersions, v)
	}
	if len(matchedVersions) == 0 {
		return nil, fmt.Errorf("no match found for semver: %s", semverTag)
	}

	// Sort versions
	sort.SliceStable(matchedVersions, func(i, j int) bool {
		left := matchedVersions[i]
		right := matchedVersions[j]

		if !left.Equal(right) {
			return left.LessThan(right)
		}

		// Having tag target timestamps at our disposal, we further try to sort
		// versions into a chronological order. This is especially important for
		// versions that differ only by build metadata, because it is not considered
		// a part of the comparable version in Semver
		return tagTimestamps[left.Original()].Before(tagTimestamps[right.Original()])
	})
	v := matchedVersions[len(matchedVersions)-1]
	t := v.Original()

	w, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("unable to open Git worktree: %w", err)
	}

	ref := plumbing.NewTagReferenceName(t)
	err = w.Checkout(&extgogit.CheckoutOptions{
		Branch: ref,
	})
	if err != nil {
		return nil, fmt.Errorf("unable to checkout tag '%s': %w", t, err)
	}
	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("unable to resolve HEAD of tag '%s': %w", t, err)
	}
	cc, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("unable to resolve commit object for HEAD '%s': %w", head.Hash(), err)
	}
	g.repository = repo
	return buildCommitWithRef(cc, ref)
}

func (g *Client) cloneRefName(ctx context.Context, url string, refName string, cloneOpts repository.CloneConfig) (*git.Commit, error) {
	if g.authOpts == nil {
		return nil, fmt.Errorf("unable to checkout repo with an empty set of auth options")
	}
	authMethod, err := transportAuth(g.authOpts, g.useDefaultKnownHosts)
	if err != nil {
		return nil, fmt.Errorf("unable to construct auth method with options: %w", err)
	}
	head, err := g.getRemoteHEAD(ctx, url, plumbing.ReferenceName(refName), authMethod)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return nil, fmt.Errorf("unable to resolve ref '%s' to a specific commit", refName)
	}

	hash := git.ExtractHashFromRevision(head)
	// check if previous revision has changed before attempting to clone
	if lastObserved := git.TransformRevision(cloneOpts.LastObservedCommit); lastObserved != "" {
		if head != "" && head == lastObserved {
			// Construct a non-concrete commit with the existing information.
			c := &git.Commit{
				Reference: refName,
				Hash:      hash,
			}
			return c, nil
		}
	}

	return g.cloneCommit(ctx, url, hash.String(), cloneOpts)
}

func recurseSubmodules(recurse bool) extgogit.SubmoduleRescursivity {
	if recurse {
		return extgogit.DefaultSubmoduleRecursionDepth
	}
	return extgogit.NoRecurseSubmodules
}

func (g *Client) getRemoteHEAD(ctx context.Context, url string, ref plumbing.ReferenceName,
	authMethod transport.AuthMethod) (string, error) {
	// ref: https://git-scm.com/docs/git-check-ref-format#_description; point no. 6
	if strings.HasPrefix(ref.String(), "/") || strings.HasSuffix(ref.String(), "/") {
		return "", fmt.Errorf("ref %s is invalid; Git refs cannot begin or end with a slash '/'", ref.String())
	}

	remoteCfg := &config.RemoteConfig{
		Name: git.DefaultRemote,
		URLs: []string{url},
	}
	remote := extgogit.NewRemote(memory.NewStorage(), remoteCfg)
	listOpts := &extgogit.ListOptions{
		Auth:          authMethod,
		CABundle:      caBundle(g.authOpts),
		PeelingOption: extgogit.AppendPeeled,
		ProxyOptions:  g.proxy,
	}
	refs, err := remote.ListContext(ctx, listOpts)
	if err != nil {
		return "", fmt.Errorf("unable to list remote for '%s': %w", url, err)
	}

	head := filterRefs(refs, ref)
	return head, nil
}

// filterRefs searches through the provided list of refs to find a matching ref
// based on the currentRef parameter.
// It returns the matching ref under the following conditions:
// - If currentRef is not a tag, or
// - If currentRef has the tag dereference suffix.
//
// If a matching ref is found in the list but doesn't satisfy the above
// conditions, it attempts to find a ref that is the same as currentRef but
// also has the tag dereference suffix.
// This is necessary because when a tag is annotated, the ref without the
// suffix points to a tag object with its own unique hash.
// However, the goal is to obtain the hash of the commit object that the tag
// points to, which requires dereferencing the tag using the dereference suffix.
// For more information, refer to: https://stackoverflow.com/a/15472310/10745226
//
// If a ref meeting the above requirements cannot be found, it means currentRef
// is a lightweight tag, so the initially found ref is returned.
//
// If it fails to find a matching ref, then an empty string is returned.
func filterRefs(refs []*plumbing.Reference, currentRef plumbing.ReferenceName) string {
	var (
		currentRefStr = currentRef.String()
		fallbackRef   *plumbing.Reference
	)

	for _, ref := range refs {
		if ref.Name().String() == currentRefStr {
			if !currentRef.IsTag() || strings.HasSuffix(currentRefStr, tagDereferenceSuffix) {
				return fmt.Sprintf("%s@%s", currentRefStr, git.Hash(ref.Hash().String()).Digest())
			}
			fallbackRef = ref
		}

		if ref.Name().String() == currentRefStr+tagDereferenceSuffix {
			return fmt.Sprintf("%s@%s", currentRefStr, git.Hash(ref.Hash().String()).Digest())
		}
	}

	if fallbackRef != nil {
		return fmt.Sprintf("%s@%s", currentRefStr, git.Hash(fallbackRef.Hash().String()).Digest())
	}

	return ""
}

func buildSignature(s object.Signature) git.Signature {
	return git.Signature{
		Name:  s.Name,
		Email: s.Email,
		When:  s.When,
	}
}

func buildCommitWithRef(c *object.Commit, ref plumbing.ReferenceName) (*git.Commit, error) {
	if c == nil {
		return nil, fmt.Errorf("unable to construct commit: no object")
	}

	// Encode commit components excluding signature into SignedData.
	encoded := &plumbing.MemoryObject{}
	if err := c.EncodeWithoutSignature(encoded); err != nil {
		return nil, fmt.Errorf("unable to encode commit '%s': %w", c.Hash, err)
	}
	reader, err := encoded.Reader()
	if err != nil {
		return nil, fmt.Errorf("unable to encode commit '%s': %w", c.Hash, err)
	}
	b, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("unable to read encoded commit '%s': %w", c.Hash, err)
	}
	return &git.Commit{
		Hash:      []byte(c.Hash.String()),
		Reference: ref.String(),
		Author:    buildSignature(c.Author),
		Committer: buildSignature(c.Committer),
		Signature: c.PGPSignature,
		Encoded:   b,
		Message:   c.Message,
	}, nil
}

func isRemoteBranchNotFoundErr(err error, ref string) bool {
	return strings.Contains(err.Error(), fmt.Sprintf("couldn't find remote ref '%s'", ref))
}

// goGitError translates an error from the go-git library, or returns
// `nil` if the argument is `nil`.
func goGitError(err error) error {
	if err == nil {
		return nil
	}
	switch strings.TrimSpace(err.Error()) {
	case "unknown error: remote:":
		// this unhelpful error arises because go-git takes the first
		// line of the output on stderr, and for some git providers
		// (GitLab, at least) the output has a blank line at the
		// start. The rest of stderr is thrown away, so we can't get
		// the actual error; but at least we know what was being
		// attempted, and the likely cause.
		return fmt.Errorf("push rejected; check git secret has write access")
	default:
		return err
	}
}
