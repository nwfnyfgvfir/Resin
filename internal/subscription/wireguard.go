package subscription

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

func parseWireGuardURI(u *url.URL, query url.Values, server string, port uint64) (ParsedNode, bool) {
	privateKey := proxyURIUserInfo(u.User)
	localAddresses := splitCommaList(query.Get("address"))
	// Unescaped '+' in URI query keys is decoded as a space.
	publicKey := strings.ReplaceAll(firstNonEmpty(query.Get("publickey"), query.Get("public_key")), " ", "+")
	preSharedKey := strings.ReplaceAll(firstNonEmpty(query.Get("presharedkey"), query.Get("pre_shared_key")), " ", "+")
	allowedIPs := splitCommaList(firstNonEmpty(query.Get("allowed_ips"), query.Get("allowedips"), "0.0.0.0/0,::/0"))
	peer, ok := wireGuardPeer(server, port, publicKey, preSharedKey, allowedIPs)
	if !ok {
		return ParsedNode{}, false
	}
	if reserved := strings.TrimSpace(query.Get("reserved")); reserved != "" {
		values := splitCommaList(strings.Trim(reserved, "[]"))
		if len(values) != 3 {
			return ParsedNode{}, false
		}
		bytes := make([]int, len(values))
		for i, value := range values {
			b, err := strconv.ParseUint(value, 10, 8)
			if err != nil {
				return ParsedNode{}, false
			}
			bytes[i] = int(b)
		}
		peer["reserved"] = bytes
	}
	if keepalive := firstNonEmpty(query.Get("persistent_keepalive_interval"), query.Get("persistentKeepalive")); keepalive != "" {
		seconds, err := strconv.ParseUint(keepalive, 10, 16)
		if err != nil {
			return ParsedNode{}, false
		}
		peer["persistent_keepalive_interval"] = seconds
	}
	// Xray-only finalmask settings affect the connection on the wire.
	if query.Get("fm") != "" {
		return ParsedNode{}, false
	}
	return wireGuardEndpoint(defaultTag(decodeTag(u.Fragment), "wireguard", server, port), privateKey, localAddresses, query.Get("mtu"), []map[string]any{peer})
}

func parseWireGuardConf(text string) ([]ParsedNode, bool, error) {
	if !strings.ContainsRune(text, '[') {
		return nil, false, nil
	}
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "[interface]") || !strings.Contains(lower, "[peer]") {
		return nil, false, nil
	}

	client := make(map[string]string)
	var hasInterface bool
	var peers []map[string]string
	var section map[string]string
	for rawLine := range strings.SplitSeq(text, "\n") {
		line, _, _ := strings.Cut(rawLine, "#")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			switch strings.ToLower(strings.TrimSpace(line[1 : len(line)-1])) {
			case "interface":
				hasInterface = true
				section = client
			case "peer":
				section = make(map[string]string)
				peers = append(peers, section)
			default:
				section = nil
			}
			continue
		}
		if key, value, ok := strings.Cut(line, "="); section != nil && ok {
			key, value = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(value)
			if old := section[key]; old != "" && (key == "address" || key == "allowedips") {
				value = old + "," + value
			}
			section[key] = value
		}
	}

	if !hasInterface || len(peers) == 0 {
		return nil, false, nil
	}
	options := make([]map[string]any, 0, len(peers))
	var tag string
	for _, entry := range peers {
		server, port, ok := parseHostPort(entry["endpoint"])
		if !ok || port == 0 {
			return nil, true, fmt.Errorf("subscription: WireGuard peer requires an endpoint host and port")
		}
		peer, ok := wireGuardPeer(server, port, entry["publickey"], entry["presharedkey"], splitCommaList(entry["allowedips"]))
		if !ok {
			return nil, true, fmt.Errorf("subscription: invalid WireGuard peer key or allowed IPs")
		}
		if keepalive := entry["persistentkeepalive"]; keepalive != "" && !strings.EqualFold(keepalive, "off") {
			seconds, err := strconv.ParseUint(keepalive, 10, 16)
			if err != nil {
				return nil, true, fmt.Errorf("subscription: invalid WireGuard keepalive")
			}
			peer["persistent_keepalive_interval"] = seconds
		}
		options = append(options, peer)
		if tag == "" {
			tag = defaultTag("", "wireguard", server, port)
		}
	}
	node, ok := wireGuardEndpoint(tag, client["privatekey"], splitCommaList(client["address"]), client["mtu"], options)
	if !ok {
		return nil, true, fmt.Errorf("subscription: invalid WireGuard private key, interface addresses, peers or MTU")
	}
	return []ParsedNode{node}, true, nil
}

func wireGuardEndpoint(tag, privateKey string, addresses []string, mtu string, peers []map[string]any) (ParsedNode, bool) {
	privateKey = wireGuardKey(privateKey)
	addresses = wireGuardPrefixes(addresses)
	if privateKey == "" || len(addresses) == 0 || len(peers) == 0 {
		return ParsedNode{}, false
	}
	outbound := map[string]any{
		"type":        "wireguard",
		"tag":         tag,
		"address":     addresses,
		"private_key": privateKey,
		"peers":       peers,
	}
	if mtu != "" {
		value, err := strconv.ParseUint(mtu, 10, 16)
		if err != nil || value == 0 {
			return ParsedNode{}, false
		}
		outbound["mtu"] = value
	}
	return buildParsedNode(outbound)
}

func wireGuardPeer(server string, port uint64, publicKey, preSharedKey string, allowedIPs []string) (map[string]any, bool) {
	publicKey = wireGuardKey(publicKey)
	allowedIPs = wireGuardPrefixes(allowedIPs)
	if publicKey == "" || len(allowedIPs) == 0 {
		return nil, false
	}
	peer := map[string]any{"address": server, "port": port, "public_key": publicKey, "allowed_ips": allowedIPs}
	if preSharedKey != "" {
		key := wireGuardKey(preSharedKey)
		if key == "" {
			return nil, false
		}
		peer["pre_shared_key"] = key
	}
	return peer, true
}

func wireGuardKey(raw string) string {
	bytes, ok := decodeBase64Relaxed(strings.TrimSpace(raw))
	if !ok || len(bytes) != 32 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

func wireGuardPrefixes(values []string) []string {
	prefixes := make([]string, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			addr, err := netip.ParseAddr(value)
			if err != nil || addr.Zone() != "" {
				return nil
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefixes = append(prefixes, prefix.String())
	}
	return prefixes
}
