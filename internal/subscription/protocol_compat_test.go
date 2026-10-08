package subscription

import (
	"testing"
)

// TestNormalizeShadowsocksMethod_Aliases pins the alias handling that keeps
// Xray/mihomo-style cipher names usable by sing-box.
//
// Regression: sing-box only registers the canonical `*-ietf-*` AEAD spellings.
// Xray and mihomo additionally accept `chacha20-poly1305` / `xchacha20-poly1305`,
// so subscriptions written for those clients produced an outbound that failed to
// build ("unknown method"), which meant the node was never probed and stayed
// circuit-open forever.
func TestNormalizeShadowsocksMethod_Aliases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"chacha20-poly1305", "chacha20-ietf-poly1305"},
		{"CHACHA20-POLY1305", "chacha20-ietf-poly1305"},
		{"xchacha20-poly1305", "xchacha20-ietf-poly1305"},
		{"XChaCha20-Poly1305", "xchacha20-ietf-poly1305"},
		// canonical spellings must be untouched
		{"chacha20-ietf-poly1305", "chacha20-ietf-poly1305"},
		{"xchacha20-ietf-poly1305", "xchacha20-ietf-poly1305"},
		{"aes-128-gcm", "aes-128-gcm"},
		{"aes-256-cfb", "aes-256-cfb"},
		{"rc4-md5", "rc4-md5"},
		{"2022-blake3-aes-256-gcm", "2022-blake3-aes-256-gcm"},
		// Quantumult-style AEAD_ names
		{"AEAD_CHACHA20_POLY1305", "chacha20-ietf-poly1305"},
		{"AEAD_AES_256_GCM", "aes-256-gcm"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := normalizeShadowsocksMethod(tc.in); got != tc.want {
			t.Errorf("normalizeShadowsocksMethod(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseShadowsocksMethodAlias_EndToEnd verifies the alias reaches the
// emitted outbound through both the Clash and the URI entry points.
func TestParseShadowsocksMethodAlias_EndToEnd(t *testing.T) {
	t.Run("clash json", func(t *testing.T) {
		data := []byte(`{
			"proxies": [
				{"name": "ss-alias", "type": "ss", "server": "1.1.1.1", "port": 8388,
				 "cipher": "chacha20-poly1305", "password": "pass"}
			]
		}`)

		nodes, err := ParseGeneralSubscription(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 1 {
			t.Fatalf("expected 1 node, got %d", len(nodes))
		}
		obj := parseNodeRaw(t, nodes[0].RawOptions)
		if got := obj["method"]; got != "chacha20-ietf-poly1305" {
			t.Fatalf("method: got %v want chacha20-ietf-poly1305", got)
		}
	})

	t.Run("uri", func(t *testing.T) {
		data := []byte("ss://chacha20-poly1305:pass@1.1.1.1:8388#ss-alias")

		nodes, err := ParseGeneralSubscription(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 1 {
			t.Fatalf("expected 1 node, got %d", len(nodes))
		}
		obj := parseNodeRaw(t, nodes[0].RawOptions)
		if got := obj["method"]; got != "chacha20-ietf-poly1305" {
			t.Fatalf("method: got %v want chacha20-ietf-poly1305", got)
		}
	})
}

// TestNormalizeVLESSFlow_UDP443Variant pins the `-udp443` flow alias.
//
// Regression: sing-box rejects "xtls-rprx-vision-udp443" with
// "unsupported flow", so a node exported by Xray with that flow could not be
// built and stayed circuit-open forever. On the wire the flow is identical, so
// normalizing to the plain spelling is correct.
func TestNormalizeVLESSFlow_UDP443Variant(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"xtls-rprx-vision", "xtls-rprx-vision"},
		{"xtls-rprx-vision-udp443", "xtls-rprx-vision"},
		{"XTLS-RPRX-VISION-UDP443", "xtls-rprx-vision"},
		{"", ""},
		{"none", ""},
		{"None", ""},
		{"null", ""},
		{"xtls-rprx-direct", ""},
	}

	for _, tc := range cases {
		if got := normalizeVLESSFlow(tc.in); got != tc.want {
			t.Errorf("normalizeVLESSFlow(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseVLESSFlowUDP443_EndToEnd(t *testing.T) {
	t.Run("clash json", func(t *testing.T) {
		data := []byte(`{
			"proxies": [
				{"name": "vless-udp443", "type": "vless", "server": "203.0.113.20", "port": 443,
				 "uuid": "11111111-2222-3333-4444-555555555556", "tls": true,
				 "flow": "xtls-rprx-vision-udp443", "network": "tcp", "servername": "example.com"}
			]
		}`)

		nodes, err := ParseGeneralSubscription(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 1 {
			t.Fatalf("expected 1 node, got %d", len(nodes))
		}
		obj := parseNodeRaw(t, nodes[0].RawOptions)
		if got := obj["flow"]; got != "xtls-rprx-vision" {
			t.Fatalf("flow: got %v want xtls-rprx-vision", got)
		}
	})

	t.Run("uri", func(t *testing.T) {
		data := []byte(
			"vless://11111111-2222-3333-4444-555555555557@example.com:443?type=tcp&security=tls&sni=example.com&flow=xtls-rprx-vision-udp443",
		)

		nodes, err := ParseGeneralSubscription(data)
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 1 {
			t.Fatalf("expected 1 node, got %d", len(nodes))
		}
		obj := parseNodeRaw(t, nodes[0].RawOptions)
		if got := obj["flow"]; got != "xtls-rprx-vision" {
			t.Fatalf("flow: got %v want xtls-rprx-vision", got)
		}
	})
}

// TestParseClashTUICCongestionControlNormalized pins lowercase normalization of
// the TUIC congestion control algorithm.
//
// Regression: sing-box rejects mixed-case values with
// "unknown congestion control algorithm: BBR", which fails the outbound build
// and leaves the node permanently circuit-open.
func TestParseClashTUICCongestionControlNormalized(t *testing.T) {
	data := []byte(`{
		"proxies": [
			{"name": "tuic-upper", "type": "tuic", "server": "203.0.113.30", "port": 443,
			 "uuid": "11111111-2222-3333-4444-555555555558", "password": "pw",
			 "congestion-controller": "BBR", "udp-relay-mode": "QUIC", "skip-cert-verify": true}
		]
	}`)

	nodes, err := ParseGeneralSubscription(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	obj := parseNodeRaw(t, nodes[0].RawOptions)
	if got := obj["congestion_control"]; got != "bbr" {
		t.Fatalf("congestion_control: got %v want bbr", got)
	}
	if got := obj["udp_relay_mode"]; got != "quic" {
		t.Fatalf("udp_relay_mode: got %v want quic", got)
	}
}
