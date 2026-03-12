// Package oci provides OCI registry operations for pulling and pushing
// pre-built VM images using the ORAS (OCI Registry As Storage) library.
package oci

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// ArtifactType identifies boxcutter VM images in OCI registries.
	ArtifactType = "application/vnd.boxcutter.vm.v1"

	// MediaTypeQCOW2 is the media type for zstd-compressed QCOW2 images.
	MediaTypeQCOW2 = "application/vnd.boxcutter.vm.qcow2.v1+zstd"

	// Default registry and repository.
	DefaultRegistry   = "ghcr.io"
	DefaultRepository = "AndrewBudd/boxcutter"
)

// ImageMetadata holds version information about an OCI-distributed VM image.
type ImageMetadata struct {
	Version string // e.g., "v0.1.0"
	Commit  string // e.g., "049616f"
	Created string // RFC3339 timestamp
	VMType  string // "node", "orchestrator", or "golden"
	Digest  string // OCI manifest digest
	Tag     string // tag that was resolved
}

// PullOptions configures an image pull operation.
type PullOptions struct {
	Registry   string // OCI registry (default: ghcr.io)
	Repository string // Repository path (default: AndrewBudd/boxcutter)
	VMType     string // "node", "orchestrator", or "golden"
	Tag        string // Tag to pull (default: "latest")
	OutputDir  string // Directory to write the pulled image to
	Auth       *GitHubAppAuth // Optional: authenticate via GitHub App (used if GITHUB_TOKEN not set)
}

func (o *PullOptions) defaults() {
	if o.Registry == "" {
		o.Registry = DefaultRegistry
	}
	if o.Repository == "" {
		o.Repository = DefaultRepository
	}
	if o.Tag == "" {
		o.Tag = "latest"
	}
}

func (o *PullOptions) ref() string {
	return fmt.Sprintf("%s/%s/%s:%s", o.Registry, strings.ToLower(o.Repository), o.VMType, o.Tag)
}

func (o *PullOptions) repoPath() string {
	return fmt.Sprintf("%s/%s", strings.ToLower(o.Repository), o.VMType)
}

// newRepo creates an authenticated remote.Repository for the given options.
func newRepo(opts *PullOptions) (*remote.Repository, error) {
	repo, err := remote.NewRepository(fmt.Sprintf("%s/%s", opts.Registry, opts.repoPath()))
	if err != nil {
		return nil, fmt.Errorf("creating repository reference: %w", err)
	}

	// Set up authentication: env var > gh CLI > GitHub App
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	if token == "" {
		// Try gh CLI token — under sudo, run as the real user for correct auth context
		ghCmd := exec.Command("gh", "auth", "token")
		if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
			ghCmd = exec.Command("sudo", "-u", sudoUser, "gh", "auth", "token")
		}
		if out, err := ghCmd.Output(); err == nil {
			token = strings.TrimSpace(string(out))
		}
	}
	if token == "" && opts.Auth != nil {
		// Fall back to GitHub App token (works for pull only on personal accounts)
		var err error
		token, err = opts.Auth.Token()
		if err != nil {
			return nil, fmt.Errorf("GitHub App auth: %w", err)
		}
	}
	if token != "" {
		repo.Client = &auth.Client{
			Client: retry.DefaultClient,
			Credential: auth.StaticCredential(opts.Registry, auth.Credential{
				Username: "boxcutter",
				Password: token,
			}),
		}
	}

	return repo, nil
}

// Resolve checks what version a tag points to without downloading the image.
func Resolve(ctx context.Context, opts PullOptions) (*ImageMetadata, error) {
	opts.defaults()

	repo, err := newRepo(&opts)
	if err != nil {
		return nil, err
	}

	desc, err := repo.Resolve(ctx, opts.Tag)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", opts.ref(), err)
	}

	meta := &ImageMetadata{
		VMType: opts.VMType,
		Digest: desc.Digest.String(),
		Tag:    opts.Tag,
	}

	if desc.Annotations != nil {
		meta.Version = desc.Annotations[ocispec.AnnotationVersion]
		meta.Commit = desc.Annotations[ocispec.AnnotationRevision]
		meta.Created = desc.Annotations[ocispec.AnnotationCreated]
	}

	return meta, nil
}

