package subscription

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

func parseExtendedProxyURI(line string) (ParsedNode, bool) {
	u, err := url.Parse(line)
	if err != nil || !isProxyURIPathAllowed(u.Path) {
		return ParsedNode{}, false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ParsedNode{}, false
	}
	server := strings.TrimSpace(u.Hostname())
	if server == "" {
		return ParsedNode{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	port, validPort := parseRequiredURIPort(u)
	if u.Port() == "" {
		switch scheme {
		case "anytls":
			port, validPort = 443, true
		case "ssh":
			port, validPort = 22, true
		}
	}
	if !validPort || port == 0 {
		return ParsedNode{}, false
	}
	if scheme == "wireguard" || scheme == "wg" {
		return parseWireGuardURI(u, query, server, port)
	}

	outbound := map[string]any{
		"type":        scheme,
		"tag":         defaultTag(decodeTag(u.Fragment), scheme, server, port),
		"server":      server,
		"server_port": port,
	}
	userinfo := proxyURIUserInfo(u.User)
	switch scheme {
	case "anytls":
		if userinfo == "" {
			return ParsedNode{}, false
		}
		outbound["password"] = userinfo
		outbound["tls"] = tlsFromProxyURI(query, server)
		for _, key := range []string{"idle_session_check_interval", "idle_session_timeout"} {
			raw := firstNonEmpty(query.Get(key), query.Get(strings.ReplaceAll(key, "_", "-")))
			if duration, ok := normalizeDurationValue(raw, "s"); ok {
				outbound[key] = duration
			}
		}
		if count, err := strconv.ParseUint(firstNonEmpty(query.Get("min_idle_session"), query.Get("min-idle-session")), 10, 31); err == nil {
			outbound["min_idle_session"] = count
		}
	case "tuic":
		id, password, ok := strings.Cut(userinfo, ":")
		if !ok {
			return ParsedNode{}, false
		}
		parsedID, err := uuid.Parse(id)
		if err != nil {
			return ParsedNode{}, false
		}
		outbound["uuid"] = parsedID.String()
		outbound["password"] = password
		outbound["tls"] = tlsFromProxyURI(query, server)
		if congestion := firstNonEmpty(query.Get("congestion_control"), query.Get("congestion-controller")); congestion != "" {
			outbound["congestion_control"] = strings.ToLower(strings.TrimSpace(congestion))
		}
		if relay := firstNonEmpty(query.Get("udp_relay_mode"), query.Get("udp-relay-mode")); relay != "" {
			outbound["udp_relay_mode"] = strings.ToLower(strings.TrimSpace(relay))
		}
		if queryBool(query, "zero_rtt_handshake", "zero-rtt-handshake", "reduce-rtt") {
			outbound["zero_rtt_handshake"] = true
		}
		if queryBool(query, "udp_over_stream", "udp-over-stream") {
			outbound["udp_over_stream"] = true
		}
		if heartbeat, ok := normalizeDurationValue(firstNonEmpty(query.Get("heartbeat"), query.Get("heartbeat_interval")), "ms"); ok {
			outbound["heartbeat"] = heartbeat
		}
	case "hysteria":
		if !setHysteriaURIOptions(outbound, query, server, userinfo) {
			return ParsedNode{}, false
		}
	case "ssh":
		if u.User.Username() == "" {
			return ParsedNode{}, false
		}
		outbound["user"] = u.User.Username()
		if password, ok := u.User.Password(); ok {
			outbound["password"] = password
		}
		if hostKeys := splitCommaList(firstNonEmpty(query.Get("host_key"), query.Get("host-key"))); len(hostKeys) > 0 {
			outbound["host_key"] = hostKeys
		}
	default:
		return ParsedNode{}, false
	}
	return buildParsedNode(outbound)
}

func proxyURIUserInfo(user *url.Userinfo) string {
	if password, ok := user.Password(); ok {
		return user.Username() + ":" + password
	}
	return user.Username()
}

func tlsFromProxyURI(query url.Values, server string) map[string]any {
	name := firstNonEmpty(query.Get("sni"), query.Get("peer"), query.Get("servername"), query.Get("server_name"), server)
	tls := newClashEnabledTLS(name, queryBool(query, "insecure", "allowInsecure", "allow_insecure"), splitALPN(query.Get("alpn")))
	if queryBool(query, "disable_sni", "disable-sni") {
		tls["disable_sni"] = true
	}
	applyUTLSFromValue(tls, firstNonEmpty(query.Get("fp"), query.Get("fingerprint"), query.Get("client-fingerprint")))
	return tls
}

func setHysteriaURIOptions(outbound map[string]any, query url.Values, server, userinfo string) bool {
	if protocol := strings.ToLower(strings.TrimSpace(query.Get("protocol"))); protocol != "" && protocol != "udp" {
		return false
	}
	up, upErr := strconv.ParseUint(firstNonEmpty(query.Get("upmbps"), query.Get("up_mbps"), query.Get("up")), 10, 31)
	down, downErr := strconv.ParseUint(firstNonEmpty(query.Get("downmbps"), query.Get("down_mbps"), query.Get("down")), 10, 31)
	if upErr != nil || downErr != nil || up == 0 || down == 0 {
		return false
	}
	outbound["up_mbps"], outbound["down_mbps"] = up, down
	outbound["tls"] = tlsFromProxyURI(query, server)
	if auth := firstNonEmpty(query.Get("auth"), query.Get("auth_str"), userinfo); auth != "" {
		outbound["auth_str"] = auth
	}

	obfs := strings.ToLower(strings.TrimSpace(query.Get("obfs")))
	if obfs != "" && obfs != "xplus" {
		return false
	}
	if password := firstNonEmpty(query.Get("obfsParam"), query.Get("obfs-param"), query.Get("obfs_password")); password != "" {
		outbound["obfs"] = password
	} else if obfs == "xplus" {
		return false
	}
	if ports := normalizeHysteriaPortList(firstNonEmpty(query.Get("ports"), query.Get("mport"))); len(ports) > 0 {
		outbound["server_ports"] = ports
	}
	if interval, ok := normalizeDurationValue(firstNonEmpty(query.Get("hopInterval"), query.Get("hop-interval"), query.Get("hop_interval")), "s"); ok {
		outbound["hop_interval"] = interval
	}
	return true
}
