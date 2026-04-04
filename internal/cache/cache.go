package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// CacheItem represents a cached item
type CacheItem struct {
	Data      []byte
	ExpiresAt time.Time
}

// Cache interface defines cache operations
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
}

// MemoryCache implements in-memory cache
type MemoryCache struct {
	items map[string]*CacheItem
	mu    sync.RWMutex
}

// NewMemoryCache creates a new memory cache
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{
		items: make(map[string]*CacheItem),
	}
}

// Get retrieves a value from cache
func (c *MemoryCache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, ok := c.items[key]
	if !ok {
		return nil, nil
	}
	if time.Now().After(item.ExpiresAt) {
		delete(c.items, key)
		return nil, nil
	}

	return item.Data, nil
}

// Set stores a value in cache
func (c *MemoryCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items[key] = &CacheItem{
		Data:      value,
		ExpiresAt: time.Now().Add(ttl),
	}
	return nil
}

// Delete removes a value from cache
func (c *MemoryCache) Delete(ctx context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.items, key)
	return nil
}

// Exists checks if a key exists
func (c *MemoryCache) Exists(ctx context.Context, key string) (bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, ok := c.items[key]
	if !ok {
		return false, nil
	}
	if time.Now().After(item.ExpiresAt) {
		delete(c.items, key)
		return false, nil
	}
	return true, nil
}

// Clear removes all items
func (c *MemoryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*CacheItem)
}

// Size returns number of items
func (c *MemoryCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.items)
}

// GetJSON retrieves and unmarshals a JSON value
func (c *MemoryCache) GetJSON(ctx context.Context, key string, v interface{}) error {
	data, err := c.Get(ctx, key)
	if err != nil || data == nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// SetJSON marshals and stores a JSON value
func (c *MemoryCache) SetJSON(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.Set(ctx, key, data, ttl)
}

// Global cache instance
var globalCache Cache
var globalCacheMu sync.RWMutex

// Init initializes a global cache
func Init(cache Cache) {
	globalCacheMu.Lock()
	defer globalCacheMu.Unlock()
	globalCache = cache
}

// GetCache returns the global cache
func GetCache() Cache {
	globalCacheMu.RLock()
	defer globalCacheMu.RUnlock()
	return globalCache
}

// Get retrieves from global cache
func Get(ctx context.Context, key string) ([]byte, error) {
	globalCacheMu.RLock()
	defer globalCacheMu.RUnlock()
	if globalCache == nil {
		return nil, ErrNoCache
	}
	return globalCache.Get(ctx, key)
}

// Set stores in global cache
func Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	globalCacheMu.RLock()
	defer globalCacheMu.RUnlock()
	if globalCache == nil {
		return ErrNoCache
	}
	return globalCache.Set(ctx, key, value, ttl)
}

// Delete removes from global cache
func Delete(ctx context.Context, key string) error {
	globalCacheMu.RLock()
	defer globalCacheMu.RUnlock()
	if globalCache == nil {
		return ErrNoCache
	}
	return globalCache.Delete(ctx, key)
}

// Exists checks if key exists in global cache
func Exists(ctx context.Context, key string) (bool, error) {
	globalCacheMu.RLock()
	defer globalCacheMu.RUnlock()
	if globalCache == nil {
		return false, ErrNoCache
	 }
	return globalCache.Exists(ctx, key)
}

// GetJSON retrieves and unmarshals from global cache
func GetJSON(ctx context.Context, key string, v interface{}) error {
	data, err := Get(ctx, key)
	if err != nil {
		return err
	}
	if data == nil {
		return nil
	}
	return json.Unmarshal(data, v)
}

// SetJSON marshals and stores in global cache
func SetJSON(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return Set(ctx, key, data, ttl)
}

// ErrNoCache is returned when cache is not initialized
var ErrNoCache = fmt.Errorf("cache not initialized")

// DefaultTTL is the default TTL for cache entries
const DefaultTTL = 5 * time.Minute

// SessionCacheKey generates a cache key for sessions
func SessionCacheKey(sessionID string) string {
	return fmt.Sprintf("session:%s", sessionID)
}

// PermissionCacheKey generates a cache key for permissions
func PermissionCacheKey(permissionID string) string {
	return fmt.Sprintf("permission:%s", permissionID)
}

// ConfigCacheKey generates a cache key for config
func ConfigCacheKey(deviceID string) string {
	return fmt.Sprintf("config:%s", deviceID)
}
