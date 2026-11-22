package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGatewayStopsForwardingDataAfterReplicaLosesReadiness(t *testing.T) {
	var dataRequests atomic.Int64
	backend := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/api/events":
			dataRequests.Add(1)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}
	first := httptest.NewServer(http.HandlerFunc(backend))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(backend))
	defer second.Close()
	gateway, err := newConsoleGateway(first.URL + "," + second.URL)
	if err != nil {
		t.Fatal(err)
	}
	gateway.peers[0].readiness.Store(gatewayReady)
	gateway.peers[1].readiness.Store(gatewayAlive)

	get := func(path string) int {
		t.Helper()
		got := httptest.NewRecorder()
		gateway.ServeHTTP(got, httptest.NewRequest(http.MethodGet, path, nil))
		return got.Code
	}
	if status := get("/readyz"); status != http.StatusServiceUnavailable {
		t.Fatalf("readiness after store failure = %d", status)
	}
	if status := get("/api/events"); status != http.StatusServiceUnavailable || dataRequests.Load() != 0 {
		t.Fatalf("API after readiness failure = %d with %d backend data requests", status, dataRequests.Load())
	}
	gateway.probe(context.Background())
	if status := get("/"); status != http.StatusOK {
		t.Fatalf("console shell during outage = %d", status)
	}
}
