package httpapi

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type AdministratorStore interface {
	CurrentAdminPasswordHash(context.Context) (string, error)
	CompleteAdministratorSetup(context.Context, string) (string, error)
}

type administratorHashKey struct{}

// Compose publishes the application only on host loopback. A request through
// Docker's host port can reach the container with the bridge gateway as peer.
func dockerSetupBridge() netip.Addr {
	contents, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return netip.Addr{}
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		gateway, err := strconv.ParseUint(fields[2], 16, 32)
		if err == nil && gateway != 0 {
			var ip [4]byte
			binary.LittleEndian.PutUint32(ip[:], uint32(gateway))
			return netip.AddrFrom4(ip)
		}
	}
	return netip.Addr{}
}

func localSetupAllowed(r *http.Request, bridge netip.Addr) bool {
	authority := r.Host
	if host, _, err := net.SplitHostPort(authority); err == nil {
		authority = host
	}
	host, _ := netip.ParseAddr(strings.Trim(authority, "[]"))
	if !host.Unmap().IsLoopback() && !strings.EqualFold(authority, "localhost") {
		return false
	}
	peerHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, err := netip.ParseAddr(peerHost)
	if err != nil || len(r.Header.Values("Forwarded")) > 0 || len(r.Header.Values("X-Real-IP")) > 0 {
		return false
	}
	peer = peer.Unmap()
	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		if len(r.Header.Values("X-Forwarded-Host")) > 0 || len(r.Header.Values("X-Forwarded-Proto")) > 0 ||
			len(r.Header.Values("X-Forwarded-Port")) > 0 {
			return false
		}
		return peer.IsLoopback() || (bridge.IsValid() && peer == bridge)
	}
	// The bundled HA gateway replaces forwarding headers using its TCP peer.
	// Resolve its private service address before trusting that one-hop value.
	if !bridge.IsValid() || len(forwarded) != 1 {
		return false
	}
	client, err := netip.ParseAddr(strings.TrimSpace(forwarded[0]))
	if err != nil || (!client.Unmap().IsLoopback() && client.Unmap() != bridge) {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	gateways, err := net.DefaultResolver.LookupNetIP(ctx, "ip", "gateway")
	if err != nil {
		return false
	}
	for _, gateway := range gateways {
		if gateway.Unmap() == peer {
			return true
		}
	}
	return false
}
