package pgvector

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/andino-agents/knowledge-base/internal/store"
	"github.com/andino-agents/knowledge-base/internal/store/storetest"
)

// lastDSN is the connection string for the single shared pgvector container.
// It stays empty when Docker is unavailable, which makes every test skip via
// SkipIfProviderIsNotHealthy, so `go test ./...` still needs nothing external
// on machines without a daemon.
var lastDSN string

// testCont holds the shared container for the lifetime of the test binary. It
// is closed in TestMain rather than per-test, so it outlives any single test.
var testCont testcontainers.Container

func TestMain(m *testing.M) {
	ctx := context.Background()
	if dockerHealthy(ctx) {
		startSharedPostgres(ctx)
	}
	code := m.Run()
	if testCont != nil {
		if err := testCont.Terminate(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "terminating pgvector container: %v\n", err)
		}
	}
	os.Exit(code)
}

// dockerHealthy reports whether a Docker daemon is reachable and healthy. It
// mirrors the check SkipIfProviderIsNotHealthy does per-test, so the container
// is only started when there is something to talk to.
func dockerHealthy(ctx context.Context) bool {
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return false
	}
	return provider.Health(ctx) == nil
}

// startSharedPostgres launches the pgvector container and resolves its DSN. On
// any failure it leaves lastDSN empty and the container closed, so tests skip.
func startSharedPostgres(ctx context.Context) {
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "pgvector/pgvector:pg17",
			Env:          map[string]string{"POSTGRES_PASSWORD": "postgres"},
			ExposedPorts: []string{"5432/tcp"},
			WaitingFor:   wait.ForListeningPort("5432/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	}
	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting pgvector container: %v\n", err)
		return
	}
	testCont = container
	defer func() {
		// Only leave the container running if we fully succeeded.
		if lastDSN == "" {
			_ = container.Terminate(ctx)
		}
	}()

	host, err := container.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving pgvector host: %v\n", err)
		lastDSN = ""
		return
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolving pgvector port: %v\n", err)
		lastDSN = ""
		return
	}
	lastDSN = fmt.Sprintf("postgres://postgres:postgres@%s:%s/postgres?sslmode=disable", host, port.Port())

	if err := waitForReady(ctx, lastDSN); err != nil {
		fmt.Fprintf(os.Stderr, "pgvector container never became ready: %v\n", err)
		lastDSN = ""
	}
}

// testDSN returns the shared container's DSN, skipping the test when Docker is
// unavailable rather than failing it.
func testDSN(t *testing.T) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	return lastDSN
}

// waitForReady polls until Postgres accepts queries. The container's port
// listens before the server finishes starting, so the image's own readiness
// probe resolves too early; a real ping is the signal.
func waitForReady(ctx context.Context, dsn string) error {
	for {
		db, err := sql.Open("pgx", dsn)
		if err == nil {
			if pingErr := db.PingContext(ctx); pingErr == nil {
				_ = db.Close()
				return nil
			}
			_ = db.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func openTestStore(t *testing.T, kb string) store.Store {
	t.Helper()
	dsn := testDSN(t)
	if err := waitForReady(context.Background(), dsn); err != nil {
		t.Fatalf("postgres not ready: %v", err)
	}
	s, err := store.Open(context.Background(), "postgres", store.Options{
		KBName:         kb,
		ModelName:      "test-model",
		Dimensions:     storetest.Dimensions,
		ProviderConfig: map[string]any{"dsn": dsn},
	})
	if err != nil {
		t.Fatalf("opening postgres store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestConformance runs the provider-independent suite. It is the pgvector
// provider's guarantee of parity with sqlite: same semantics, same harness.
func TestConformance(t *testing.T) {
	storetest.TestStore(t, openTestStore(t, "conformance"))
}

func TestIdentityMismatchIsHardError(t *testing.T) {
	ctx := context.Background()
	dsn := testDSN(t)
	open := func(model string, dim int) (store.Store, error) {
		return store.Open(ctx, "postgres", store.Options{
			KBName: "kb", ModelName: model, Dimensions: dim,
			ProviderConfig: map[string]any{"dsn": dsn},
		})
	}
	s, err := open("model-a", storetest.Dimensions)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := open("model-b", storetest.Dimensions); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("model change must be a hard error, got: %v", err)
	}
	if _, err := open("model-a", storetest.Dimensions+8); err == nil ||
		!strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("dimension change must be a hard error, got: %v", err)
	}
	// Same identity reopens fine.
	s, err = open("model-a", storetest.Dimensions)
	if err != nil {
		t.Fatalf("reopen with same identity: %v", err)
	}
	s.Close()
}

func TestSanitizeFTSQuery(t *testing.T) {
	cases := map[string]string{
		`what's "cache-reuse"?`:     `"what" OR "s" OR "cache-reuse"`,
		`ttm.pages_limit=25165824`:  `"ttm.pages_limit" OR "25165824"`,
		`(a AND b) NOT c*`:          `"a" OR "AND" OR "b" OR "NOT" OR "c"`,
		`   `:                       ``,
		`configuración de búsqueda`: `"configuración" OR "de" OR "búsqueda"`,
	}
	for in, want := range cases {
		if got := sanitizeFTSQuery(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
