package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
)

// MetricType represents the type of metric
type MetricType string

const (
	MetricTypeCounter   MetricType = "counter"
	MetricTypeGauge     MetricType = "gauge"
	MetricTypeHistogram MetricType = "histogram"
)

// Metric represents a single metric data point
type Metric struct {
	Name      string                 `json:"name"`
	Type      MetricType             `json:"type"`
	Value     interface{}            `json:"value"`
	Tags      map[string]string      `json:"tags,omitempty"`
	Timestamp int64                  `json:"timestamp"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// SessionMetrics tracks metrics for a single session
type SessionMetrics struct {
	SessionID       string    `json:"sessionId"`
	StartTime       time.Time `json:"startTime"`
	EndTime         time.Time `json:"endTime,omitempty"`
	MessageCount    int64     `json:"messageCount"`
	TokenUsage      TokenUsage `json:"tokenUsage"`
	PermissionCount int64     `json:"permissionCount"`
	ErrorCount      int64     `json:"errorCount"`
	ToolCallCount   int64     `json:"toolCallCount"`
}

// TokenUsage tracks token consumption
type TokenUsage struct {
	InputTokens   int64 `json:"inputTokens"`
	OutputTokens  int64 `json:"outputTokens"`
	CacheCreation int64 `json:"cacheCreation"`
	CacheRead     int64 `json:"cacheRead"`
}

// SystemMetrics tracks system-level metrics
type SystemMetrics struct {
	GoroutineCount int     `json:"goroutineCount"`
	MemoryAllocMB  float64 `json:"memoryAllocMB"`
	MemoryTotalMB  float64 `json:"memoryTotalMB"`
	MemorySysMB    float64 `json:"memorySysMB"`
	CPUUsage       float64 `json:"cpuUsage"`
	UptimeSeconds  int64   `json:"uptimeSeconds"`
}

// Collector collects and aggregates metrics
type Collector struct {
	counters   map[string]int64
	gauges     map[string]float64
	histograms map[string][]float64
	sessions   map[string]*SessionMetrics
	tags       map[string]string
	mu         sync.RWMutex
	startTime  time.Time
	hooks      []Hook
}

// Hook is a callback for metric events
type Hook func(metric Metric)

// NewCollector creates a new metrics collector
func NewCollector() *Collector {
	return &Collector{
		counters:   make(map[string]int64),
		gauges:     make(map[string]float64),
		histograms: make(map[string][]float64),
		sessions:   make(map[string]*SessionMetrics),
		tags:       make(map[string]string),
		startTime:  time.Now(),
	}
}

// SetTag sets a global tag for all metrics
func (c *Collector) SetTag(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tags[key] = value
}

// AddHook adds a hook to be called on each metric
func (c *Collector) AddHook(hook Hook) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hooks = append(c.hooks, hook)
}

// IncrementCounter increments a counter metric
func (c *Collector) IncrementCounter(name string, delta int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counters[name] += delta
	c.notifyHook(Metric{
		Name:      name,
		Type:      MetricTypeCounter,
		Value:     c.counters[name],
		Tags:      c.copyTags(),
		Timestamp: time.Now().UnixMilli(),
	})
}

// SetGauge sets a gauge metric value
func (c *Collector) SetGauge(name string, value float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gauges[name] = value
	c.notifyHook(Metric{
		Name:      name,
		Type:      MetricTypeGauge,
		Value:     value,
		Tags:      c.copyTags(),
		Timestamp: time.Now().UnixMilli(),
	})
}

// RecordHistogram records a value in a histogram
func (c *Collector) RecordHistogram(name string, value float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.histograms[name] = append(c.histograms[name], value)
	c.notifyHook(Metric{
		Name:      name,
		Type:      MetricTypeHistogram,
		Value:     value,
		Tags:      c.copyTags(),
		Timestamp: time.Now().UnixMilli(),
	})
}

// StartSession starts tracking a new session
func (c *Collector) StartSession(sessionID string) *SessionMetrics {
	c.mu.Lock()
	defer c.mu.Unlock()

	metrics := &SessionMetrics{
		SessionID: sessionID,
		StartTime: time.Now(),
	}
	c.sessions[sessionID] = metrics

	c.notifyHook(Metric{
		Name:      "session.started",
		Type:      MetricTypeCounter,
		Value:     1,
		Tags:      c.mergeTags(map[string]string{"sessionId": sessionID}),
		Timestamp: time.Now().UnixMilli(),
	})

	return metrics
}

// EndSession ends a session
func (c *Collector) EndSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if session, ok := c.sessions[sessionID]; ok {
		session.EndTime = time.Now()
		duration := session.EndTime.Sub(session.StartTime).Milliseconds()

		c.notifyHook(Metric{
			Name:  "session.ended",
			Type:  MetricTypeHistogram,
			Value: float64(duration),
			Tags:  c.mergeTags(map[string]string{"sessionId": sessionID}),
			Timestamp: time.Now().UnixMilli(),
			Metadata: map[string]interface{}{
				"messageCount":    session.MessageCount,
				"tokenUsage":      session.TokenUsage,
				"permissionCount": session.PermissionCount,
				"errorCount":      session.ErrorCount,
			},
		})
	}
}

// RecordMessage records a message in a session
func (c *Collector) RecordMessage(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if session, ok := c.sessions[sessionID]; ok {
		session.MessageCount++
	}
	c.counters["messages.total"]++
}

// RecordTokenUsage records token usage for a session
func (c *Collector) RecordTokenUsage(sessionID string, input, output, cacheCreation, cacheRead int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if session, ok := c.sessions[sessionID]; ok {
		session.TokenUsage.InputTokens += input
		session.TokenUsage.OutputTokens += output
		session.TokenUsage.CacheCreation += cacheCreation
		session.TokenUsage.CacheRead += cacheRead
	}

	c.counters["tokens.input"] += input
	c.counters["tokens.output"] += output
}

// RecordPermission records a permission request
func (c *Collector) RecordPermission(sessionID string, approved bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if session, ok := c.sessions[sessionID]; ok {
		session.PermissionCount++
	}

	status := "denied"
	if approved {
		status = "approved"
	}
	c.counters["permissions."+status]++
}

// RecordError records an error
func (c *Collector) RecordError(sessionID string, errorType string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if session, ok := c.sessions[sessionID]; ok {
		session.ErrorCount++
	}
	c.counters["errors."+errorType]++
}

// RecordToolCall records a tool call
func (c *Collector) RecordToolCall(sessionID string, toolName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if session, ok := c.sessions[sessionID]; ok {
		session.ToolCallCount++
	}
	c.counters["tools."+toolName]++
}

// GetSystemMetrics returns current system metrics
func (c *Collector) GetSystemMetrics() SystemMetrics {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return SystemMetrics{
		GoroutineCount: runtime.NumGoroutine(),
		MemoryAllocMB:  float64(m.Alloc) / 1024 / 1024,
		MemoryTotalMB:  float64(m.TotalAlloc) / 1024 / 1024,
		MemorySysMB:    float64(m.Sys) / 1024 / 1024,
		UptimeSeconds:  int64(time.Since(c.startTime).Seconds()),
	}
}

// GetCounters returns all counter values
func (c *Collector) GetCounters() map[string]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[string]int64)
	for k, v := range c.counters {
		result[k] = v
	}
	return result
}

// GetGauges returns all gauge values
func (c *Collector) GetGauges() map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[string]float64)
	for k, v := range c.gauges {
		result[k] = v
	}
	return result
}

// GetSessionMetrics returns metrics for a specific session
func (c *Collector) GetSessionMetrics(sessionID string) *SessionMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessions[sessionID]
}

// GetAllSessionMetrics returns all session metrics
func (c *Collector) GetAllSessionMetrics() map[string]*SessionMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[string]*SessionMetrics)
	for k, v := range c.sessions {
		result[k] = v
	}
	return result
}

// Export exports all metrics as JSON
func (c *Collector) Export() ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	export := struct {
		Timestamp    int64                       `json:"timestamp"`
		Uptime       int64                       `json:"uptime"`
		System       SystemMetrics               `json:"system"`
		Counters     map[string]int64            `json:"counters"`
		Gauges       map[string]float64          `json:"gauges"`
		Sessions     map[string]*SessionMetrics  `json:"sessions"`
		GlobalTags   map[string]string           `json:"globalTags"`
	}{
		Timestamp:  time.Now().UnixMilli(),
		Uptime:     int64(time.Since(c.startTime).Seconds()),
		System:     c.GetSystemMetrics(),
		Counters:   c.counters,
		Gauges:     c.gauges,
		Sessions:   c.sessions,
		GlobalTags: c.tags,
	}

	return json.Marshal(export)
}

// notifyHook notifies all registered hooks
func (c *Collector) notifyHook(metric Metric) {
	for _, hook := range c.hooks {
		go hook(metric)
	}
}

// copyTags creates a copy of global tags
func (c *Collector) copyTags() map[string]string {
	result := make(map[string]string)
	for k, v := range c.tags {
		result[k] = v
	}
	return result
}

// mergeTags merges additional tags with global tags
func (c *Collector) mergeTags(extra map[string]string) map[string]string {
	result := c.copyTags()
	for k, v := range extra {
		result[k] = v
	}
	return result
}

// Global collector instance
var globalCollector = NewCollector()

// DefaultCollector returns the global collector
func DefaultCollector() *Collector {
	return globalCollector
}

// IncrementCounter increments a counter on the global collector
func IncrementCounter(name string, delta int64) {
	globalCollector.IncrementCounter(name, delta)
}

// SetGauge sets a gauge on the global collector
func SetGauge(name string, value float64) {
	globalCollector.SetGauge(name, value)
}

// RecordHistogram records a histogram value on the global collector
func RecordHistogram(name string, value float64) {
	globalCollector.RecordHistogram(name, value)
}

// StartSession starts a session on the global collector
func StartSession(sessionID string) *SessionMetrics {
	return globalCollector.StartSession(sessionID)
}

// EndSession ends a session on the global collector
func EndSession(sessionID string) {
	globalCollector.EndSession(sessionID)
}

// RecordMessage records a message on the global collector
func RecordMessage(sessionID string) {
	globalCollector.RecordMessage(sessionID)
}

// RecordTokenUsage records token usage on the global collector
func RecordTokenUsage(sessionID string, input, output, cacheCreation, cacheRead int64) {
	globalCollector.RecordTokenUsage(sessionID, input, output, cacheCreation, cacheRead)
}

// RecordPermission records a permission on the global collector
func RecordPermission(sessionID string, approved bool) {
	globalCollector.RecordPermission(sessionID, approved)
}

// RecordError records an error on the global collector
func RecordError(sessionID string, errorType string) {
	globalCollector.RecordError(sessionID, errorType)
}

// RecordToolCall records a tool call on the global collector
func RecordToolCall(sessionID string, toolName string) {
	globalCollector.RecordToolCall(sessionID, toolName)
}

// GetSystemMetrics returns system metrics from global collector
func GetSystemMetrics() SystemMetrics {
	return globalCollector.GetSystemMetrics()
}

// Export exports all metrics from global collector
func Export() ([]byte, error) {
	return globalCollector.Export()
}

// SetGlobalTag sets a global tag on the global collector
func SetGlobalTag(key, value string) {
	globalCollector.SetTag(key, value)
}

// AddGlobalHook adds a hook to the global collector
func AddGlobalHook(hook Hook) {
	globalCollector.AddHook(hook)
}

// Init initializes the metrics system with device info
func Init(deviceID, version string) {
	SetGlobalTag("deviceId", deviceID)
	SetGlobalTag("version", version)
	SetGlobalTag("hostname", getHostname())
}

func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// Print prints current metrics (for debugging)
func Print() {
	data, err := Export()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error exporting metrics: %v\n", err)
		return
	}
	fmt.Println(string(data))
}
