package alert

import (
	"testing"
	"time"
)

func TestManager_SendStoresAlert(t *testing.T) {
	m := NewManager(Config{Enabled: true, Cooldown: 0, MaxAlerts: 100})
	err := m.Send(Alert{Level: LevelWarning, Type: "test", Title: "Test", Message: "msg"})
	if err != nil {
		t.Fatal(err)
	}
	alerts := m.GetAlerts(0)
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}
	if alerts[0].Title != "Test" {
		t.Errorf("expected Test, got %s", alerts[0].Title)
	}
}

func TestManager_CooldownPreventsDuplication(t *testing.T) {
	m := NewManager(Config{Enabled: true, Cooldown: 1 * time.Hour, MaxAlerts: 100})
	m.Send(Alert{Type: "dup", Title: "First"})
	m.Send(Alert{Type: "dup", Title: "Second"})

	alerts := m.GetAlerts(0)
	if len(alerts) != 1 {
		t.Errorf("expected 1 alert due to cooldown, got %d", len(alerts))
	}
	if alerts[0].Title != "First" {
		t.Errorf("expected First, got %s", alerts[0].Title)
	}
}

func TestManager_MaxAlertsTrimming(t *testing.T) {
	m := NewManager(Config{Enabled: true, Cooldown: 0, MaxAlerts: 3})
	m.Send(Alert{Type: "a", Title: "1"})
	m.Send(Alert{Type: "b", Title: "2"})
	m.Send(Alert{Type: "c", Title: "3"})
	m.Send(Alert{Type: "d", Title: "4"})

	alerts := m.GetAlerts(0)
	if len(alerts) != 3 {
		t.Fatalf("expected 3 alerts, got %d", len(alerts))
	}
	if alerts[0].Title == "1" {
		t.Error("oldest alert should have been trimmed")
	}
}

func TestManager_DisabledSkips(t *testing.T) {
	m := NewManager(Config{Enabled: false, Cooldown: 0, MaxAlerts: 100})
	err := m.Send(Alert{Type: "test", Title: "Ignored"})
	if err != nil {
		t.Fatal(err)
	}
	alerts := m.GetAlerts(0)
	if len(alerts) != 0 {
		t.Error("disabled manager should not store alerts")
	}
}

func TestManager_RegisterHandler(t *testing.T) {
	m := NewManager(Config{Enabled: true, Cooldown: 0, MaxAlerts: 100})
	called := false
	m.RegisterHandler(&testHandler{fn: func(a Alert) error {
		called = true
		return nil
	}})
	m.Send(Alert{Type: "test", Title: "Trigger"})
	if !called {
		t.Error("handler was not called")
	}
}

func TestManager_GetAlerts_WithLimit(t *testing.T) {
	m := NewManager(Config{Enabled: true, Cooldown: 0, MaxAlerts: 100})
	m.Send(Alert{Type: "a", Title: "1"})
	m.Send(Alert{Type: "b", Title: "2"})
	m.Send(Alert{Type: "c", Title: "3"})

	alerts := m.GetAlerts(2)
	if len(alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(alerts))
	}
	if alerts[0].Title != "2" {
		t.Errorf("expected last 2, got %s first", alerts[0].Title)
	}
}

func TestManager_Clear(t *testing.T) {
	m := NewManager(Config{Enabled: true, Cooldown: 0, MaxAlerts: 100})
	m.Send(Alert{Type: "a", Title: "Test"})
	m.Clear()
	if len(m.GetAlerts(0)) != 0 {
		t.Error("expected empty after clear")
	}
}

func TestManager_AutoTimestampAndID(t *testing.T) {
	m := NewManager(Config{Enabled: true, Cooldown: 0, MaxAlerts: 100})
	m.Send(Alert{Type: "auto", Title: "Auto"})
	alerts := m.GetAlerts(0)
	if alerts[0].Timestamp == 0 {
		t.Error("expected auto-populated timestamp")
	}
	if alerts[0].ID == "" {
		t.Error("expected auto-populated ID")
	}
}

type testHandler struct {
	fn func(Alert) error
}

func (h *testHandler) Name() string       { return "test" }
func (h *testHandler) Send(a Alert) error { return h.fn(a) }
