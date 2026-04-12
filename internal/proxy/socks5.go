package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	"github.com/sagernet/sing/common/buf"
	singbufio "github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/varbin"
	"github.com/sagernet/sing/protocol/socks/socks5"
)

// Socks5ProxyConfig holds dependencies for the SOCKS5 inbound proxy.
type Socks5ProxyConfig struct {
	ProxyToken        string
	AuthVersion       string
	DeploymentProfile config.DeploymentProfile
	AdvertiseHost     string
	ListenAddress     string
	ListenPort        int
	Router            *routing.Router
	Pool              outbound.PoolAccessor
	Health            HealthRecorder
	Events            EventEmitter
	MetricsSink       MetricsEventSink
}

// Socks5Proxy implements a SOCKS5 inbound supporting CONNECT everywhere and
// UDP ASSOCIATE only when the deployment profile allows it.
type Socks5Proxy struct {
	token             string
	authVersion       config.AuthVersion
	deploymentProfile config.DeploymentProfile
	advertiseHost     string
	listenAddress     string
	listenPort        int
	router            *routing.Router
	pool              outbound.PoolAccessor
	health            HealthRecorder
	events            EventEmitter
	metricsSink       MetricsEventSink
}

func NewSocks5Proxy(cfg Socks5ProxyConfig) *Socks5Proxy {
	ev := cfg.Events
	if ev == nil {
		ev = NoOpEventEmitter{}
	}
	authVersion := config.NormalizeAuthVersion(cfg.AuthVersion)
	if authVersion == "" {
		authVersion = config.AuthVersionLegacyV0
	}
	profile := cfg.DeploymentProfile
	if profile == "" {
		profile = config.DeploymentProfileStandard
	}
	return &Socks5Proxy{
		token:             cfg.ProxyToken,
		authVersion:       authVersion,
		deploymentProfile: profile,
		advertiseHost:     strings.TrimSpace(cfg.AdvertiseHost),
		listenAddress:     strings.TrimSpace(cfg.ListenAddress),
		listenPort:        cfg.ListenPort,
		router:            cfg.Router,
		pool:              cfg.Pool,
		health:            cfg.Health,
		events:            ev,
		metricsSink:       cfg.MetricsSink,
	}
}

func (p *Socks5Proxy) Serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go p.serveConn(conn)
	}
}

func (p *Socks5Proxy) serveConn(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	authResult, err := p.handshake(conn, reader)
	if err != nil {
		return
	}

	req, err := socks5.ReadRequest(varbin.StubReader(reader))
	if err != nil {
		return
	}
	switch req.Command {
	case socks5.CommandConnect:
		p.handleConnectConn(conn, reader, req, authResult)
	case socks5.CommandUDPAssociate:
		p.handleUDPAssociateConn(conn, req, authResult)
	default:
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeUnsupported})
	}
}

type socks5AuthResult struct {
	platformName string
	account      string
}

func (p *Socks5Proxy) handshake(conn net.Conn, reader *bufio.Reader) (socks5AuthResult, error) {
	authReq, err := socks5.ReadAuthRequest(varbin.StubReader(reader))
	if err != nil {
		return socks5AuthResult{}, err
	}

	method := socks5.AuthTypeNoAcceptedMethods
	if p.token == "" {
		for _, candidate := range authReq.Methods {
			if candidate == socks5.AuthTypeUsernamePassword {
				method = socks5.AuthTypeUsernamePassword
				break
			}
		}
		if method == socks5.AuthTypeNoAcceptedMethods {
			for _, candidate := range authReq.Methods {
				if candidate == socks5.AuthTypeNotRequired {
					method = socks5.AuthTypeNotRequired
					break
				}
			}
		}
	} else {
		for _, candidate := range authReq.Methods {
			if candidate == socks5.AuthTypeUsernamePassword {
				method = socks5.AuthTypeUsernamePassword
				break
			}
		}
	}
	if err := socks5.WriteAuthResponse(conn, socks5.AuthResponse{Method: method}); err != nil {
		return socks5AuthResult{}, err
	}
	if method == socks5.AuthTypeNoAcceptedMethods {
		return socks5AuthResult{}, errors.New("socks5: no accepted auth methods")
	}
	if method != socks5.AuthTypeUsernamePassword {
		return socks5AuthResult{}, nil
	}

	creds, err := socks5.ReadUsernamePasswordAuthRequest(varbin.StubReader(reader))
	if err != nil {
		return socks5AuthResult{}, err
	}
	authResult, ok := p.authenticateCredentials(creds.Username, creds.Password)
	if !ok {
		_ = socks5.WriteUsernamePasswordAuthResponse(conn, socks5.UsernamePasswordAuthResponse{Status: socks5.UsernamePasswordStatusFailure})
		return socks5AuthResult{}, errors.New("socks5: authentication failed")
	}
	if err := socks5.WriteUsernamePasswordAuthResponse(conn, socks5.UsernamePasswordAuthResponse{Status: socks5.UsernamePasswordStatusSuccess}); err != nil {
		return socks5AuthResult{}, err
	}
	return authResult, nil
}

