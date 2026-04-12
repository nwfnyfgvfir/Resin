package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/proxy"
)

var multiplexReadDeadline = 2 * time.Second

type bufferedPrefixedConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedPrefixedConn(conn net.Conn, reader *bufio.Reader) net.Conn {
	if reader == nil {
		return conn
	}
	return &bufferedPrefixedConn{
		Conn:   conn,
		reader: reader,
	}
}

func (c *bufferedPrefixedConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}

func serveMixedInboundConn(
	conn net.Conn,
	deploymentProfile config.DeploymentProfile,
	httpHandler http.Handler,
	socks5Proxy *proxy.Socks5Proxy,
) {
	if conn == nil {
		return
	}
	if httpHandler == nil {
		_ = conn.Close()
		return
	}

	protocol, prefixedConn, err := classifyInboundConn(conn, deploymentProfile, socks5Proxy != nil)
	if err != nil {
		_ = conn.Close()
		return
	}

	switch protocol {
	case inboundProtocolSOCKS5:
		if socks5Proxy == nil {
			_ = prefixedConn.Close()
			return
		}
		socks5Proxy.ServeConn(prefixedConn)
	default:
		http.Serve(&singleConnListener{conn: prefixedConn}, httpHandler)
	}
}

type inboundProtocol string

const (
	inboundProtocolHTTP   inboundProtocol = "http"
	inboundProtocolSOCKS5 inboundProtocol = "socks5"
)

func classifyInboundConn(conn net.Conn, deploymentProfile config.DeploymentProfile, allowSocks5 bool) (inboundProtocol, net.Conn, error) {
	if deploymentProfile.SharesSocks5OnResinPort() && allowSocks5 {
		_ = conn.SetReadDeadline(time.Now().Add(multiplexReadDeadline))
		reader := bufio.NewReader(conn)
		peek, err := reader.Peek(1)
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil {
			return "", nil, err
		}
		prefixedConn := newBufferedPrefixedConn(conn, reader)
		if len(peek) > 0 && peek[0] == 0x05 {
			return inboundProtocolSOCKS5, prefixedConn, nil
		}
		return inboundProtocolHTTP, prefixedConn, nil
	}
	return inboundProtocolHTTP, conn, nil
}

type singleConnListener struct {
	conn net.Conn
	used bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.used || l.conn == nil {
		return nil, io.EOF
	}
	l.used = true
	return l.conn, nil
}

func (l *singleConnListener) Close() error {
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.conn == nil {
		return &net.TCPAddr{}
	}
	return l.conn.LocalAddr()
}

func describeSocks5Listener(envCfg *config.EnvConfig) string {
	if envCfg == nil {
		return "disabled"
	}
	if envCfg.DeploymentProfile.SharesSocks5OnResinPort() {
		return fmt.Sprintf("shared on %s", formatListenAddress(envCfg.ListenAddress, envCfg.ResinPort))
	}
	if envCfg.Socks5Port <= 0 {
		return "disabled"
	}
	return formatListenAddress(envCfg.ListenAddress, envCfg.Socks5Port)
}

func socks5EffectivePort(envCfg *config.EnvConfig) int {
	if envCfg == nil {
		return 0
	}
	if envCfg.DeploymentProfile.SharesSocks5OnResinPort() {
		return envCfg.ResinPort
	}
	return envCfg.Socks5Port
}

func socks5PortExposedSeparately(envCfg *config.EnvConfig) bool {
	if envCfg == nil {
		return false
	}
	return envCfg.Socks5Port > 0 && !envCfg.DeploymentProfile.SharesSocks5OnResinPort()
}

func describeDeploymentProfileBehavior(envCfg *config.EnvConfig) string {
	if envCfg == nil {
		return ""
	}
	if envCfg.DeploymentProfile.SharesSocks5OnResinPort() {
		return strings.TrimSpace(fmt.Sprintf("%s uses single-port HTTP/SOCKS5 multiplexing on RESIN_PORT", envCfg.DeploymentProfile))
	}
	return strings.TrimSpace(fmt.Sprintf("%s uses a dedicated SOCKS5 listener when RESIN_SOCKS5_PORT > 0", envCfg.DeploymentProfile))
}
