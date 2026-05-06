package ec2

import (
	"context"
	"log"
	"sync"
	"time"

	"ec2cp/internal/config"

	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
)

// Cache memoizes Snapshot per session. A background ticker re-fetches every
// session every Interval; callers can also force a single-session refresh
// synchronously via Refresh.
type Cache struct {
	env      *config.EnvConfig
	interval time.Duration
	fanout   int

	clientMu sync.Mutex
	client   *awsec2.Client

	mu        sync.RWMutex
	snapshots map[string]*Snapshot
}

func NewCache(env *config.EnvConfig, interval time.Duration, fanout int) *Cache {
	if fanout <= 0 {
		fanout = 8
	}
	return &Cache{
		env:       env,
		interval:  interval,
		fanout:    fanout,
		snapshots: make(map[string]*Snapshot),
	}
}

func (c *Cache) Run(ctx context.Context) {
	c.refreshAll(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshAll(ctx)
		}
	}
}

func (c *Cache) Get(sessionID string) *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshots[sessionID]
}

// Refresh fetches a fresh snapshot for sessionID, updates the cache, and
// returns it. Synchronous — caller blocks for ~0.5–1s on the AWS calls.
func (c *Cache) Refresh(ctx context.Context, sessionID string) *Snapshot {
	inst, err := config.GetInstance(sessionID)
	if err != nil {
		// Don't poison the cache for unknown sessions; just return a
		// one-off snapshot that carries the error.
		return &Snapshot{SessionID: sessionID, AsOf: time.Now(), FetchErr: err.Error()}
	}
	client, err := c.getClient(ctx)
	if err != nil {
		return &Snapshot{SessionID: sessionID, AsOf: time.Now(), FetchErr: err.Error()}
	}
	snap := c.refreshOne(ctx, client, sessionID, *inst)
	return snap
}

// refreshOne is the shared "fetch + store" body used by both Refresh (single
// session, on-demand) and refreshAll (every session, on a tick).
func (c *Cache) refreshOne(ctx context.Context, client *awsec2.Client, sessionID string, cfg config.InstanceConfig) *Snapshot {
	az := FirstNonEmpty(cfg.AvailabilityZone, c.env.AvailabilityZone)
	snap := Fetch(ctx, client, c.env, sessionID, cfg.AWSName(sessionID), az)
	snap.Owner = cfg.Owner
	c.set(sessionID, snap)
	return snap
}

// getClient builds the AWS client lazily on first call. On error, the client
// is left nil so the next call retries — transient credential failures
// shouldn't permanently disable the cache.
func (c *Cache) getClient(ctx context.Context) (*awsec2.Client, error) {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if c.client != nil {
		return c.client, nil
	}
	client, err := NewClient(ctx, c.env.Region)
	if err != nil {
		return nil, err
	}
	c.client = client
	return c.client, nil
}

func (c *Cache) refreshAll(ctx context.Context) {
	insts, err := config.LoadInstances()
	if err != nil {
		log.Printf("cache: load instances: %v", err)
		return
	}
	client, err := c.getClient(ctx)
	if err != nil {
		log.Printf("cache: aws client: %v", err)
		return
	}

	sem := make(chan struct{}, c.fanout)
	var wg sync.WaitGroup
	for name, instCfg := range insts {
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(name string, cfg config.InstanceConfig) {
			defer wg.Done()
			defer func() { <-sem }()
			c.refreshOne(ctx, client, name, cfg)
		}(name, instCfg)
	}
	wg.Wait()

	// Drop snapshots for sessions that have been removed from instances.json.
	c.mu.Lock()
	for k := range c.snapshots {
		if _, ok := insts[k]; !ok {
			delete(c.snapshots, k)
		}
	}
	c.mu.Unlock()
}

func (c *Cache) set(sessionID string, snap *Snapshot) {
	c.mu.Lock()
	c.snapshots[sessionID] = snap
	c.mu.Unlock()
}
