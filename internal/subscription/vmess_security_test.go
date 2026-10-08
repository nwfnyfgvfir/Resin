package subscription

import (
	"encoding/json"
	"testing"
)

// TestNormalizeVmessSecurityMode pins the mapping from the ambiguous "auto"
// VMess security mode to a concrete cipher.
//
// Regression: sing-box rewrites security "auto" to "zero" (no VMess-layer
// encryption) whenever TLS is enabled, so nodes exported by mainstream panels
// with `security=auto` failed the VMess handshake, were recorded as failed
// probes, and stayed circuit-open forever.
func TestNormalizeVmessSecurityMode(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{
			name: "auto with AEAD alter_id resolves to aes-128-gcm",
			in:   map[string]any{"type": "vmess", "security": "auto", "alter_id": uint64(0)},
			want: "aes-128-gcm",
		},
		{
			name: "missing security resolves to aes-128-gcm",
			in:   map[string]any{"type": "vmess", "alter_id": uint64(0)},
			want: "aes-128-gcm",
		},
		{
			name: "empty security resolves to aes-128-gcm",
			in:   map[string]any{"type": "vmess", "security": "", "alter_id": uint64(0)},
			want: "aes-128-gcm",
		},
		{
			name: "case-insensitive AUTO resolves to aes-128-gcm",
			in:   map[string]any{"type": "vmess", "security": "AUTO", "alter_id": uint64(0)},
			want: "aes-128-gcm",
		},
		{
			name: "legacy alter_id resolves to aes-128-cfb",
			in:   map[string]any{"type": "vmess", "security": "auto", "alter_id": uint64(64)},
			want: "aes-128-cfb",
		},
		{
			name: "explicit aes-128-gcm is preserved",
			in:   map[string]any{"type": "vmess", "security": "aes-128-gcm"},
			want: "aes-128-gcm",
		},
		{
			name: "explicit chacha20-poly1305 is preserved",
			in:   map[string]any{"type": "vmess", "security": "chacha20-poly1305"},
			want: "chacha20-poly1305",
		},
		{
			name: "explicit none is preserved",
			in:   map[string]any{"type": "vmess", "security": "none"},
			want: "none",
		},
		{
			name: "explicit zero is preserved",
			in:   map[string]any{"type": "vmess", "security": "zero"},
			want: "zero",
		},
		{
			name: "explicit aes-128-cfb is preserved",
			in:   map[string]any{"type": "vmess", "security": "aes-128-cfb"},
			want: "aes-128-cfb",
		},
		{
			name: "non-vmess outbound is untouched",
			in:   map[string]any{"type": "vless", "security": "auto"},
			want: "auto",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalizeVmessSecurityMode(tc.in)
			if got := getString(tc.in, "security"); got != tc.want {
				t.Fatalf("security: got %q want %q", got, tc.want)
			}
		})
	}
}

// TestParseVmessVariantURI_NormalizesAutoSecurity covers the real-world URI
// shape used by panel-generated subscriptions:
//
//	vmess://base64("auto:<uuid>@<host>:<port>")?obfs=websocket&obfsParam=...&tls=1&peer=...
//
// The cipher field carries "auto", which previously reached sing-box verbatim
// and produced a permanently circuit-broken node.
func TestParseVmessVariantURI_NormalizesAutoSecurity(t *testing.T) {
	const uri = "vmess://YXV0bzoxZDIxYWIyZC03Mjc5LTVhN2QtOTk4YS1hN2QwMGUxZGM3ZDJANDMuMTc1LjEzMS4zMDo0NDM=" +
		"?path=/pimg&remarks=hk-jp-1&obfsParam=hy.coe.re&obfs=websocket&tls=1&peer=hy.coe.re&udp=1&alterId=0"

	node, ok := parseVmessURI(uri)
	if !ok {
		t.Fatal("parseVmessURI returned !ok for a valid variant vmess URI")
	}

	var outbound map[string]any
	if err := json.Unmarshal(node.RawOptions, &outbound); err != nil {
		t.Fatalf("unmarshal outbound: %v", err)
	}

	if got := getString(outbound, "type"); got != "vmess" {
		t.Fatalf("type: got %q want vmess", got)
	}
	if got := getString(outbound, "uuid"); got != "1d21ab2d-7279-5a7d-998a-a7d00e1dc7d2" {
		t.Fatalf("uuid was mangled: got %q", got)
	}
	if got := getString(outbound, "server"); got != "43.175.131.30" {
		t.Fatalf("server: got %q", got)
	}
	if got := getString(outbound, "security"); got != "aes-128-gcm" {
		t.Fatalf("security: got %q want aes-128-gcm (must never reach sing-box as \"auto\")", got)
	}
	if alterID, _ := getUint(outbound, "alter_id"); alterID != 0 {
		t.Fatalf("alter_id: got %d want 0", alterID)
	}

	tls, ok := outbound["tls"].(map[string]any)
	if !ok {
		t.Fatal("missing tls block")
	}
	if enabled, _ := tls["enabled"].(bool); !enabled {
		t.Fatal("tls.enabled must be true")
	}
	if got := getString(tls, "server_name"); got != "hy.coe.re" {
		t.Fatalf("tls.server_name: got %q want hy.coe.re", got)
	}

	transport, ok := outbound["transport"].(map[string]any)
	if !ok {
		t.Fatal("missing transport block")
	}
	if got := getString(transport, "type"); got != "ws" {
		t.Fatalf("transport.type: got %q want ws", got)
	}
	if got := getString(transport, "path"); got != "/pimg" {
		t.Fatalf("transport.path: got %q want /pimg", got)
	}
	headers, _ := transport["headers"].(map[string]any)
	if got := getString(headers, "Host"); got != "hy.coe.re" {
		t.Fatalf("transport.headers.Host: got %q want hy.coe.re", got)
	}
}
