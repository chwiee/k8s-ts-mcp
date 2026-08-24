package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "k8sts:agent:"

// Redis is the production Registry: every hub-server replica shares the
// same view through Redis, so any replica can answer Lookup for any
// cluster, no matter which replica actually holds that cluster's
// connection. Keys carry their own TTL — a replica that crashes without
// calling Forget self-heals within one TTL window instead of leaving a
// stale record forever.
type Redis struct {
	client *redis.Client
}

func NewRedis(client *redis.Client) *Redis {
	return &Redis{client: client}
}

func (r *Redis) Announce(ctx context.Context, clusterID, replicaAddr string, ttl time.Duration) error {
	if err := r.client.Set(ctx, keyPrefix+clusterID, replicaAddr, ttl).Err(); err != nil {
		return fmt.Errorf("announcing %s in redis: %w", clusterID, err)
	}
	return nil
}

func (r *Redis) Heartbeat(ctx context.Context, clusterID, replicaAddr string, ttl time.Duration) error {
	return r.Announce(ctx, clusterID, replicaAddr, ttl)
}

func (r *Redis) Lookup(ctx context.Context, clusterID string) (string, bool, error) {
	addr, err := r.client.Get(ctx, keyPrefix+clusterID).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("looking up %s in redis: %w", clusterID, err)
	}
	return addr, true, nil
}

func (r *Redis) Forget(ctx context.Context, clusterID string) error {
	if err := r.client.Del(ctx, keyPrefix+clusterID).Err(); err != nil {
		return fmt.Errorf("forgetting %s in redis: %w", clusterID, err)
	}
	return nil
}

func (r *Redis) List(ctx context.Context) ([]string, error) {
	var out []string
	iter := r.client.Scan(ctx, 0, keyPrefix+"*", 0).Iterator()
	for iter.Next(ctx) {
		out = append(out, strings.TrimPrefix(iter.Val(), keyPrefix))
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("listing clusters in redis: %w", err)
	}
	return out, nil
}