// Pull downloads a VM image from an OCI registry and writes it to outputDir.
// Returns metadata about the pulled image and the path to the downloaded file.
func Pull(ctx context.Context, opts PullOptions) (*ImageMetadata, string, error) {
	opts.defaults()

	repo, err := newRepo(&opts)
	if err != nil {
		return nil, "", err
	}

	if err := os.MkdirAll(opts.OutputDir, 0755); err != nil {
		return nil, "", fmt.Errorf("creating output directory: %w", err)
	}

	// Use a clean staging directory for the OCI pull to avoid oras file store
	// conflicts with unrelated files in the output directory.
	stageDir, err := os.MkdirTemp(opts.OutputDir, ".oci-pull-*")
	if err != nil {
		return nil, "", fmt.Errorf("creating staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir)

	store, err := file.New(stageDir)
	if err != nil {
		return nil, "", fmt.Errorf("creating file store: %w", err)
	}
	defer store.Close()

	desc, err := oras.Copy(ctx, repo, opts.Tag, store, opts.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return nil, "", fmt.Errorf("pulling %s: %w", opts.ref(), err)
	}

	meta := &ImageMetadata{
		VMType: opts.VMType,
		Digest: desc.Digest.String(),
		Tag:    opts.Tag,
	}
	if desc.Annotations != nil {
		meta.Version = desc.Annotations[ocispec.AnnotationVersion]
		meta.Commit = desc.Annotations[ocispec.AnnotationRevision]
		meta.Created = desc.Annotations[ocispec.AnnotationCreated]
	}

	// Move the downloaded file from staging to output directory
	outputFile := ""
	entries, _ := os.ReadDir(stageDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".zst" {
			src := filepath.Join(stageDir, e.Name())
			dst := filepath.Join(opts.OutputDir, e.Name())
			if err := os.Rename(src, dst); err != nil {
				return nil, "", fmt.Errorf("moving downloaded file: %w", err)
			}
			outputFile = dst
			break
		}
	}

	return meta, outputFile, nil
}

// PushOptions configures an image push operation.
type PushOptions struct {
	Registry   string   // OCI registry (default: ghcr.io)
	Repository string   // Repository path (default: AndrewBudd/boxcutter)
	VMType     string   // "node", "orchestrator", or "golden"
	Tags       []string // Tags to apply (e.g., ["v0.1.0", "latest"])
	FilePath   string   // Path to the .qcow2.zst or .ext4.zst file
	Auth       *GitHubAppAuth // Optional: authenticate via GitHub App

	Version string
	Commit  string
}

func (o *PushOptions) defaults() {
	if o.Registry == "" {
		o.Registry = DefaultRegistry
	}
	if o.Repository == "" {
		o.Repository = DefaultRepository
	}
}

// Push uploads a VM image to an OCI registry with the given tags and annotations.
func Push(ctx context.Context, opts PushOptions) error {
	opts.defaults()

	if len(opts.Tags) == 0 {
		return fmt.Errorf("at least one tag is required")
	}

	pullOpts := &PullOptions{
		Registry:   opts.Registry,
		Repository: opts.Repository,
		VMType:     opts.VMType,
		Auth:       opts.Auth,
	}
	repo, err := newRepo(pullOpts)
	if err != nil {
		return err
	}

	dir := filepath.Dir(opts.FilePath)
	store, err := file.New(dir)
	if err != nil {
		return fmt.Errorf("creating file store: %w", err)
	}
	defer store.Close()

	fileName := filepath.Base(opts.FilePath)

	fileDesc, err := store.Add(ctx, fileName, MediaTypeQCOW2, opts.FilePath)
	if err != nil {
		return fmt.Errorf("adding file to store: %w", err)
	}

	annotations := map[string]string{}
	if opts.Version != "" {
		annotations[ocispec.AnnotationVersion] = opts.Version
	}
	if opts.Commit != "" {
		annotations[ocispec.AnnotationRevision] = opts.Commit
	}

	packOpts := oras.PackManifestOptions{
		Layers:              []ocispec.Descriptor{fileDesc},
		ManifestAnnotations: annotations,
	}

	desc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, ArtifactType, packOpts)
	if err != nil {
		return fmt.Errorf("packing manifest: %w", err)
	}

	for _, tag := range opts.Tags {
		if err := store.Tag(ctx, desc, tag); err != nil {
			return fmt.Errorf("tagging %s: %w", tag, err)
		}

		_, err = oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions)
		if err != nil {
			return fmt.Errorf("pushing %s: %w", tag, err)
		}
	}

	return nil
}