func (p *Socks5Proxy) authenticateCredentials(username, password string) (socks5AuthResult, bool) {
	identityPlatform, identityAccount := p.credentialIdentity(username)
	if p.token == "" {
		return socks5AuthResult{platformName: identityPlatform, account: identityAccount}, true
	}
	if password != p.token {
		return socks5AuthResult{}, false
	}
	return socks5AuthResult{platformName: identityPlatform, account: identityAccount}, true
}

func (p *Socks5Proxy) credentialIdentity(username string) (string, string) {
	if p.authVersion == config.AuthVersionV1 {
		if p.token == "" {
			return parseForwardCredentialV1WhenAuthDisabled(username)
		}
		return parseV1PlatformAccountIdentity(username)
	}
	if p.token == "" {
		return parseLegacyAuthDisabledIdentityCredential(username)
	}
	return parseLegacyPlatformAccountIdentity(username)
}

func (p *Socks5Proxy) handleConnectConn(conn net.Conn, reader *bufio.Reader, req socks5.Request, authResult socks5AuthResult) {
	platformName, account := authResult.platformName, authResult.account
	lifecycle := newRequestLifecycleFromFields(p.events, ProxyTypeSocks5, true, "SOCKS_CONNECT", remoteIPString(conn.RemoteAddr()))
	lifecycle.setTarget(req.Destination.String(), "")
	lifecycle.setAccount(account)
	defer lifecycle.finish()

	routed, routeErr := resolveRoutedOutbound(p.router, p.pool, platformName, account, req.Destination.String())
	if routeErr != nil {
		lifecycle.setProxyError(routeErr)
		lifecycle.setHTTPStatus(routeErr.HTTPCode)
		lifecycle.setUpstreamError("socks5_route", errors.New(routeErr.ResinError))
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socksReplyCodeForProxyError(routeErr), Bind: p.bindAddress()})
		return
	}
	lifecycle.setRouteResult(routed.Route)
	domain := netutil.ExtractDomain(req.Destination.String())
	nodeHash := routed.Route.NodeHash
	go p.health.RecordLatency(nodeHash, domain, nil)

	rawConn, err := routed.Outbound.DialContext(context.Background(), "tcp", req.Destination)
	if err != nil {
		proxyErr := classifyConnectError(err)
		if proxyErr != nil {
			lifecycle.setProxyError(proxyErr)
			lifecycle.setHTTPStatus(proxyErr.HTTPCode)
			lifecycle.setUpstreamError("socks5_connect_dial", err)
			go p.health.RecordResult(nodeHash, false)
			_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socksReplyCodeForError(err, proxyErr), Bind: p.bindAddress()})
		}
		return
	}
	recordResult := func(ok bool) {
		lifecycle.setNetOK(ok)
		go p.health.RecordResult(nodeHash, ok)
	}

	var upstreamBase net.Conn = rawConn
	if p.metricsSink != nil {
		p.metricsSink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
		upstreamBase = newCountingConn(rawConn, p.metricsSink)
	}
	upstreamConn := newTLSLatencyConn(upstreamBase, func(latency time.Duration) {
		p.health.RecordLatency(nodeHash, domain, &latency)
	})

	if err := socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeSuccess, Bind: p.bindAddress()}); err != nil {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setHTTPStatus(ErrUpstreamRequestFailed.HTTPCode)
		lifecycle.setUpstreamError("socks5_connect_response_write", err)
		recordResult(false)
		return
	}

	clientToUpstream, err := makeTunnelClientReader(conn, reader)
	if err != nil {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setHTTPStatus(ErrUpstreamRequestFailed.HTTPCode)
		lifecycle.setUpstreamError("socks5_connect_prefetch_drain", err)
		recordResult(false)
		return
	}

	type copyResult struct {
		n   int64
		err error
	}
	egressBytesCh := make(chan copyResult, 1)
	go func() {
		defer upstreamConn.Close()
		defer conn.Close()
		n, copyErr := io.Copy(upstreamConn, clientToUpstream)
		egressBytesCh <- copyResult{n: n, err: copyErr}
	}()
	ingressBytes, ingressCopyErr := io.Copy(conn, upstreamConn)
	lifecycle.addIngressBytes(ingressBytes)
	_ = conn.Close()
	_ = upstreamConn.Close()
	egressResult := <-egressBytesCh
	lifecycle.addEgressBytes(egressResult.n)

	okResult := ingressBytes > 0 && egressResult.n > 0
	if !okResult {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setHTTPStatus(ErrUpstreamRequestFailed.HTTPCode)
		switch {
		case !isBenignTunnelCopyError(ingressCopyErr):
			lifecycle.setUpstreamError("socks5_connect_upstream_to_client_copy", ingressCopyErr)
		case !isBenignTunnelCopyError(egressResult.err):
			lifecycle.setUpstreamError("socks5_connect_client_to_upstream_copy", egressResult.err)
		default:
			switch {
			case ingressBytes == 0 && egressResult.n == 0:
				lifecycle.setUpstreamError("socks5_connect_zero_traffic", nil)
			case ingressBytes == 0:
				lifecycle.setUpstreamError("socks5_connect_no_ingress_traffic", nil)
			default:
				lifecycle.setUpstreamError("socks5_connect_no_egress_traffic", nil)
			}
		}
	}
	recordResult(okResult)
}

