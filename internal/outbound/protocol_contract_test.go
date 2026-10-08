package outbound_test

import (
	"strings"
	"testing"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/sagernet/sing/common"
)

// TestParsedSubscriptionBuildsOutbound is a contract test between the
// subscription parser and the sing-box outbound builder: everything the parser
// emits must be accepted by the builder.
//
// This is the guard for a whole class of "node shows up but stays circuit-open
// forever" bugs. When an outbound fails to build, Resin stores no outbound, the
// probe loop skips the node entirely, and the node never recovers. Two such
// bugs were found this way:
//
//   - vmess security "auto": sing-box rewrites it to "zero" when TLS is on,
//     which breaks every Xray/v2ray server. Fixed in normalizeVmessSecurityMode.
//   - shadowsocks "chacha20-poly1305" / "xchacha20-poly1305": Xray/mihomo
//     aliases that sing-box rejects with "unknown method". Fixed in
//     normalizeShadowsocksMethod.
//
// Only non-QUIC protocols are covered here so the test runs without the
// `with_quic` build tag. QUIC protocol normalization (tuic, hysteria2) is
// covered by the parser-level tests in internal/subscription.
func TestParsedSubscriptionBuildsOutbound(t *testing.T) {
	b, err := outbound.NewSingboxBuilderWithConfig(outbound.SingboxBuilderConfig{
		DNSUpstreams: config.DefaultNodeDNSUpstreams(),
	})
	if err != nil {
		t.Fatalf("NewSingboxBuilderWithConfig: %v", err)
	}
	defer b.Close()

	cases := []struct {
		name string
		sub  string
	}{
		{
			name: "vmess security=auto over tls",
			sub:  `{"proxies":[{"name":"n","type":"vmess","server":"1.1.1.1","port":443,"uuid":"11111111-2222-3333-4444-555555555556","cipher":"auto","tls":true,"servername":"e.com"}]}`,
		},
		{
			name: "vmess security=auto over ws+tls",
			sub:  `{"proxies":[{"name":"n","type":"vmess","server":"1.1.1.1","port":443,"uuid":"11111111-2222-3333-4444-555555555556","cipher":"auto","tls":true,"servername":"e.com","network":"ws","ws-opts":{"path":"/p","headers":{"Host":"e.com"}}}]}`,
		},
		{
			name: "vmess security=auto over tcp without tls",
			sub:  `{"proxies":[{"name":"n","type":"vmess","server":"1.1.1.1","port":80,"uuid":"11111111-2222-3333-4444-555555555556","cipher":"auto"}]}`,
		},
		{
			name: "ss chacha20-poly1305 (xray alias)",
			sub:  `{"proxies":[{"name":"n","type":"ss","server":"1.1.1.1","port":8388,"cipher":"chacha20-poly1305","password":"pw"}]}`,
		},
		{
			name: "ss xchacha20-poly1305 (xray alias)",
			sub:  `{"proxies":[{"name":"n","type":"ss","server":"1.1.1.1","port":8388,"cipher":"xchacha20-poly1305","password":"pw"}]}`,
		},
		{
			name: "ss canonical aead methods",
			sub:  `{"proxies":[{"name":"n","type":"ss","server":"1.1.1.1","port":8388,"cipher":"chacha20-ietf-poly1305","password":"pw"}]}`,
		},
		{
			name: "ss legacy stream method",
			sub:  `{"proxies":[{"name":"n","type":"ss","server":"1.1.1.1","port":8388,"cipher":"aes-256-cfb","password":"pw"}]}`,
		},
		{
			name: "ss with simple-obfs plugin alias",
			sub:  `{"proxies":[{"name":"n","type":"ss","server":"1.1.1.1","port":8388,"cipher":"aes-128-gcm","password":"pw","plugin":"simple-obfs","plugin-opts":"obfs=http;obfs-host=e.com"}]}`,
		},
		{
			name: "ss with v2ray-plugin",
			sub:  `{"proxies":[{"name":"n","type":"ss","server":"1.1.1.1","port":8388,"cipher":"aes-128-gcm","password":"pw","plugin":"v2ray-plugin","plugin-opts":"mode=websocket;host=e.com;path=/x;tls"}]}`,
		},
		{
			name: "ss 2022-blake3",
			sub:  `{"proxies":[{"name":"n","type":"ss","server":"1.1.1.1","port":8388,"cipher":"2022-blake3-aes-256-gcm","password":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}]}`,
		},
		{
			name: "vless vision flow",
			sub:  `{"proxies":[{"name":"n","type":"vless","server":"1.1.1.1","port":443,"uuid":"11111111-2222-3333-4444-555555555556","tls":true,"flow":"xtls-rprx-vision","servername":"e.com"}]}`,
		},
		{
			name: "vless vision-udp443 flow alias",
			sub:  `{"proxies":[{"name":"n","type":"vless","server":"1.1.1.1","port":443,"uuid":"11111111-2222-3333-4444-555555555556","tls":true,"flow":"xtls-rprx-vision-udp443","servername":"e.com"}]}`,
		},
		{
			name: "vless reality",
			sub:  `{"proxies":[{"name":"n","type":"vless","server":"1.1.1.1","port":443,"uuid":"11111111-2222-3333-4444-555555555556","tls":true,"servername":"e.com","reality-opts":{"public-key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","short-id":"0123"},"client-fingerprint":"chrome"}]}`,
		},
		{
			name: "vless ws transport",
			sub:  `{"proxies":[{"name":"n","type":"vless","server":"1.1.1.1","port":443,"uuid":"11111111-2222-3333-4444-555555555556","tls":true,"servername":"e.com","network":"ws","ws-opts":{"path":"/p","headers":{"Host":"e.com"}}}]}`,
		},
		{
			name: "vless grpc transport",
			sub:  `{"proxies":[{"name":"n","type":"vless","server":"1.1.1.1","port":443,"uuid":"11111111-2222-3333-4444-555555555556","tls":true,"servername":"e.com","network":"grpc","grpc-opts":{"grpc-service-name":"svc"}}]}`,
		},
		{
			name: "vless httpupgrade transport",
			sub:  `{"proxies":[{"name":"n","type":"vless","server":"1.1.1.1","port":443,"uuid":"11111111-2222-3333-4444-555555555556","tls":true,"servername":"e.com","network":"httpupgrade","ws-opts":{"path":"/p","headers":{"Host":"e.com"}}}]}`,
		},
		{
			name: "trojan basic",
			sub:  `{"proxies":[{"name":"n","type":"trojan","server":"1.1.1.1","port":443,"password":"pw","sni":"e.com"}]}`,
		},
		{
			name: "trojan ws transport",
			sub:  `{"proxies":[{"name":"n","type":"trojan","server":"1.1.1.1","port":443,"password":"pw","sni":"e.com","network":"ws","ws-opts":{"path":"/p","headers":{"Host":"e.com"}}}]}`,
		},
		{
			name: "anytls",
			sub:  `{"proxies":[{"name":"n","type":"anytls","server":"1.1.1.1","port":443,"password":"pw","sni":"e.com"}]}`,
		},
		{
			name: "vmess variant URI (auto cipher)",
			sub:  "vmess://YXV0bzoxZDIxYWIyZC03Mjc5LTVhN2QtOTk4YS1hN2QwMGUxZGM3ZDJANDMuMTc1LjEzMS4zMDo0NDM=?path=/pimg&obfsParam=hy.coe.re&obfs=websocket&tls=1&peer=hy.coe.re&alterId=0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := subscription.ParseGeneralSubscription([]byte(tc.sub))
			if err != nil {
				t.Fatalf("ParseGeneralSubscription: %v", err)
			}
			if len(nodes) != 1 {
				t.Fatalf("expected 1 parsed node, got %d", len(nodes))
			}
			ob, err := b.Build(nodes[0].RawOptions)
			if err != nil {
				// Some protocol features are only compiled in with build tags
				// (with_utls for REALITY, with_quic, with_wireguard, with_grpc).
				// Skip instead of failing so this test stays runnable in a plain
				// `go test ./...` build. Production images always carry the tags.
				if strings.Contains(err.Error(), "not included in this build") {
					t.Skipf("build tag missing for this protocol: %v", err)
				}
				t.Fatalf("sing-box rejected parsed outbound: %v\nraw: %s", err, nodes[0].RawOptions)
			}
			common.Close(ob)
		})
	}
}
