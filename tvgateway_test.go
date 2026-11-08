package main

import (
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Task 134: on the live server a tailnet laptop reached the door through
// Tailscale's subnet-router masquerade and showed up as the Docker bridge
// gateway 172.18.0.1 -- a private address, so 162756b let it in. The door
// now refuses its own gateway(s); a LAN device (DNAT keeps its address) and
// another container on the bridge still get in.
func TestTVDoorRefusesOwnGateway(t *testing.T) {
	f := newTVFix(t)
	tok := f.pair(t, "Den")
	f.a.doorRefuse = []net.IP{net.ParseIP("172.18.0.1").To4()}
	door := f.a.tvDoor(newStaticServer())
	cases := []struct {
		peer string
		ok   bool
	}{
		{"172.18.0.1:51000", false},          // tailnet via masquerade, docker-proxy
		{"[::ffff:172.18.0.1]:51000", false}, // same, as a v4-mapped peer
		{"192.168.1.27:40000", true},         // the Shield, DNAT keeps its address
		{"172.18.0.9:1", true},               // a neighbour container on the bridge
		{"172.18.0.10:1", true},              // prefix of the gateway text, not the gateway
		{"127.0.0.1:1", true},                // the container itself
	}
	for _, c := range cases {
		for _, p := range []string{"/api/tv/me", "/tv", "/tv/tv.js"} {
			req := httptest.NewRequest("GET", p, nil)
			req.RemoteAddr = c.peer
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			door.ServeHTTP(rec, req)
			if c.ok && rec.Code != 200 {
				t.Errorf("peer %s %s: %d, want 200", c.peer, p, rec.Code)
			}
			if !c.ok && rec.Code != 404 {
				t.Errorf("gateway peer %s %s: %d, want 404", c.peer, p, rec.Code)
			}
		}
		if !c.ok {
			req := httptest.NewRequest("POST", "/tv/pair", nil)
			req.RemoteAddr = c.peer
			rec := httptest.NewRecorder()
			door.ServeHTTP(rec, req)
			if rec.Code != 404 {
				t.Errorf("gateway peer %s POST /tv/pair: %d, want 404", c.peer, rec.Code)
			}
		}
	}
	// no gateway known (non-Linux, unreadable table): the private check alone
	f.a.doorRefuse = nil
	req := httptest.NewRequest("GET", "/api/tv/me", nil)
	req.RemoteAddr = "172.18.0.1:1"
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	f.a.tvDoor(newStaticServer()).ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("no refuse list: %d, want 200", rec.Code)
	}
}

func TestDefaultGateways(t *testing.T) {
	const hdr = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	cases := []struct {
		name, table string
		want        []string
	}{
		// the live holiday-map container, 11-08
		{"docker bridge", hdr +
			"eth0\t00000000\t010012AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
			"eth0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n", []string{"172.18.0.1"}},
		{"two networks", hdr +
			"eth0\t00000000\t010012AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
			"eth1\t00000000\t0100A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
			"eth0\t00000000\t010012AC\t0003\t0\t0\t200\t00000000\t0\t0\t0\n", []string{"172.18.0.1", "192.168.0.1"}},
		{"host network", hdr +
			"enp7s0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
			"enp7s0\t0001A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n", []string{"192.168.1.1"}},
		{"no default route", hdr +
			"eth0\t000012AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n", nil},
		{"default without gateway flag", hdr +
			"wg0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n", nil},
		{"garbage", hdr + "eth0\t00000000\tZZZZ\t0003\nshort\n\n", nil},
		{"empty", "", nil},
	}
	dir := t.TempDir()
	for _, c := range cases {
		p := filepath.Join(dir, "route")
		os.WriteFile(p, []byte(c.table), 0o644)
		got := defaultGateways(p)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i].String() != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
	if got := defaultGateways(filepath.Join(dir, "missing")); got != nil {
		t.Errorf("missing file: %v, want nil", got)
	}
}
