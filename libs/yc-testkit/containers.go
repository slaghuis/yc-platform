package testkit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Images match docker-compose.yml so tests run against what dev runs.
const (
	PostgresImage  = "postgres:16-alpine"
	NATSImage      = "nats:2.10-alpine"
	SeaweedFSImage = "chrislusf/seaweedfs:4.48"
)

// S3Store is a throwaway S3-compatible store with a single bucket that
// anyone may read, as in deploy/compose/seaweedfs-s3.json.
type S3Store struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
}

// S3 starts a throwaway SeaweedFS S3 server with the named bucket and
// returns how to reach it. The container stops when tb finishes.
func S3(tb testing.TB, bucket string) S3Store {
	tb.Helper()
	ctx := context.Background()
	store := S3Store{Bucket: bucket, AccessKey: "testkey", SecretKey: "testsecret"}

	identities := fmt.Sprintf(`{"identities": [
  {"name": "test", "credentials": [{"accessKey": %q, "secretKey": %q}],
   "actions": ["Admin", "Read", "List", "Tagging", "Write"]},
  {"name": "anonymous", "actions": ["Read:%s"]}
]}`, store.AccessKey, store.SecretKey, bucket)

	c, err := testcontainers.Run(ctx, SeaweedFSImage,
		testcontainers.WithCmd("mini", "-dir=/data", "-bucket="+bucket,
			"-s3.config=/etc/seaweedfs/s3.json", "-master.telemetry=false"),
		testcontainers.WithExposedPorts("8333/tcp"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader: strings.NewReader(identities), ContainerFilePath: "/etc/seaweedfs/s3.json", FileMode: 0o644,
		}),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/healthz").WithPort("8333/tcp").WithStartupTimeout(90*time.Second),
		),
	)
	testcontainers.CleanupContainer(tb, c)
	if err != nil {
		tb.Fatalf("testkit: start seaweedfs: %v", err)
	}
	endpoint, err := c.PortEndpoint(ctx, "8333/tcp", "http")
	if err != nil {
		tb.Fatalf("testkit: seaweedfs endpoint: %v", err)
	}
	store.Endpoint = endpoint
	return store
}

// Postgres starts a throwaway Postgres and returns a connection URL for a
// non-superuser that owns the database, as in the dev stack, so RLS applies
// exactly as it does in services. The container stops when tb finishes.
func Postgres(tb testing.TB) string {
	tb.Helper()
	ctx := context.Background()

	c, err := tcpostgres.Run(ctx, PostgresImage,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	testcontainers.CleanupContainer(tb, c)
	if err != nil {
		tb.Fatalf("testkit: start postgres: %v", err)
	}

	steps := []string{
		"CREATE USER yc_test_app WITH PASSWORD 'devpass'",
		"CREATE DATABASE yc_test OWNER yc_test_app",
	}
	for _, s := range steps {
		if code, _, err := c.Exec(ctx, []string{"psql", "-U", "postgres", "-c", s}); err != nil || code != 0 {
			tb.Fatalf("testkit: %s: exit %d: %v", s, code, err)
		}
	}

	host, err := c.Host(ctx)
	if err != nil {
		tb.Fatalf("testkit: postgres host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		tb.Fatalf("testkit: postgres port: %v", err)
	}
	return "postgres://yc_test_app:devpass@" + host + ":" + port.Port() + "/yc_test?sslmode=disable"
}

// NATS starts a throwaway NATS server with JetStream and returns its URL.
// The container stops when tb finishes.
func NATS(tb testing.TB) string {
	tb.Helper()
	ctx := context.Background()

	c, err := tcnats.Run(ctx, NATSImage)
	testcontainers.CleanupContainer(tb, c)
	if err != nil {
		tb.Fatalf("testkit: start nats: %v", err)
	}
	url, err := c.ConnectionString(ctx)
	if err != nil {
		tb.Fatalf("testkit: nats url: %v", err)
	}
	return url
}
