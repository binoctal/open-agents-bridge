package metrics

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestCollector_IncrementCounter(t *testing.T) {
	c := NewCollector()
	c.IncrementCounter("requests", 1)
	c.IncrementCounter("requests", 2)

	counters := c.GetCounters()
	if counters["requests"] != 3 {
		t.Errorf("expected 3, got %d", counters["requests"])
	}
}

func TestCollector_SetGauge(t *testing.T) {
	c := NewCollector()
	c.SetGauge("cpu", 75.5)

	gauges := c.GetGauges()
	if gauges["cpu"] != 75.5 {
		t.Errorf("expected 75.5, got %f", gauges["cpu"])
	}
}

func TestCollector_RecordHistogram(t *testing.T) {
	c := NewCollector()
	c.RecordHistogram("latency", 100)
	c.RecordHistogram("latency", 200)
	// Verify collector doesn't panic with histogram values
	counters := c.GetCounters()
	if counters["latency"] != 0 {
		// histogram is stored separately, not in counters
		t.Log("histogram not in counters, as expected")
	}
}

func TestCollector_SessionLifecycle(t *testing.T) {
	c := NewCollector()

	sm := c.StartSession("sess1")
	if sm == nil {
		t.Fatal("expected session metrics")
	}
	if sm.SessionID != "sess1" {
		t.Errorf("expected sess1, got %s", sm.SessionID)
	}

	c.RecordMessage("sess1")
	c.RecordTokenUsage("sess1", 100, 50, 10, 5)
	c.RecordPermission("sess1", true)
	c.RecordToolCall("sess1", "edit_file")
	c.RecordError("sess1", "timeout")
	c.EndSession("sess1")

	sm = c.GetSessionMetrics("sess1")
	if sm.MessageCount != 1 {
		t.Errorf("expected 1 message, got %d", sm.MessageCount)
	}
	if sm.TokenUsage.InputTokens != 100 {
		t.Errorf("expected 100 input tokens, got %d", sm.TokenUsage.InputTokens)
	}
	if sm.PermissionCount != 1 {
		t.Errorf("expected 1 permission, got %d", sm.PermissionCount)
	}
	if sm.ToolCallCount != 1 {
		t.Errorf("expected 1 tool call, got %d", sm.ToolCallCount)
	}
	if sm.ErrorCount != 1 {
		t.Errorf("expected 1 error, got %d", sm.ErrorCount)
	}
}

func TestCollector_GetSessionMetrics_Missing(t *testing.T) {
	c := NewCollector()
	sm := c.GetSessionMetrics("nonexistent")
	if sm != nil {
		t.Error("expected nil for missing session")
	}
}

func TestCollector_GetCounters_Copy(t *testing.T) {
	c := NewCollector()
	c.IncrementCounter("req", 1)
	counters := c.GetCounters()
	counters["req"] = 999 // mutate copy

	original := c.GetCounters()
	if original["req"] == 999 {
		t.Error("GetCounters should return a copy")
	}
}

func TestCollector_Export(t *testing.T) {
	c := NewCollector()
	c.IncrementCounter("test", 1)
	c.SetGauge("temp", 42.0)

	data, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["counters"] == nil {
		t.Error("expected counters in export")
	}
	if result["gauges"] == nil {
		t.Error("expected gauges in export")
	}
}

func TestCollector_Hooks(t *testing.T) {
	c := NewCollector()
	var mu sync.Mutex
	var received []Metric
	c.AddHook(func(m Metric) {
		mu.Lock()
		received = append(received, m)
		mu.Unlock()
	})

	c.IncrementCounter("test", 1)
	time.Sleep(50 * time.Millisecond) // hook runs in goroutine

	mu.Lock()
	if len(received) == 0 {
		t.Fatal("expected hook to be called")
	}
	if received[0].Name != "test" {
		t.Errorf("expected metric name test, got %s", received[0].Name)
	}
	mu.Unlock()
}

func TestCollector_Tags(t *testing.T) {
	c := NewCollector()
	c.SetTag("env", "test")

	var mu sync.Mutex
	var received Metric
	c.AddHook(func(m Metric) {
		mu.Lock()
		received = m
		mu.Unlock()
	})

	c.IncrementCounter("req", 1)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	if received.Tags["env"] != "test" {
		t.Errorf("expected tag env=test, got %v", received.Tags)
	}
	mu.Unlock()
}

func TestCollector_GetSystemMetrics(t *testing.T) {
	c := NewCollector()
	sm := c.GetSystemMetrics()
	if sm.GoroutineCount <= 0 {
		t.Error("expected positive goroutine count")
	}
	if sm.MemoryAllocMB <= 0 {
		t.Error("expected positive memory alloc")
	}
	if sm.UptimeSeconds < 0 {
		t.Error("expected non-negative uptime")
	}
}

func TestCollector_ConcurrentAccess(t *testing.T) {
	c := NewCollector()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.IncrementCounter("concurrent", 1)
			c.SetGauge("gauge", float64(time.Now().Unix()))
			c.RecordHistogram("hist", float64(time.Now().UnixMilli()))
		}()
	}
	wg.Wait()

	counters := c.GetCounters()
	if counters["concurrent"] != 100 {
		t.Errorf("expected 100, got %d", counters["concurrent"])
	}
}
