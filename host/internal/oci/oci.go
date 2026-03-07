// Package oci provides OCI registry operations for pulling and pushing
// pre-built VM images using the ORAS (OCI Registry As Storage) library.
package oci

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	Version   string // e.g., "v0.1.0"
	Commit    string // e.g., "049616f"
	Created   string // RFC3339 timestamp
	VMType    string // "node" or "orchestrator"
	Digest    string // OCI manifest digest
	Tag       string // tag that was resolved
}

// PullOptions configures an image pull operation.
type PullOptions struct {
	Registry   string // OCI registry (default: ghcr.io)
	Repository string // Repository path (default: AndrewBudd/boxcutter)
	VMType     string // "node" or "orchestrator"
	Tag        string // Tag to pull (default: "latest")
	OutputDir  string // Directory to write the pulled image to

	// Progress is called with bytes downloaded so far and total bytes.
	// May be nil.
	Progress func(downloaded, total int64)
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
	return fmt.Sprintf("%s/%s/%s:%s", o.Registry, o.Repository, o.VMType, o.Tag)
}

func (o *PullOptions) repoPath() string {
	return fmt.Sprintf("%s/%s", o.Repository, o.VMType)
}

// newRepo creates an authenticated remote.Repository for the given options.
func newRepo(opts *PullOptions) (*remote.Repository, error) {
	repo, err := remote.NewRepository(fmt.Sprintf("%s/%s", opts.Registry, opts.repoPath()))
	if err != nil {
		return nil, fmt.Errorf("creating repository reference: %w", err)
	}

	// Set up authentication
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
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

	// Extract annotations if present
	if desc.Annotations != nil {
		meta.Version = desc.Annotations[ocispec.AnnotationVersion]
		meta.Commit = desc.Annotations[ocispec.AnnotationRevision]
		meta.Created = desc.Annotations[ocispec.AnnotationCreated]
	}

	return meta, nil
}

// Pull downloads a VM image from an OCI registry and writes it to outputDir.
// Returns metadata about the pulled image.
func Pull(ctx context.Context, opts PullOptions) (*ImageMetadata, string, error) {
	opts.defaults()

	repo, err := newRepo(&opts)
	if err != nil {
		return nil, "", err
	}

	// Create a file store for the output
	if err := os.MkdirAll(opts.OutputDir, 0755); err != nil {
		return nil, "", fmt.Errorf("creating output directory: %w", err)
	}

	store, err := file.New(opts.OutputDir)
	if err != nil {
		return nil, "", fmt.Errorf("creating file store: %w", err)
	}
	defer store.Close()

	// Pull the artifact
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

	// Find the QCOW2 file in the output directory
	outputFile := ""
	entries, _ := os.ReadDir(opts.OutputDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".zst" {
			outputFile = filepath.Join(opts.OutputDir, e.Name())
			break
		}
	}
	if outputFile == "" {
		// Look for any file that isn't a directory
		for _, e := range entries {
			if !e.IsDir() && e.Name() != "index.json" {
				outputFile = filepath.Join(opts.OutputDir, e.Name())
				break
			}
		}
	}

	return meta, outputFile, nil
}

// Push uploads a VM image to an OCI registry.
type PushOptions struct {
	Registry   string // OCI registry (default: ghcr.io)
	Repository string // Repository path (default: AndrewBudd/boxcutter)
	VMType     string // "node" or "orchestrator"
	Tags       []string // Tags to apply (e.g., ["v0.1.0", "latest"])
	FilePath   string // Path to the .qcow2.zst file

	// Annotations to add to the manifest.
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
	}
	repo, err := newRepo(pullOpts)
	if err != nil {
		return err
	}

	// Create file store from the directory containing the file
	dir := filepath.Dir(opts.FilePath)
	store, err := file.New(dir)
	if err != nil {
		return fmt.Errorf("creating file store: %w", err)
	}
	defer store.Close()

	fileName := filepath.Base(opts.FilePath)

	// Get file info for size
	fi, err := os.Stat(opts.FilePath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", opts.FilePath, err)
	}

	// Add the file as a layer
	fileDesc, err := store.Add(ctx, fileName, MediaTypeQCOW2, opts.FilePath)
	if err != nil {
		return fmt.Errorf("adding file to store: %w", err)
	}
	_ = fi // fileDesc gets the size from the file

	// Set up annotations
	annotations := map[string]string{}
	if opts.Version != "" {
		annotations[ocispec.AnnotationVersion] = opts.Version
	}
	if opts.Commit != "" {
		annotations[ocispec.AnnotationRevision] = opts.Commit
	}
	annotations[ocispec.AnnotationCreated] = "now" // Will be set properly by caller

	// Pack the manifest
	packOpts := oras.PackManifestOptions{
		Layers:              []ocispec.Descriptor{fileDesc},
		ManifestAnnotations: annotations,
	}

	desc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, ArtifactType, packOpts)
	if err != nil {
		return fmt.Errorf("packing manifest: %w", err)
	}

	// Tag and push for each tag
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

// progressReader wraps an io.Reader to report progress.
type progressReader struct {
	reader     io.Reader
	total      int64
	downloaded int64
	callback   func(downloaded, total int64)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.downloaded += int64(n)
	if pr.callback != nil {
		pr.callback(pr.downloaded, pr.total)
	}
	return n, err
}