func (p *Socks5Proxy) handleUDPAssociateConn(conn net.Conn, req socks5.Request, authResult socks5AuthResult) {
	platformName, account := authResult.platformName, authResult.account
	lifecycle := newRequestLifecycleFromFields(p.events, ProxyTypeSocks5, true, "SOCKS_UDP_ASSOCIATE", remoteIPString(conn.RemoteAddr()))
	lifecycle.setTarget(req.Destination.String(), "")
	lifecycle.setAccount(account)
	defer lifecycle.finish()

	if !p.deploymentProfile.AllowsSocks5UDP() {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setHTTPStatus(ErrUpstreamRequestFailed.HTTPCode)
		lifecycle.setUpstreamError("socks5_udp_disabled", nil)
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeNotAllowed, Bind: p.bindAddress()})
		return
	}

	routed, routeErr := resolveRoutedOutbound(p.router, p.pool, platformName, account, req.Destination.String())
	if routeErr != nil {
		lifecycle.setProxyError(routeErr)
		lifecycle.setHTTPStatus(routeErr.HTTPCode)
		lifecycle.setUpstreamError("socks5_udp_route", errors.New(routeErr.ResinError))
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socksReplyCodeForProxyError(routeErr), Bind: p.bindAddress()})
		return
	}
	lifecycle.setRouteResult(routed.Route)
	nodeHash := routed.Route.NodeHash
	go p.health.RecordLatency(nodeHash, netutil.ExtractDomain(req.Destination.String()), nil)

	var listenDestination M.Socksaddr
	if req.Destination.IsValid() {
		listenDestination = req.Destination
		if listenDestination.Fqdn != "" {
			listenDestination = M.Socksaddr{}
		}
	}
	packetConn, err := routed.Outbound.ListenPacket(context.Background(), listenDestination)
	if err != nil {
		proxyErr := classifyUpstreamError(err)
		if proxyErr == nil {
			proxyErr = ErrUpstreamRequestFailed
		}
		lifecycle.setProxyError(proxyErr)
		lifecycle.setHTTPStatus(proxyErr.HTTPCode)
		lifecycle.setUpstreamError("socks5_udp_listen_packet", err)
		go p.health.RecordResult(nodeHash, false)
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socksReplyCodeForError(err, proxyErr), Bind: p.bindAddress()})
		return
	}
	defer packetConn.Close()

	udpListener, err := net.ListenPacket("udp", net.JoinHostPort(p.bindAdvertiseHost(), "0"))
	if err != nil {
		lifecycle.setProxyError(ErrInternalError)
		lifecycle.setHTTPStatus(ErrInternalError.HTTPCode)
		lifecycle.setUpstreamError("socks5_udp_listen", err)
		go p.health.RecordResult(nodeHash, false)
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeFailure, Bind: p.bindAddress()})
		return
	}
	defer udpListener.Close()

	associateAddr, ok := udpListener.LocalAddr().(*net.UDPAddr)
	if !ok {
		lifecycle.setProxyError(ErrInternalError)
		lifecycle.setHTTPStatus(ErrInternalError.HTTPCode)
		lifecycle.setUpstreamError("socks5_udp_local_addr", nil)
		go p.health.RecordResult(nodeHash, false)
		_ = socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeFailure, Bind: p.bindAddress()})
		return
	}
	bindAddr := M.ParseSocksaddr(net.JoinHostPort(p.bindAdvertiseHost(), fmt.Sprintf("%d", associateAddr.Port)))
	if err := socks5.WriteResponse(conn, socks5.Response{ReplyCode: socks5.ReplyCodeSuccess, Bind: bindAddr}); err != nil {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setHTTPStatus(ErrUpstreamRequestFailed.HTTPCode)
		lifecycle.setUpstreamError("socks5_udp_response_write", err)
		go p.health.RecordResult(nodeHash, false)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tcpDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, conn)
		tcpDone <- err
	}()

	clientPacketConn := singbufio.NewPacketConn(udpListener)
	upstreamPacketConn := singbufio.NewPacketConn(packetConn)
	allowedClientIP := remoteIPString(conn.RemoteAddr())
	type udpCopyResult struct {
		n   int64
		err error
	}
	clientToUpstreamCh := make(chan udpCopyResult, 1)
	upstreamToClientCh := make(chan udpCopyResult, 1)

	go func() {
		clientToUpstreamCh <- p.copySocks5UDPClientToUpstream(ctx, clientPacketConn, upstreamPacketConn, allowedClientIP)
	}()
	go func() {
		upstreamToClientCh <- p.copySocks5UDPUpstreamToClient(ctx, upstreamPacketConn, clientPacketConn, associateAddr)
	}()

	var clientToUpstream udpCopyResult
	var upstreamToClient udpCopyResult
	clientToUpstreamDone := false
	upstreamToClientDone := false
	for !(clientToUpstreamDone && upstreamToClientDone) {
		select {
		case clientToUpstream = <-clientToUpstreamCh:
			clientToUpstreamDone = true
			cancel()
		case upstreamToClient = <-upstreamToClientCh:
			upstreamToClientDone = true
			cancel()
		case <-tcpDone:
			cancel()
		}
	}

	lifecycle.addEgressBytes(clientToUpstream.n)
	lifecycle.addIngressBytes(upstreamToClient.n)
	okResult := clientToUpstream.n > 0 && upstreamToClient.n > 0
	if !okResult {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setHTTPStatus(ErrUpstreamRequestFailed.HTTPCode)
		switch {
		case !isBenignTunnelCopyError(clientToUpstream.err):
			lifecycle.setUpstreamError("socks5_udp_client_to_upstream_copy", clientToUpstream.err)
		case !isBenignTunnelCopyError(upstreamToClient.err):
			lifecycle.setUpstreamError("socks5_udp_upstream_to_client_copy", upstreamToClient.err)
		default:
			lifecycle.setUpstreamError("socks5_udp_zero_traffic", nil)
		}
	}
	lifecycle.setNetOK(okResult)
	go p.health.RecordResult(nodeHash, okResult)
}

