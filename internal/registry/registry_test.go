package registry

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// impl pairs a Registry with a way to make time pass for it: real
// time.Sleep for InMemory (which checks the real clock), and miniredis's
// FastForward for Redis (miniredis only expires keys when its simulated
// clock is advanced, not on a real-time ticker) — this lets the same TTL
// test run correctly against both without actually sleeping through a
// simulated server's TTL.
type impl struct {
	Registry
	advance func(time.Duration)
}

// implementations returns every Registry implementation under test, so the
// same behavioral test suite runs against both — a Redis-specific bug (e.g.
// key prefix collision, TTL semantics) should fail the same tests as an
// InMemory bug would.
func implementations(t *testing.T) map[string]impl {
	t.Helper()

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	return map[string]impl{
		"InMemory": {Registry: NewInMemory(), advance: func(d time.Duration) { time.Sleep(d) }},
		"Redis":    {Registry: NewRedis(rdb), advance: mr.FastForward},
	}
}

func TestRegistry_AnnounceLookupForget(t *testing.T) {
	for name, reg := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			if _, ok, err := reg.Lookup(ctx, "spoke-1"); err != nil || ok {
				t.Fatalf("Lookup before Announce = ok=%v err=%v, want ok=false", ok, err)
			}

			if err := reg.Announce(ctx, "spoke-1", "10.0.0.1:7443", time.Minute); err != nil {
				t.Fatalf("Announce: %v", err)
			}
			addr, ok, err := reg.Lookup(ctx, "spoke-1")
			if err != nil || !ok || addr != "10.0.0.1:7443" {
				t.Fatalf("Lookup after Announce = addr=%q ok=%v err=%v, want 10.0.0.1:7443/true", addr, ok, err)
			}

			if err := reg.Forget(ctx, "spoke-1"); err != nil {
				t.Fatalf("Forget: %v", err)
			}
			if _, ok, err := reg.Lookup(ctx, "spoke-1"); err != nil || ok {
				t.Fatalf("Lookup after Forget = ok=%v err=%v, want ok=false", ok, err)
			}
		})
	}
}

func TestRegistry_TTLExpires(t *testing.T) {
	for name, reg := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := reg.Announce(ctx, "spoke-1", "10.0.0.1:7443", 50*time.Millisecond); err != nil {
				t.Fatalf("Announce: %v", err)
			}
			reg.advance(150 * time.Millisecond)
			if _, ok, err := reg.Lookup(ctx, "spoke-1"); err != nil || ok {
				t.Fatalf("Lookup after TTL expiry = ok=%v err=%v, want ok=false (self-heal on replica crash without Forget)", ok, err)
			}
		})
	}
}

func TestRegistry_HeartbeatRefreshesTTL(t *testing.T) {
	for name, reg := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := reg.Announce(ctx, "spoke-1", "10.0.0.1:7443", 100*time.Millisecond); err != nil {
				t.Fatalf("Announce: %v", err)
			}
			// Heartbeat twice, each within the previous TTL window — should
			// never expire if the heartbeat loop is keeping up.
			reg.advance(60 * time.Millisecond)
			if err := reg.Heartbeat(ctx, "spoke-1", "10.0.0.1:7443", 100*time.Millisecond); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}
			reg.advance(60 * time.Millisecond)
			if _, ok, err := reg.Lookup(ctx, "spoke-1"); err != nil || !ok {
				t.Fatalf("Lookup after heartbeat = ok=%v err=%v, want ok=true (heartbeat should have kept it alive)", ok, err)
			}
		})
	}
}

func TestRegistry_List(t *testing.T) {
	for name, reg := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, id := range []string{"spoke-1", "spoke-2", "spoke-3"} {
				if err := reg.Announce(ctx, id, "10.0.0.1:7443", time.Minute); err != nil {
					t.Fatalf("Announce(%s): %v", id, err)
				}
			}
			got, err := reg.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			sort.Strings(got)
			want := []string{"spoke-1", "spoke-2", "spoke-3"}
			if len(got) != len(want) {
				t.Fatalf("List() = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("List() = %v, want %v", got, want)
				}
			}
		})
	}
}
