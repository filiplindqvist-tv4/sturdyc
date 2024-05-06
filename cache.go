package sturdyc

import (
	"context"
	"sync"
	"time"

	"github.com/cespare/xxhash"
)

type MetricsRecorder interface {
	CacheHit()
	CacheMiss()
	Eviction()
	ForcedEviction()
	EntriesEvicted(int)
	ShardIndex(int)
	CacheBatchRefreshSize(size int)
	ObserveCacheSize(callback func() int)
}

// FetchFn Fetch represents a function that can be used to fetch a single record from a data source.
type FetchFn[T any] func(ctx context.Context) (T, error)

// BatchFetchFn represents a function that can be used to fetch multiple records from a data source.
type BatchFetchFn[T any] func(ctx context.Context, ids []string) (map[string]T, error)

type BatchResponse[T any] map[string]T

// KeyFn is called invoked for each record that a batch fetch
// operation returns. It is used to create unique cache keys.
type KeyFn func(id string) string

// Config represents the configuration that can be applied to the cache.
type Config struct {
	clock            Clock
	evictionInterval time.Duration
	metricsRecorder  MetricsRecorder

	refreshesEnabled bool
	minRefreshTime   time.Duration
	maxRefreshTime   time.Duration
	retryBaseDelay   time.Duration
	storeMisses      bool

	bufferRefreshes       bool
	batchMutex            sync.Mutex
	batchSize             int
	bufferTimeout         time.Duration
	bufferPermutationIDs  map[string][]string
	bufferPermutationChan map[string]chan<- []string

	passthroughPercentage int
	passthroughBuffering  bool

	useRelativeTimeKeyFormat bool
	keyTruncation            time.Duration
	getSize                  func() int
}

// Client represents a cache client that can be used to store and retrieve values.
type Client[T any] struct {
	*Config
	ttl       time.Duration
	shards    []*shard[T]
	nextShard int
}

// New creates a new Client instance with the specified configuration.
//
// `capacity` defines the maximum number of entries that the cache can store.
// `numShards` Is used to set the number of shards. Has to be greater than 0.
// `ttl` Sets the time to live for each entry in the cache. Has to be greater than 0.
// `evictionPercentage` Percentage of items to evict when the cache exceeds its capacity.
// `opts` allows for additional configurations to be applied to the cache client.
func New[T any](capacity, numShards int, ttl time.Duration, evictionPercentage int, opts ...Option) *Client[T] {
	validateArgs(capacity, numShards, ttl, evictionPercentage)

	client := &Client[T]{ttl: ttl}

	// Create a default configuration.
	cfg := &Config{
		clock:                 NewClock(),
		passthroughPercentage: 100,
		evictionInterval:      ttl / time.Duration(numShards),
		getSize:               client.Size,
	}

	client.Config = cfg
	for _, opt := range opts {
		opt(cfg)
	}

	// We create the shards after we've applied the options to ensure that the correct config is used.
	shardSize := capacity / numShards
	shards := make([]*shard[T], numShards)
	for i := 0; i < numShards; i++ {
		shards[i] = newShard[T](shardSize, ttl, evictionPercentage, cfg)
	}
	client.shards = shards
	client.nextShard = 0

	// Run evictions in a separate goroutine.
	client.startEvictions()

	return client
}

// Size returns the number of entries in the cache.
func (c *Client[T]) Size() int {
	var sum int
	for _, shard := range c.shards {
		sum += shard.size()
	}
	return sum
}

// Delete removes a single entry from the cache.
func (c *Client[T]) Delete(key string) {
	shard := c.getShard(key)
	shard.delete(key)
}

// startEvictions is going to be running in a separate goroutine that we're going to prevent from ever exiting.
func (c *Client[T]) startEvictions() {
	go func() {
		ticker, stop := c.clock.NewTicker(c.evictionInterval)
		defer stop()
		for range ticker {
			if c.metricsRecorder != nil {
				c.metricsRecorder.Eviction()
			}
			c.shards[c.nextShard].evictExpired()
			c.nextShard = (c.nextShard + 1) % len(c.shards)
		}
	}()
}

// getShard returns the shard that should be used for the specified key.
func (c *Client[T]) getShard(key string) *shard[T] {
	hash := xxhash.Sum64String(key)
	shardIndex := hash % uint64(len(c.shards))
	if c.metricsRecorder != nil {
		c.metricsRecorder.ShardIndex(int(shardIndex))
	}
	return c.shards[shardIndex]
}

// reportCacheHits is used to report cache hits and misses to the metrics recorder.
func (c *Client[T]) reportCacheHits(cacheHit bool) {
	if c.metricsRecorder == nil {
		return
	}
	if !cacheHit {
		c.metricsRecorder.CacheMiss()
		return
	}
	c.metricsRecorder.CacheHit()
}

// set writes a single value to the cache. Returns true if it triggered an eviction.
func (c *Client[T]) set(key string, value T, isMissingRecord bool) bool {
	shard := c.getShard(key)
	return shard.set(key, value, isMissingRecord)
}
