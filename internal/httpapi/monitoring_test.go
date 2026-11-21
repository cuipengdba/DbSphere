package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMonitoringSeriesRejectsOutOfScopeAssetBeforeReading(t *testing.T) {
	const a = "172fe64e-ffb0-419f-8d92-9fa895defc02"
	const b = "6750e811-d1c0-4ecb-a0e8-5d3d167ae246"
	scope := map[string]bool{a: true}
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"asset=" + a + "&asset=" + b, http.StatusNotFound},
		{"asset=" + a + "&asset=" + a, http.StatusBadRequest},
		{"asset=bad-id", http.StatusBadRequest},
		{"", http.StatusBadRequest},
		{"asset=" + a, http.StatusNotImplemented},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/monitoring/series?"+tc.query, nil)
		readMonitoringSeries(w, r, nil, scope)
		if w.Code != tc.status {
			t.Errorf("%s returned %d, want %d", tc.query, w.Code, tc.status)
		}
	}
}
