package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type gatewayPeer struct {
	url       *url.URL
	proxy     *httputil.ReverseProxy
	readiness atomic.Int32
}

type consoleGateway struct {
	peers  []*gatewayPeer
	next   atomic.Uint64
	probes *http.Client
}

const (
	gatewayOffline = iota
	gatewayAlive
	gatewayReady
)

func newConsoleGateway(upstreams string) (*consoleGateway, error) {
	gateway := &consoleGateway{probes: &http.Client{Timeout: 1200 * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	seen := map[string]bool{}
	for _, raw := range strings.Split(upstreams, ",") {
		peerURL, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || peerURL.Host == "" || (peerURL.Scheme != "http" && peerURL.Scheme != "https") ||
			(peerURL.Path != "" && peerURL.Path != "/") || peerURL.User != nil || peerURL.RawQuery != "" || peerURL.Fragment != "" {
			return nil, errors.New("DBSPHERE_GATEWAY_UPSTREAMS must contain base HTTP or HTTPS replica URLs")
		}
		peerURL.Path = ""
		if seen[peerURL.String()] {
			return nil, errors.New("Gateway replica URLs must be distinct")
		}
		seen[peerURL.String()] = true
		peer := &gatewayPeer{url: peerURL}
		peer.proxy = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(peerURL)
				r.Out.Host = r.In.Host
				r.SetXForwarded()
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				peer.readiness.Store(gatewayOffline)
				gatewayUnavailable(w)
			},
		}
		gateway.peers = append(gateway.peers, peer)
	}
	if len(gateway.peers) < 2 || len(gateway.peers) > 16 {
		return nil, errors.New("Gateway requires between 2 and 16 replicas")
	}
	return gateway, nil
}

func (g *consoleGateway) probe(ctx context.Context) {
	var group sync.WaitGroup
	for _, peer := range g.peers {
		group.Go(func() {
			switch {
			case g.ok(ctx, peer.url.String()+"/readyz"):
				peer.readiness.Store(gatewayReady)
			case g.ok(ctx, peer.url.String()+"/healthz"):
				peer.readiness.Store(gatewayAlive)
			default:
				peer.readiness.Store(gatewayOffline)
			}
		})
	}
	group.Wait()
}

func (g *consoleGateway) ok(ctx context.Context, address string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false
	}
	res, err := g.probes.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusOK
}

func gatewayUnavailable(w http.ResponseWriter) {
	w.Header().Set("X-DbSphere-Storage", "unavailable")
	gatewayJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": "DbSphere replicas or long-term storage are unavailable"})
}

func gatewayJSON(w http.ResponseWriter, status int, value map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (g *consoleGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		gatewayJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	offset := int(g.next.Add(1) % uint64(len(g.peers)))
	var live *gatewayPeer
	for i := range len(g.peers) {
		peer := g.peers[(i+offset)%len(g.peers)]
		switch peer.readiness.Load() {
		case gatewayReady:
			peer.proxy.ServeHTTP(w, r)
			return
		case gatewayAlive:
			live = peer
		}
	}
	if live != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		!strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/readyz" {
		live.proxy.ServeHTTP(w, r)
		return
	}
	gatewayUnavailable(w)
}

func runGateway() error {
	gateway, err := newConsoleGateway(os.Getenv("DBSPHERE_GATEWAY_UPSTREAMS"))
	if err != nil {
		return err
	}
	host := os.Getenv("DBSPHERE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("DBSPHERE_PORT")
	if port == "" {
		port = "8080"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gateway.probe(ctx)
	go func() {
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				gateway.probe(ctx)
			}
		}
	}()
	server := &http.Server{Addr: net.JoinHostPort(host, port), Handler: gateway, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve console gateway: %w", err)
	}
	return nil
}
