package s3

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

func fakeS3(t *testing.T) (*awss3.Client, *gofakes3.GoFakeS3) {
	t.Helper()
	backend := s3mem.New()
	faker := gofakes3.New(backend)
	srv := httptest.NewServer(faker.Server())
	t.Cleanup(srv.Close)

	client := awss3.New(awss3.Options{
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
	})
	return client, faker
}

func put(t *testing.T, client *awss3.Client, bucket, key, content string) {
	t.Helper()
	_, err := client.PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(content),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestListReadAndFiltering(t *testing.T) {
	ctx := context.Background()
	client, _ := fakeS3(t)
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("docs")}); err != nil {
		t.Fatal(err)
	}
	put(t, client, "docs", "team/guide.md", "# Guide\n\nHello from the bucket.")
	put(t, client, "docs", "team/policies/backup.md", "# Backup policy")
	put(t, client, "docs", "team/logo.png", "PNG")
	put(t, client, "docs", "other/skip.md", "outside prefix")

	b := NewWithClient(Options{
		Name: "bucket", Bucket: "docs", Prefix: "team/",
		Paths: nil, Exts: []string{".md"},
	}, client)

	metas, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, m := range metas {
		got[m.RelPath] = true
		if m.SizeBytes == 0 || m.MtimeUnix == 0 {
			t.Errorf("meta missing size/mtime: %+v", m)
		}
	}
	if !got["guide.md"] || !got["policies/backup.md"] || len(got) != 2 {
		t.Fatalf("listing = %v, want guide.md and policies/backup.md only", got)
	}

	rc, err := b.Read(ctx, "guide.md")
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(content), "Hello from the bucket") {
		t.Fatalf("content = %q", content)
	}

	if uri := b.URI("guide.md"); uri != "s3://docs/team/guide.md" {
		t.Errorf("URI = %q", uri)
	}
}

func TestGlobFilter(t *testing.T) {
	ctx := context.Background()
	client, _ := fakeS3(t)
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("bkt")}); err != nil {
		t.Fatal(err)
	}
	put(t, client, "bkt", "adr/001.md", "adr")
	put(t, client, "bkt", "notes/x.md", "note")

	b := NewWithClient(Options{
		Name: "bkt", Bucket: "bkt", Paths: []string{"adr/**"}, Exts: []string{".md"},
	}, client)
	metas, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].RelPath != "adr/001.md" {
		t.Fatalf("glob filter failed: %+v", metas)
	}
}

func TestChangeDetectionSignal(t *testing.T) {
	// The incremental sync diffs size+mtime; overwriting an object must move
	// at least one of them.
	ctx := context.Background()
	client, _ := fakeS3(t)
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String("bkt")}); err != nil {
		t.Fatal(err)
	}
	put(t, client, "bkt", "doc.md", "v1 content")

	b := NewWithClient(Options{Name: "bkt", Bucket: "bkt", Exts: []string{".md"}}, client)
	before, err := b.List(ctx)
	if err != nil || len(before) != 1 {
		t.Fatalf("before: %v %v", before, err)
	}
	time.Sleep(1100 * time.Millisecond) // fake S3 mtime granularity is 1s
	put(t, client, "bkt", "doc.md", "v2 content, longer than before")
	after, err := b.List(ctx)
	if err != nil || len(after) != 1 {
		t.Fatalf("after: %v %v", after, err)
	}
	if before[0].SizeBytes == after[0].SizeBytes && before[0].MtimeUnix == after[0].MtimeUnix {
		t.Fatal("overwrite did not change size or mtime; incremental sync would miss it")
	}
}
