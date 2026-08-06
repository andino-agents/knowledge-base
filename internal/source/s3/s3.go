// Package s3 is the object-storage source: an S3 or S3-compatible bucket
// (MinIO, Ceph RGW) listed by poll and read on demand.
//
// Credentials come from the standard AWS chain (environment, shared config,
// SSO, IRSA, instance profiles) via aws-sdk-go-v2; nothing secret lives in
// the andino-kb YAML. A custom endpoint plus path-style addressing covers
// MinIO and friends.
package s3

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bmatcuk/doublestar/v4"

	"github.com/andino-agents/knowledge-base/internal/source"
)

type Options struct {
	Name      string
	Bucket    string
	Prefix    string   // key prefix inside the bucket; stripped from RelPath
	Region    string   // optional; the chain's default applies otherwise
	Endpoint  string   // optional custom endpoint (MinIO)
	PathStyle bool     // path-style addressing, required by most MinIO setups
	Paths     []string // doublestar globs over the stripped key; empty = all
	Exts      []string // extractor extension allowlist
}

type Bucket struct {
	opts   Options
	client *awss3.Client
	exts   map[string]bool
}

// New builds the source. The client construction is separated behind a
// small hook so tests can point it at a fake server.
func New(ctx context.Context, opts Options) (*Bucket, error) {
	if opts.Bucket == "" {
		return nil, fmt.Errorf("s3 %s: bucket is required", opts.Name)
	}
	var loadOpts []func(*awsconfig.LoadOptions) error
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3 %s: loading AWS config: %w", opts.Name, err)
	}
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.PathStyle
	})
	return NewWithClient(opts, client), nil
}

// NewWithClient wires a prebuilt client (tests, custom credential setups).
func NewWithClient(opts Options, client *awss3.Client) *Bucket {
	exts := make(map[string]bool, len(opts.Exts))
	for _, e := range opts.Exts {
		exts[strings.ToLower(e)] = true
	}
	return &Bucket{opts: opts, client: client, exts: exts}
}

func (b *Bucket) Name() string { return b.opts.Name }

func (b *Bucket) URI(relPath string) string {
	key := b.opts.Prefix + relPath
	return "s3://" + b.opts.Bucket + "/" + key
}

// Sync is a no-op: every List reflects the bucket's current state.
func (b *Bucket) Sync(ctx context.Context) error { return nil }

func (b *Bucket) indexable(rel string) bool {
	if !b.exts[strings.ToLower(filepath.Ext(rel))] {
		return false
	}
	if len(b.opts.Paths) == 0 {
		return true
	}
	for _, g := range b.opts.Paths {
		if ok, _ := doublestar.Match(g, rel); ok {
			return true
		}
	}
	return false
}

func (b *Bucket) List(ctx context.Context) ([]source.FileMeta, error) {
	var metas []source.FileMeta
	paginator := awss3.NewListObjectsV2Paginator(b.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(b.opts.Bucket),
		Prefix: aws.String(b.opts.Prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 %s: listing %s: %w", b.opts.Name, b.opts.Bucket, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			rel := strings.TrimPrefix(key, b.opts.Prefix)
			if rel == "" || strings.HasSuffix(rel, "/") || !b.indexable(rel) {
				continue
			}
			var mtime int64
			if obj.LastModified != nil {
				mtime = obj.LastModified.Unix()
			}
			metas = append(metas, source.FileMeta{
				RelPath:   rel,
				SizeBytes: aws.ToInt64(obj.Size),
				MtimeUnix: mtime,
			})
		}
	}
	return metas, nil
}

func (b *Bucket) Read(ctx context.Context, relPath string) (io.ReadCloser, error) {
	out, err := b.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(b.opts.Bucket),
		Key:    aws.String(b.opts.Prefix + relPath),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 %s: reading %s: %w", b.opts.Name, relPath, err)
	}
	return out.Body, nil
}
