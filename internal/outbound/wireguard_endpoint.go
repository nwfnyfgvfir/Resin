package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	sJson "github.com/sagernet/sing/common/json"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

func hasWireGuardEndpointAddress(raw json.RawMessage) bool {
	var header struct {
		Address json.RawMessage `json:"address"`
	}
	return json.Unmarshal(raw, &header) == nil && len(header.Address) != 0
}

func (b *SingboxBuilder) buildWireGuardEndpoint(raw json.RawMessage) (adapter.Outbound, error) {
	var config option.Endpoint
	if err := sJson.UnmarshalContext(b.ctx, raw, &config); err != nil {
		return nil, fmt.Errorf("parse WireGuard endpoint options: %w", err)
	}
	options := config.Options.(*option.WireGuardEndpointOptions)
	if options.System {
		return nil, errors.New("WireGuard proxy endpoints require a userspace interface")
	}
	if len(options.Address) == 0 || len(options.Peers) == 0 {
		return nil, errors.New("WireGuard endpoint requires local addresses and peers")
	}
	for i, peer := range options.Peers {
		if peer.Address == "" || peer.Port == 0 || len(peer.AllowedIPs) == 0 {
			return nil, fmt.Errorf("WireGuard peer %d requires an address, port and allowed IPs", i+1)
		}
	}
	registry := service.FromContext[adapter.EndpointRegistry](b.ctx)
	startup := newWireGuardStartupLogger(b.logFactory.NewLogger("endpoint/wireguard"))
	endpoint, err := registry.Create(b.ctx, outboundEndpointRouter{}, startup, config.Tag, config.Type, options)
	if err != nil {
		return nil, fmt.Errorf("create WireGuard endpoint: %w", err)
	}
	return startOutbound(endpoint, startup)
}

// The pinned wireguard-go release acquires its state and IPC locks in opposite
// orders during the asynchronous initial Up and Close. The userspace device
// queues one initial Up event. Its state log follows release of the IPC lock,
// so wait for it before exposing the outbound to removal.
type wireGuardStartupLogger struct {
	log.ContextLogger
	once                 sync.Once
	interfaceUpAttempted chan struct{}
}

func newWireGuardStartupLogger(logger log.ContextLogger) *wireGuardStartupLogger {
	return &wireGuardStartupLogger{ContextLogger: logger, interfaceUpAttempted: make(chan struct{})}
}

func (l *wireGuardStartupLogger) Debug(args ...any) {
	if len(args) == 1 {
		if message, ok := args[0].(string); ok &&
			strings.HasPrefix(message, "interface state was ") && strings.Contains(message, ", requested Up, now ") {
			l.once.Do(func() { close(l.interfaceUpAttempted) })
		}
	}
	l.ContextLogger.Debug(args...)
}

var errWireGuardInbound = errors.New("inbound connections on WireGuard proxy endpoints are disabled")

// A subscription endpoint is used for dialing. Handle all inbound callbacks
// that the WireGuard endpoint can invoke without a full sing-box router.
type outboundEndpointRouter struct {
	adapter.Router
}

func (outboundEndpointRouter) PreMatch(adapter.InboundContext) error {
	return errWireGuardInbound
}

func (outboundEndpointRouter) RouteConnectionEx(_ context.Context, conn net.Conn, _ adapter.InboundContext, onClose N.CloseHandlerFunc) {
	_ = N.CloseOnHandshakeFailure(conn, onClose, errWireGuardInbound)
}

func (outboundEndpointRouter) RoutePacketConnectionEx(_ context.Context, conn N.PacketConn, _ adapter.InboundContext, onClose N.CloseHandlerFunc) {
	_ = N.CloseOnHandshakeFailure(conn, onClose, errWireGuardInbound)
}