func (p *Socks5Proxy) copySocks5UDPClientToUpstream(
	ctx context.Context,
	clientConn N.NetPacketConn,
	upstreamConn N.NetPacketConn,
	allowedClientIP string,
) (result struct {
	n   int64
	err error
}) {
	for {
		if err := clientConn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			result.err = err
			return
		}
		buffer := buf.NewPacket()
		source, err := clientConn.ReadPacket(buffer)
		if err != nil {
			buffer.Release()
			if ctx.Err() != nil {
				return
			}
			if isTemporaryTimeout(err) {
				continue
			}
			result.err = err
			return
		}
		if allowedClientIP != "" {
			sourceIP := remoteIPString(source.UDPAddr())
			if sourceIP != allowedClientIP {
				buffer.Release()
				continue
			}
		}
		if buffer.Len() < 3 {
			buffer.Release()
			continue
		}
		if buffer.Byte(2) != 0 {
			buffer.Release()
			continue
		}
		buffer.Advance(3)
		destination, err := M.SocksaddrSerializer.ReadAddrPort(buffer)
		if err != nil {
			buffer.Release()
			continue
		}
		payloadBytes := append([]byte(nil), buffer.Bytes()...)
		buffer.Release()
		payload := buf.As(payloadBytes)
		if err := upstreamConn.WritePacket(payload, destination); err != nil {
			result.err = err
			return
		}
		result.n += int64(3 + M.SocksaddrSerializer.AddrPortLen(destination) + len(payloadBytes))
	}
}

