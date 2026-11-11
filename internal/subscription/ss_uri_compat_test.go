package subscription

import (
	"encoding/base64"
	"net/url"
	"testing"
)

func TestParseSSURI_SIP002CredentialsAndOptionalSlash(t *testing.T) {
	for _, tc := range []struct{ name, uri, password string }{
		{"base64 with plugin", "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:secret")) + "@example.com:8388/?plugin=obfs-local%3Bobfs%3Dhttp#node", "secret"},
		{"escaped plaintext", "ss://aes-128-gcm:" + url.PathEscape("p@ss:+/%20") + "@example.com:8388/#node", "p@ss:+/%20"},
		{"uppercase scheme", "SS://aes-128-gcm:secret@example.com:8388#node", "secret"},
		{"literal plus", "ss://2022-blake3-aes-256-gcm:abc%2Bdef%2Fghi%3D@example.com:8388/#node", "abc+def/ghi="},
		{"base64 preserves percent", "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:literal%20")) + "@example.com:8388/#node", "literal%20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := ParseGeneralSubscription([]byte(tc.uri))
			if err != nil || len(nodes) != 1 {
				t.Fatalf("parse: count=%d err=%v", len(nodes), err)
			}
			options := parseNodeRaw(t, nodes[0].RawOptions)
			if options["password"] != tc.password || options["server_port"] != float64(8388) {
				t.Fatalf("incorrect credentials or port: %+v", options)
			}
			if tc.name == "base64 with plugin" && options["plugin"] != "obfs-local" {
				t.Fatal("lost plugin configuration")
			}
		})
	}
}

func TestParseGeneralSubscription_Base64ProviderShadowsocksFormat(t *testing.T) {
	content := "# Provider metadata\r\n\r\nss://" + base64.StdEncoding.EncodeToString([]byte("aes-128-gcm:example-password")) + "@edge.example:443?tfo=1&udp=1&type=ss#Hong%20Kong\r\n"
	nodes, err := ParseGeneralSubscription([]byte(base64.StdEncoding.EncodeToString([]byte(content))))
	if err != nil || len(nodes) != 1 {
		t.Fatalf("parse: count=%d err=%v", len(nodes), err)
	}
	if nodes[0].Tag != "Hong Kong" {
		t.Fatalf("incorrect tag: %q", nodes[0].Tag)
	}
}
