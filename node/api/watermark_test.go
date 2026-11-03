package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The watermark payload must carry bootId (design D8): the API's
// node-disposability sweep keys off a CHANGE in that value, so an agent that
// silently dropped it would reset nothing after a rebuild.
func TestReportWatermarkCarriesBootID(t *testing.T) {
	var gotBootID any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Watermark Watermark `json:"watermark"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotBootID = body.Watermark.BootID
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := New(srv.URL, "agent-1", "secret")
	err := c.ReportWatermark(context.Background(), Watermark{
		MemoryAvailableBytes: 1, MemoryTotalBytes: 2, ContainersRunning: 0, BootID: "boot-abc",
	})
	if err != nil {
		t.Fatalf("ReportWatermark: %v", err)
	}
	if gotBootID != "boot-abc" {
		t.Fatalf("bootId not on the wire: got %v", gotBootID)
	}
}
