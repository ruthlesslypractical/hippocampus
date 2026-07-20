package tests

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/ruthlesslypractical/hippocampus/internal/config"
	"github.com/ruthlesslypractical/hippocampus/internal/memory"
)

// TestRedis holds the redis container and client for integration tests.
type TestRedis struct {
	Container testcontainers.Container
	Client    *redis.Client
	Store     memory.Store
	Addr      string
}

// SetupRedis starts a redis-stack container and returns a connected client + store.
// Use redis-stack to get RediSearch (FT.SEARCH) + Vector Sets (VADD/VSIM).
func SetupRedis(t *testing.T) *TestRedis {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "redis/redis-stack:latest",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "failed to start redis-stack container")

	t.Cleanup(func() {
		container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)

	port, err := container.MappedPort(ctx, "6379")
	require.NoError(t, err)

	addr := host + ":" + port.Port()

	client := redis.NewClient(&redis.Options{Addr: addr})

	// Wait for Redis to be truly ready
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := client.Ping(ctx).Err(); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.NoError(t, client.Ping(ctx).Err(), "redis not responding after container start")

	// Create a store using the test Redis
	cfg := config.RedisConfig{Addr: addr}
	store, err := memory.NewRedisStore(cfg, nil)
	require.NoError(t, err, "failed to create RedisStore")

	t.Cleanup(func() {
		store.Close()
		client.Close()
	})

	return &TestRedis{
		Container: container,
		Client:    client,
		Store:     store,
		Addr:      addr,
	}
}

// FlushAll clears all data between subtests if needed.
func (tr *TestRedis) FlushAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, tr.Client.FlushAll(ctx).Err())
}
