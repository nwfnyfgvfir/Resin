package subscription

import (
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

var errUnsupportedXrayOutbounds = errors.New("subscription: no Xray outbounds with supported protocols and transport settings")

type xrayCredentials struct {
	ID         string
	Encryption string
	Security   string
	Flow       string
	AlterID    uint16
	Method     string
	User       string
	Pass       string
	Password   string
	Reverse    map[string]any
}

type xrayServer struct {
	Address string
	Port    uint16
	xrayCredentials
	Users []xrayCredentials
}

func appendXrayOutbounds(nodes []ParsedNode, raw json.RawMessage) []ParsedNode {
	var config struct {
		Protocol       string
		Tag            string
		SendThrough    string
		Settings       json.RawMessage
		StreamSettings map[string]any
		ProxySettings  struct{ Tag string }
		Mux            struct{ Enabled bool }
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nodes
	}
	sockopt, _ := getMap(config.StreamSettings, "sockopt")
	if config.ProxySettings.Tag != "" || config.Mux.Enabled || getString(sockopt, "dialerProxy") != "" {
		return nodes
	}
	if config.SendThrough != "" && config.SendThrough != "0.0.0.0" && config.SendThrough != "::" {
		return nodes
	}
	protocol := strings.ToLower(strings.TrimSpace(config.Protocol))
	switch protocol {
	case "vmess", "vless", "trojan", "shadowsocks", "http", "socks":
	case "hysteria":
		if node, ok := parseXrayHysteria2(config.Tag, config.Settings, config.StreamSettings); ok {
			return append(nodes, node)
		}
		return nodes
	case "wireguard":
		if len(config.StreamSettings) == 0 {
			if node, ok := parseXrayWireGuard(config.Tag, config.Settings); ok {
				return append(nodes, node)
			}
		}
		return nodes
	default:
		return nodes
	}

	template := map[string]any{"type": protocol}
	if !setXrayStream(template, protocol, config.StreamSettings) {
		return nodes
	}
	var settings struct {
		xrayServer
		VNext   []xrayServer
		Servers []xrayServer
	}
	if err := json.Unmarshal(config.Settings, &settings); err != nil {
		return nodes
	}
	servers := settings.VNext
	if protocol != "vmess" && protocol != "vless" {
		servers = settings.Servers
	}
	if len(servers) == 0 {
		servers = []xrayServer{settings.xrayServer}
	}
	for _, server := range servers {
		if server.Address == "" || server.Port == 0 {
			continue
		}
		users := server.Users
		if len(users) == 0 {
			users = []xrayCredentials{server.xrayCredentials}
		}
		for _, user := range users {
			outbound := maps.Clone(template)
			outbound["server"], outbound["server_port"] = server.Address, server.Port
			outbound["tag"] = defaultTag(config.Tag, protocol, server.Address, uint64(server.Port))
			if setXrayCredentials(outbound, protocol, user) {
				if node, ok := buildParsedNode(outbound); ok {
					nodes = append(nodes, node)
				}
			}
		}
	}
	return nodes
}

func setXrayCredentials(outbound map[string]any, protocol string, user xrayCredentials) bool {
	switch protocol {
	case "vmess", "vless":
		if protocol == "vless" && ((user.Encryption != "" && user.Encryption != "none") || len(user.Reverse) > 0) {
			return false
		}
		id, err := uuid.Parse(user.ID)
		if err != nil {
			if len(user.ID) == 0 || len(user.ID) > 30 {
				return false
			}
			// Xray maps 1-30 byte user IDs to UUIDv5 in the nil namespace.
			id = uuid.NewSHA1(uuid.Nil, []byte(user.ID))
		}
		outbound["uuid"] = id.String()
		if protocol == "vmess" {
			outbound["security"] = firstNonEmpty(user.Security, "auto")
			outbound["alter_id"] = user.AlterID
		} else if flow := normalizeVLESSFlow(user.Flow); flow != "" {
			outbound["flow"] = flow
		}
	case "trojan", "shadowsocks":
		if user.Password == "" {
			return false
		}
		outbound["password"] = user.Password
		if protocol == "shadowsocks" {
			method := normalizeShadowsocksMethod(user.Method)
			if method == "" {
				return false
			}
			outbound["method"] = method
		}
	case "http", "socks":
		if user.User != "" || user.Pass != "" {
			outbound["username"], outbound["password"] = user.User, user.Pass
		}
	}
	return true
}

func setXrayStream(outbound map[string]any, protocol string, stream map[string]any) bool {
	if settings, _ := getMap(stream, "finalmask"); len(settings) > 0 {
		return false
	}
	if hasXrayStreamTarget(stream) {
		return false
	}
	network := xrayStreamNetwork(stream)
	security := strings.ToLower(strings.TrimSpace(getString(stream, "security")))
	bare := network == "" || network == "tcp"
	if protocol == "socks" || protocol == "shadowsocks" {
		if !bare || (security != "" && security != "none") {
			return false
		}
	}
	if protocol == "http" && !bare {
		return false
	}
	switch security {
	case "", "none":
	case "tls":
		options, ok := xrayTLSOptions(stream)
		if !ok {
			return false
		}
		outbound["tls"] = options
	case "reality":
		reality, _ := getMap(stream, "realitySettings")
		key := firstNonEmpty(getString(reality, "publicKey"), getString(reality, "password"))
		if key == "" || getString(reality, "mldsa65Verify") != "" {
			return false
		}
		options := newClashEnabledTLS(getString(reality, "serverName"), false, nil)
		applyUTLSFromValue(options, firstNonEmpty(getString(reality, "fingerprint"), "chrome"))
		options["reality"] = map[string]any{"enabled": true, "public_key": key, "short_id": getString(reality, "shortId")}
		outbound["tls"] = options
	default:
		return false
	}

	transport := map[string]any{"network": network}
	switch network {
	case "", "tcp":
		options, _ := getMap(stream, "rawSettings", "tcpSettings")
		header, _ := getMap(options, "header")
		return firstNonEmpty(getString(header, "type"), "none") == "none"
	case "ws":
		options, _ := getMap(stream, "wsSettings")
		headers, _ := getMap(options, "headers")
		if host := getString(options, "host"); host != "" {
			headers = maps.Clone(headers)
			if headers == nil {
				headers = make(map[string]any)
			}
			headers["Host"] = host
		}
		transport["ws-opts"] = map[string]any{"path": getString(options, "path"), "headers": headers}
	case "grpc":
		options, _ := getMap(stream, "grpcSettings")
		if multiMode, _ := getBool(options, "multiMode"); multiMode {
			return false
		}
		if getString(options, "authority") != "" {
			return false
		}
		transport["grpc-opts"] = map[string]any{"service-name": getString(options, "serviceName")}
	case "http", "h2":
		options, _ := getMap(stream, "httpSettings")
		transport["http-opts"], transport["h2-opts"] = options, options
	case "httpupgrade":
		options, _ := getMap(stream, "httpupgradeSettings")
		transport["http-upgrade-opts"] = options
	case "quic":
		options, _ := getMap(stream, "quicSettings")
		header, _ := getMap(options, "header")
		if getString(options, "key") != "" || firstNonEmpty(getString(options, "security"), "none") != "none" ||
			firstNonEmpty(getString(header, "type"), "none") != "none" {
			return false
		}
	default:
		return false
	}
	return setV2RayTransportFromClash(outbound, transport)
}

func xrayStreamNetwork(stream map[string]any) string {
	return normalizeV2RayNetwork(firstNonEmpty(getString(stream, "method"), getString(stream, "network")))
}

func hasXrayStreamTarget(stream map[string]any) bool {
	port, _ := getUint(stream, "port")
	return port != 0 || getString(stream, "address") != ""
}

func xrayTLSOptions(stream map[string]any) (map[string]any, bool) {
	tls, _ := getMap(stream, "tlsSettings")
	if roots, _ := getBool(tls, "disableSystemRoot"); roots {
		return nil, false
	}
	if certs, ok := tls["certificates"].([]any); ok && len(certs) > 0 {
		return nil, false
	}
	serverName := getString(tls, "serverName")
	if len(getStringList(tls, "pinnedPeerCertSha256", "pinnedPeerCertificateChainSha256", "verifyPeerCertByName", "verifyPeerCertInNames")) > 0 ||
		getString(tls, "echConfigList") != "" || strings.EqualFold(serverName, "FromMitM") {
		return nil, false
	}
	insecure, _ := getBool(tls, "allowInsecure")
	alpn := getStringList(tls, "alpn")
	if len(alpn) == 1 && alpn[0] == "FromMitM" {
		return nil, false
	}
	options := newClashEnabledTLS(serverName, insecure, alpn)
	if minVersion := getString(tls, "minVersion"); minVersion != "" {
		options["min_version"] = minVersion
	}
	if maxVersion := getString(tls, "maxVersion"); maxVersion != "" {
		options["max_version"] = maxVersion
	}
	if cipherSuites := getString(tls, "cipherSuites"); cipherSuites != "" {
		options["cipher_suites"] = strings.Split(cipherSuites, ":")
	}
	if fingerprint := getString(tls, "fingerprint"); !strings.EqualFold(fingerprint, "unsafe") {
		applyUTLSFromValue(options, fingerprint)
	}
	return options, true
}

func parseXrayHysteria2(tag string, raw json.RawMessage, stream map[string]any) (ParsedNode, bool) {
	if xrayStreamNetwork(stream) != "hysteria" || !strings.EqualFold(getString(stream, "security"), "tls") || hasXrayStreamTarget(stream) {
		return ParsedNode{}, false
	}
	var settings struct {
		Version uint16
		Address string
		Port    uint16
	}
	if err := json.Unmarshal(raw, &settings); err != nil || settings.Version != 2 || settings.Address == "" || settings.Port == 0 {
		return ParsedNode{}, false
	}
	transport, _ := getMap(stream, "hysteriaSettings")
	version, _ := getUint(transport, "version")
	password := getString(transport, "auth")
	if version != 2 || password == "" {
		return ParsedNode{}, false
	}
	tls, ok := xrayTLSOptions(stream)
	if !ok {
		return ParsedNode{}, false
	}
	outbound := map[string]any{
		"type": "hysteria2", "tag": defaultTag(tag, "hysteria2", settings.Address, uint64(settings.Port)),
		"server": settings.Address, "server_port": settings.Port,
		"password": password, "tls": tls,
	}
	mask, _ := getMap(stream, "finalmask")
	tcpMasks, _ := mask["tcp"].([]any)
	udpMasks, _ := mask["udp"].([]any)
	quic, _ := getMap(mask, "quicParams")
	if len(tcpMasks) > 0 || len(udpMasks) > 1 || len(quic) > 0 {
		return ParsedNode{}, false
	}
	if len(udpMasks) == 1 {
		udp, _ := udpMasks[0].(map[string]any)
		obfs, _ := getMap(udp, "settings")
		obfsPassword := getString(obfs, "password")
		if getString(udp, "type") != "salamander" || obfsPassword == "" || obfs["packetSize"] != nil && obfs["packetSize"] != "" {
			return ParsedNode{}, false
		}
		outbound["obfs"] = map[string]any{"type": "salamander", "password": obfsPassword}
	}
	return buildParsedNode(outbound)
}

func parseXrayWireGuard(tag string, raw json.RawMessage) (ParsedNode, bool) {
	var settings struct {
		SecretKey string
		Address   []string
		MTU       uint16
		Reserved  []uint16
		Peers     []struct {
			Endpoint     string
			PublicKey    string
			PreSharedKey string
			AllowedIPs   []string
			KeepAlive    uint16
		}
	}
	if err := json.Unmarshal(raw, &settings); err != nil || len(settings.Peers) == 0 {
		return ParsedNode{}, false
	}
	if r := settings.Reserved; len(r) != 0 && (len(r) != 3 || r[0] > 255 || r[1] > 255 || r[2] > 255) {
		return ParsedNode{}, false
	}
	var peers []map[string]any
	for _, peer := range settings.Peers {
		host, port, ok := parseHostPort(peer.Endpoint)
		if !ok || port == 0 {
			return ParsedNode{}, false
		}
		allowedIPs := peer.AllowedIPs
		if len(allowedIPs) == 0 {
			allowedIPs = []string{"0.0.0.0/0", "::/0"}
		}
		config, ok := wireGuardPeer(host, port, peer.PublicKey, peer.PreSharedKey, allowedIPs)
		if !ok {
			return ParsedNode{}, false
		}
		if len(settings.Reserved) != 0 {
			config["reserved"] = settings.Reserved
		}
		if peer.KeepAlive != 0 {
			config["persistent_keepalive_interval"] = peer.KeepAlive
		}
		tag = defaultTag(tag, "wireguard", host, port)
		peers = append(peers, config)
	}
	if len(settings.Address) == 0 {
		settings.Address = []string{"10.0.0.1", "fd59:7153:2388:b5fd::1"}
	}
	if settings.MTU == 0 {
		settings.MTU = 1420
	}
	return wireGuardEndpoint(tag, settings.SecretKey, settings.Address, strconv.Itoa(int(settings.MTU)), peers)
}