func (p *Socks5Proxy) copySocks5UDPUpstreamToClient(
	ctx context.Context,
	upstreamConn N.NetPacketConn,
	clientConn N.NetPacketConn,
	clientAddr *net.UDPAddr,
) (result struct {
	n   int64
	err error
}) {
	if clientAddr == nil {
		result.err = errors.New("missing UDP client address")
		return
	}
	for {
		if err := upstreamConn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			result.err = err
			return
		}
		buffer := buf.NewPacket()
		destination, err := upstreamConn.ReadPacket(buffer)
		if err != nil {
			buffer.Release()
			if ctx.Err() != nil {
				return
			}
			if isTemporaryTimeout(err) {
				continue
			}
			result.err = err
			return
		}
		payloadBytes := append([]byte(nil), buffer.Bytes()...)
		buffer.Release()
		outBuffer := buf.NewSize(3 + M.SocksaddrSerializer.AddrPortLen(destination) + len(payloadBytes))
		if _, err := outBuffer.Write([]byte{0x00, 0x00, 0x00}); err != nil {
			outBuffer.Release()
			result.err = err
			return
		}
		if err := M.SocksaddrSerializer.WriteAddrPort(outBuffer, destination); err != nil {
			outBuffer.Release()
			result.err = err
			return
		}
		if _, err := outBuffer.Write(payloadBytes); err != nil {
			outBuffer.Release()
			result.err = err
			return
		}
		packetLen := len(outBuffer.Bytes())
		writeErr := clientConn.WritePacket(outBuffer, M.SocksaddrFromNet(clientAddr))
		if writeErr != nil {
			result.err = writeErr
			return
		}
		result.n += int64(packetLen)
	}
}

func (p *Socks5Proxy) bindAdvertiseHost() string {
	if p.advertiseHost != "" {
		return p.advertiseHost
	}
	if p.listenAddress != "" && p.listenAddress != "0.0.0.0" && p.listenAddress != "::" {
		return p.listenAddress
	}
	return "127.0.0.1"
}

func (p *Socks5Proxy) bindAddress() M.Socksaddr {
	if p.listenPort <= 0 {
		return M.Socksaddr{}
	}
	return M.ParseSocksaddr(net.JoinHostPort(p.bindAdvertiseHost(), fmt.Sprintf("%d", p.listenPort)))
}

func socksReplyCodeForProxyError(pe *ProxyError) byte {
	if pe == nil {
		return socks5.ReplyCodeFailure
	}
	switch pe {
	case ErrAuthRequired, ErrAuthFailed:
		return socks5.ReplyCodeNotAllowed
	case ErrPlatformNotFound, ErrInvalidHost, ErrURLParseError, ErrInvalidProtocol:
		return socks5.ReplyCodeHostUnreachable
	case ErrNoAvailableNodes:
		return socks5.ReplyCodeHostUnreachable
	case ErrUpstreamTimeout:
		return socks5.ReplyCodeTTLExpired
	case ErrUpstreamConnectFailed:
		return socks5.ReplyCodeHostUnreachable
	case ErrUpstreamRequestFailed:
		return socks5.ReplyCodeFailure
	default:
		return socks5.ReplyCodeFailure
	}
}

func socksReplyCodeForError(err error, fallback *ProxyError) byte {
	if code := socks5.ReplyCodeForError(err); code != socks5.ReplyCodeFailure {
		return code
	}
	return socksReplyCodeForProxyError(fallback)
}

func remoteIPString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}

func isTemporaryTimeout(err error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return false
}
