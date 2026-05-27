package cache

import (
	"context"
	"testing"
	"time"
)

func TestMemoryCache_SetGet(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()

	err := c.Set(ctx, "key1", []byte("value1"), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	val, err := c.Get(ctx, "key1")
	if err != nil {
		t.Fatal(err)
	}
	if string(val) != "value1" {
		t.Errorf("expected value1, got %s", val)
	}
}

func TestMemoryCache_Expired(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()

	err := c.Set(ctx, "key1", []byte("value1"), 1*time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	val, _ := c.Get(ctx, "key1")
	if val != nil {
		t.Error("expected nil for expired key")
	}
}

func TestMemoryCache_Delete(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()

	c.Set(ctx, "key1", []byte("value1"), 5*time.Minute)
	c.Delete(ctx, "key1")

	val, _ := c.Get(ctx, "key1")
	if val != nil {
		t.Error("expected nil after delete")
	}
}

func TestMemoryCache_Exists(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()

	exists, _ := c.Exists(ctx, "key1")
	if exists {
		t.Error("expected false for missing key")
	}

	c.Set(ctx, "key1", []byte("v"), 5*time.Minute)
	exists, _ = c.Exists(ctx, "key1")
	if !exists {
		t.Error("expected true for existing key")
	}
}

func TestMemoryCache_Clear(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()

	c.Set(ctx, "k1", []byte("v1"), 5*time.Minute)
	c.Set(ctx, "k2", []byte("v2"), 5*time.Minute)
	c.Clear()

	if c.Size() != 0 {
		t.Errorf("expected 0 after clear, got %d", c.Size())
	}
}

func TestMemoryCache_Size(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()

	if c.Size() != 0 {
		t.Errorf("expected 0, got %d", c.Size())
	}

	c.Set(ctx, "k1", []byte("v1"), 5*time.Minute)
	if c.Size() != 1 {
		t.Errorf("expected 1, got %d", c.Size())
	}
}

func TestMemoryCache_GetJSON_SetJSON(t *testing.T) {
	c := NewMemoryCache()
	ctx := context.Background()

	type data struct {
		Name string `json:"name"`
	}
	err := c.SetJSON(ctx, "json1", &data{Name: "test"}, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	var result data
	err = c.GetJSON(ctx, "json1", &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "test" {
		t.Errorf("expected test, got %s", result.Name)
	}
}

func TestCacheKeyHelpers(t *testing.T) {
	if SessionCacheKey("abc") != "session:abc" {
		t.Errorf("unexpected session key: %s", SessionCacheKey("abc"))
	}
	if PermissionCacheKey("xyz") != "permission:xyz" {
		t.Errorf("unexpected permission key: %s", PermissionCacheKey("xyz"))
	}
	if ConfigCacheKey("dev1") != "config:dev1" {
		t.Errorf("unexpected config key: %s", ConfigCacheKey("dev1"))
	}
}
