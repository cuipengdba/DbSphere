package httpapi

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGrafanaReturnWithOrWithoutFrameHeaders(t *testing.T) {
	const asset = "00000000-0000-4000-8000-000000000222"
	const event = "00000000-0000-4000-8000-000000000111"
	const from = "2026-11-25T04:44:01.331Z"
	const to = "2026-11-25T05:44:01.331Z"
	mux := http.NewServeMux()
	registerGrafana(mux, nil, &AuthConfig{})
	for _, tc := range []struct {
		name, view, eventID, source, dest, accept, fragment string
		bridge                                              bool
	}{
		{"direct", "live", "", "", "document", "text/html", "live?", false},
		{"embedded", "live", "", "", "iframe", "text/html", "live?", true},
		{"proxy removes frame header", "event", "", "", "", "text/html", "live?", true},
		{"event source", "event", event, "", "iframe", "text/html", "incidents?", true},
		{"unlinked storage", "event", "", "storage", "", "text/html", "storage?", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := url.Values{"view": {tc.view}, "event": {tc.eventID}, "asset": {asset}, "from": {from}, "to": {to}, "source": {tc.source}}
			r := httptest.NewRequest(http.MethodGet, "https://dbsphere.test:8443"+grafanaReturnPath+"?"+input.Encode(), nil)
			r.Header.Set("Sec-Fetch-Dest", tc.dest)
			r.Header.Set("Accept", tc.accept)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			location := w.Header().Get("Location")
			if tc.bridge {
				if w.Code != http.StatusOK || w.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
					t.Fatalf("return bridge failed: %d %v", w.Code, w.Header())
				}
				_, after, _ := strings.Cut(w.Body.String(), `id="dbsphere-grafana-return" href="`)
				href, _, _ := strings.Cut(after, `"`)
				location = html.UnescapeString(href)
			} else if w.Code != http.StatusSeeOther {
				t.Fatalf("expected direct redirect, got %d", w.Code)
			}
			if !strings.HasPrefix(location, "/#"+tc.fragment) {
				t.Fatalf("wrong return route: %s", location)
			}
			_, raw, _ := strings.Cut(location, "?")
			query, _ := url.ParseQuery(raw)
			endKey := "end"
			if strings.HasPrefix(tc.fragment, "storage") || tc.eventID != "" {
				endKey = "to"
			}
			if query.Get("asset") != asset || query.Get("event") != tc.eventID || query.Get("from") != from || query.Get(endKey) != to {
				t.Fatalf("lost history or event context: %s", location)
			}
		})
	}
}
