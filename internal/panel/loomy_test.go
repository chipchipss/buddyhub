package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPanelLoomyStatus(t *testing.T) {
	p := newTestPanel()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/loomy/status", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}

	var resp struct {
		OK         bool `json:"ok"`
		HasAccount bool `json:"has_account"`
		Status     struct {
			Earned int `json:"earned"`
			Total  int `json:"total"`
		} `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.OK {
		t.Errorf("resp.OK = false, want true")
	}
	if resp.Status.Total != 10000 {
		t.Errorf("status.Total = %d, want 10000", resp.Status.Total)
	}
}
