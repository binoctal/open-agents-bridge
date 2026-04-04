package metrics

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// HealthStatus represents the health status of a component
type HealthStatus string

const (
	HealthStatusHealthy   HealthStatus = "healthy"
	HealthStatusDegraded  HealthStatus = "degraded"
	HealthStatusUnhealthy HealthStatus = "unhealthy"
)

// HealthCheck represents a health check result
type HealthCheck struct {
	Name      string       `json:"name"`
	Status    HealthStatus `json:"status"`
	Message   string       `json:"message,omitempty"`
	Timestamp int64        `json:"timestamp"`
	Duration  int64        `json:"durationMs"`
}

// HealthReport represents the overall health report
type HealthReport struct {
	Status    HealthStatus            `json:"status"`
	Timestamp int64                   `json:"timestamp"`
	Uptime    int64                   `json:"uptime"`
	Checks    map[string]HealthCheck  `json:"checks"`
	System    SystemMetrics           `json:"system"`
}

// HealthChecker is a function that performs a health check
type HealthChecker func() HealthCheck

// HealthManager manages health checks
type HealthManager struct {
	checkers map[string]HealthChecker
	mu       sync.RWMutex
	startTime time.Time
}

// NewHealthManager creates a new health manager
func NewHealthManager() *HealthManager {
	return &HealthManager{
		checkers:  make(map[string]HealthChecker),
		startTime: time.Now(),
	}
}

// RegisterChecker registers a health check
func (h *HealthManager) RegisterChecker(name string, checker HealthChecker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checkers[name] = checker
}

// RunChecks runs all health checks
func (h *HealthManager) RunChecks() *HealthReport {
	h.mu.RLock()
	defer h.mu.RUnlock()

	report := &HealthReport{
		Timestamp: time.Now().UnixMilli(),
		Uptime:    int64(time.Since(h.startTime).Seconds()),
		Checks:    make(map[string]HealthCheck),
		System:    GetSystemMetrics(),
		Status:    HealthStatusHealthy,
	}

	for name, checker := range h.checkers {
		check := checker()
		check.Name = name
		check.Timestamp = time.Now().UnixMilli()
		report.Checks[name] = check

		// Update overall status
		if check.Status == HealthStatusUnhealthy {
			report.Status = HealthStatusUnhealthy
		} else if check.Status == HealthStatusDegraded && report.Status != HealthStatusUnhealthy {
			report.Status = HealthStatusDegraded
		}
	}

	return report
}

// Handler returns an HTTP handler for health checks
func (h *HealthManager) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := h.RunChecks()

		w.Header().Set("Content-Type", "application/json")

		// Set status code based on health
		switch report.Status {
		case HealthStatusHealthy:
			w.WriteHeader(http.StatusOK)
		case HealthStatusDegraded:
			w.WriteHeader(http.StatusOK) // Still OK but degraded
		case HealthStatusUnhealthy:
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		json.NewEncoder(w).Encode(report)
	}
}

// ReadyHandler returns a simple readiness handler
func (h *HealthManager) ReadyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := h.RunChecks()

		if report.Status == HealthStatusUnhealthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("not ready"))
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
	}
}

// LiveHandler returns a simple liveness handler
func (h *HealthManager) LiveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("alive"))
	}
}

// MetricsHandler returns an HTTP handler for metrics
func MetricsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := Export()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}
}

// Global health manager
var globalHealthManager = NewHealthManager()

// RegisterHealthCheck registers a health check globally
func RegisterHealthCheck(name string, checker HealthChecker) {
	globalHealthManager.RegisterChecker(name, checker)
}

// RunHealthChecks runs all global health checks
func RunHealthChecks() *HealthReport {
	return globalHealthManager.RunChecks()
}

// HealthHandler returns the global health handler
func HealthHandler() http.HandlerFunc {
	return globalHealthManager.Handler()
}

// ReadyHandler returns the global ready handler
func ReadyHandler() http.HandlerFunc {
	return globalHealthManager.ReadyHandler()
}

// LiveHandler returns the global live handler
func LiveHandler() http.HandlerFunc {
	return globalHealthManager.LiveHandler()
}

// Common health checkers

// MemoryHealthChecker creates a memory usage health check
func MemoryHealthChecker(maxMB float64) HealthChecker {
	return func() HealthCheck {
		metrics := GetSystemMetrics()
		
		if metrics.MemoryAllocMB > maxMB {
			return HealthCheck{
				Status:   HealthStatusUnhealthy,
				Message:  "Memory usage too high",
				Duration: 0,
			}
		}

		return HealthCheck{
			Status:   HealthStatusHealthy,
			Message:  "Memory usage normal",
			Duration: 0,
		}
	}
}

// GoroutineHealthChecker creates a goroutine count health check
func GoroutineHealthChecker(maxCount int) HealthChecker {
	return func() HealthCheck {
		metrics := GetSystemMetrics()

		if metrics.GoroutineCount > maxCount {
			return HealthCheck{
				Status:   HealthStatusDegraded,
				Message:  "High goroutine count",
				Duration: 0,
			}
		}

		return HealthCheck{
			Status:   HealthStatusHealthy,
			Message:  "Goroutine count normal",
			Duration: 0,
		}
	}
}

// SessionHealthChecker creates a session health check
func SessionHealthChecker(getActiveCount func() int, maxSessions int) HealthChecker {
	return func() HealthCheck {
		count := getActiveCount()

		if count > maxSessions {
			return HealthCheck{
				Status:   HealthStatusDegraded,
				Message:  "Too many active sessions",
				Duration: 0,
			}
		}

		return HealthCheck{
			Status:   HealthStatusHealthy,
			Message:  "Session count normal",
			Duration: 0,
		}
	}
}

// WebSocketHealthChecker creates a WebSocket connection health check
func WebSocketHealthChecker(isConnected func() bool) HealthChecker {
	return func() HealthCheck {
		if isConnected() {
			return HealthCheck{
				Status:   HealthStatusHealthy,
				Message:  "WebSocket connected",
				Duration: 0,
			}
		}

		return HealthCheck{
			Status:   HealthStatusDegraded,
			Message:  "WebSocket not connected",
			Duration: 0,
		}
	}
}
