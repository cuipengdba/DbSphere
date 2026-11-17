package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestContainerReadinessProbe(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	defer server.Close()
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBSPHERE_PORT", address.Port())
	if err := probeReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = http.StatusServiceUnavailable
	if err := probeReady(context.Background()); err == nil {
		t.Fatal("unready PostgreSQL store was reported healthy")
	}
}
