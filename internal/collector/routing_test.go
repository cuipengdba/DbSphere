package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHAProxyStatsScopesCSVAndDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stats;csv":
			if user, pass, ok := r.BasicAuth(); !ok || user != "reader" || pass != "test" {
				t.Error("statistics authentication missing")
			}
			fmt.Fprint(w, "# pxname,svname,qcur,qmax,status,addr,check_status,scur,slim,\n"+
				"other,wrong,4,8,DOWN,192.0.2.5:5432,L4CON,1,10,\n"+
				"pg,replica,0,8,UP,192.0.2.6:5432,L4OK,1,10,\n"+
				"pg,BACKEND,1,9,UP,,,3,20,\n")
		case "/redirect;csv":
			http.Redirect(w, r, server.URL+"/redirected", http.StatusFound)
		case "/redirected":
			redirected.Store(true)
		default:
			t.Errorf("unexpected statistics request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	stats := HAProxyStats(context.Background(), "http://reader:test@"+server.Listener.Addr().String()+"/stats", "pg")
	rows, ok := stats["rows"].([]map[string]any)
	if stats["status"] != "ok" || !ok || len(rows) != 2 || rows[0]["check_status"] != "L4OK" ||
		rows[0]["qcur"] != 0 || rows[1]["qcur"] != 1 || rows[1]["qmax"] != 9 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	stats = HAProxyStats(context.Background(), server.URL+"/redirect", "pg")
	if stats["status"] != "unavailable" || redirected.Load() {
		t.Fatalf("redirect followed: %+v", stats)
	}
}
