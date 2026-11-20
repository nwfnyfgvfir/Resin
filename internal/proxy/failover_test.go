package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
)

const failoverAccount = "acct"

func seedFailoverLease(t *testing.T, env *proxyE2EEnv) model.Lease {
	t.Helper()
	route, err := env.router.RouteRequest("plat", failoverAccount, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	lease := env.router.ReadLease(model.LeaseKey{PlatformID: route.PlatformID, Account: failoverAccount})
	if lease == nil {
		t.Fatal("missing initial lease")
	}
	return *lease
}

func addFailoverNode(t *testing.T, env *proxyE2EEnv, tag, ip string, latency time.Duration, dial func(context.Context, string, M.Socksaddr) (net.Conn, error)) node.Hash {
	t.Helper()
	raw := []byte(fmt.Sprintf(`{"type":"stub","server":%q,"server_port":1}`, tag))
	hash := node.HashFromRawOptions(raw)
	env.sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: []string{tag}})
	env.pool.AddNodeFromSub(hash, raw, env.sub.ID)
	entry, ok := env.pool.GetEntry(hash)
	if !ok {
		t.Fatal("failover node not added")
	}
	var ob adapter.Outbound = &mockOutbound{dialFunc: dial}
	entry.Outbound.Store(&ob)
	entry.SetEgressIP(netip.MustParseAddr(ip))
	entry.LatencyTable.Update("example.com", latency, time.Minute)
	env.pool.RecordResult(hash, true)
	return hash
}

type closeTrackingBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *closeTrackingBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, net.ErrClosed
	}
	return b.Reader.Read(p)
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func serveFailoverPOST(env *proxyE2EEnv, kind, upstreamURL string, body io.Reader, events EventEmitter) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	if kind == "reverse" {
		proxy := NewReverseProxy(ReverseProxyConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, PlatformLookup: env.pool, Events: events})
		path := "/tok/plat:" + failoverAccount + "/http/" + strings.TrimPrefix(upstreamURL, "http://") + "/upload"
		proxy.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, body))
	} else {
		proxy := NewForwardProxy(ForwardProxyConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, Events: events})
		req := httptest.NewRequest(http.MethodPost, upstreamURL+"/upload", body)
		req.Header.Set("Proxy-Authorization", basicAuth("plat."+failoverAccount, "tok"))
		proxy.ServeHTTP(w, req)
	}
	return w
}

func TestHTTPProxy_FailoverPreservesUnreadBodyAndExit(t *testing.T) {
	for _, kind := range []string{"forward", "reverse"} {
		t.Run(kind, func(t *testing.T) {
			env := newProxyE2EEnv(t)
			first := seedFailoverLease(t, env)
			const payload = "streamed POST body"
			var posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				data, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || string(data) != payload {
					t.Errorf("upstream received %s with body %q, error %v", r.Method, data, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer upstream.Close()

			dials := make(chan string, 4)
			fail := func(name string) func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				return func(context.Context, string, M.Socksaddr) (net.Conn, error) {
					dials <- name
					return nil, errors.New("dial failed")
				}
			}
			setProxyE2EOutboundDialFunc(t, env, fail("first"))
			addFailoverNode(t, env, "other", "203.0.113.11", time.Millisecond, fail("other"))
			addFailoverNode(t, env, "same-failed", first.EgressIP, 5*time.Millisecond, fail("same-failed"))
			winner := addFailoverNode(t, env, "same-ok", first.EgressIP, 10*time.Millisecond, func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
				dials <- "same-ok"
				return (&net.Dialer{}).DialContext(ctx, network, dest.String())
			})

			events := newMockEventEmitter()
			w := serveFailoverPOST(env, kind, upstream.URL, &closeTrackingBody{Reader: strings.NewReader(payload)}, events)
			if w.Code != http.StatusCreated || posts.Load() != 1 {
				t.Fatalf("status=%d, upstream posts=%d, reply=%q", w.Code, posts.Load(), w.Body.String())
			}
			if len(dials) != 3 {
				t.Fatalf("dial count=%d, want 3", len(dials))
			}
			for _, want := range []string{"first", "same-failed", "same-ok"} {
				if got := <-dials; got != want {
					t.Fatalf("dialed %s, want %s", got, want)
				}
			}
			if log := <-events.logCh; log.NodeHash != winner.Hex() || log.EgressIP != first.EgressIP {
				t.Fatalf("logged node=%s exit=%s, want %s %s", log.NodeHash, log.EgressIP, winner.Hex(), first.EgressIP)
			}
			lease := env.router.ReadLease(model.LeaseKey{PlatformID: first.PlatformID, Account: failoverAccount})
			if lease == nil || lease.NodeHash != winner.Hex() || lease.ExpiryNs != first.ExpiryNs || lease.CreatedAtNs != first.CreatedAtNs {
				t.Fatalf("same-exit lease after failover = %+v, original = %+v", lease, first)
			}
		})
	}
}

func TestHTTPProxy_FailoverDoesNotReplaySentPOST(t *testing.T) {
	for _, kind := range []string{"forward", "reverse"} {
		t.Run(kind, func(t *testing.T) {
			env := newProxyE2EEnv(t)
			first := seedFailoverLease(t, env)
			var posts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			}))
			defer upstream.Close()
			setProxyE2EOutboundDialFunc(t, env, func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, dest.String())
			})
			var backupDials atomic.Int32
			addFailoverNode(t, env, "backup", first.EgressIP, time.Millisecond, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				backupDials.Add(1)
				return nil, errors.New("unexpected backup dial")
			})

			w := serveFailoverPOST(env, kind, upstream.URL, strings.NewReader("record a transaction"), NoOpEventEmitter{})
			if w.Code != http.StatusBadGateway || posts.Load() != 1 || backupDials.Load() != 0 {
				t.Fatalf("status=%d, upstream posts=%d, backup dials=%d", w.Code, posts.Load(), backupDials.Load())
			}
		})
	}
}

func TestTunnelFailoverExhaustsOnlyOriginalPlatform(t *testing.T) {
	env := newProxyE2EEnv(t)
	first := seedFailoverLease(t, env)
	plat, _ := env.pool.GetPlatform(first.PlatformID)
	plat.RegexFilters.MustNot = []*regexp.Regexp{regexp.MustCompile(`/outside$`)}
	env.pool.RebuildPlatform(plat)
	outside := platform.NewPlatform("other-plat", "other-plat", []*regexp.Regexp{regexp.MustCompile(`/outside$`)}, nil)
	env.pool.RegisterPlatform(outside)

	var dials []string
	dialErr := errors.New("target unreachable")
	fail := func(name string) func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		return func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			dials = append(dials, name)
			return nil, dialErr
		}
	}
	setProxyE2EOutboundDialFunc(t, env, fail("first"))
	addFailoverNode(t, env, "outside", first.EgressIP, time.Millisecond, fail("outside"))
	addFailoverNode(t, env, "other", "203.0.113.11", time.Millisecond, fail("other"))
	addFailoverNode(t, env, "same", first.EgressIP, 10*time.Millisecond, fail("same"))
	addFailoverNode(t, env, "other2", "203.0.113.12", time.Millisecond, fail("other2"))
	if outside.View().Size() != 1 {
		t.Fatal("fixture must contain an available node in the other platform")
	}
	result := prepareConnectTunnel(context.Background(), tunnelDeps{router: env.router, pool: env.pool}, "plat", failoverAccount, "example.com:443")
	if result.proxyErr != ErrUpstreamConnectFailed || result.upstreamErr != dialErr || result.session != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(dials) != 4 || !slices.Equal(dials[:2], []string{"first", "same"}) {
		t.Fatalf("dial sequence = %v", dials)
	}
	slices.Sort(dials[2:])
	if !slices.Equal(dials[2:], []string{"other", "other2"}) {
		t.Fatalf("different exit dials = %v", dials[2:])
	}
	lease := env.router.ReadLease(model.LeaseKey{PlatformID: first.PlatformID, Account: failoverAccount})
	if lease == nil || lease.NodeHash != first.NodeHash || lease.ExpiryNs != first.ExpiryNs {
		t.Fatalf("failed alternate replaced lease: %+v", lease)
	}
}

func TestTunnelFailoverChangesExit(t *testing.T) {
	for _, kind := range []string{"CONNECT", "SOCKS5"} {
		t.Run(kind, func(t *testing.T) {
			env := newProxyE2EEnv(t)
			first := seedFailoverLease(t, env)
			var dials []string
			setProxyE2EOutboundDialFunc(t, env, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				dials = append(dials, "first")
				return nil, errors.New("initial dial failed")
			})
			addFailoverNode(t, env, "same", first.EgressIP, time.Millisecond, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				dials = append(dials, "same")
				return nil, errors.New("same-exit dial failed")
			})
			const replacementIP = "203.0.113.11"
			const payload = "tunneled payload"
			winner := addFailoverNode(t, env, "other", replacementIP, time.Millisecond, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				dials = append(dials, "other")
				conn, peer := net.Pipe()
				go func() {
					defer peer.Close()
					buf := make([]byte, len(payload))
					if _, err := io.ReadFull(peer, buf); err == nil {
						_, _ = peer.Write(buf)
					}
				}()
				return conn, nil
			})

			events := newMockEventEmitter()
			const dest = "127.0.0.1:12345"
			var conn net.Conn
			var reader *bufio.Reader
			if kind == "CONNECT" {
				proxy := NewForwardProxy(ForwardProxyConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, Events: events})
				server := httptest.NewServer(proxy)
				defer server.Close()
				var err error
				conn, err = net.Dial("tcp", server.Listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				reader = bufio.NewReader(conn)
				req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", dest, dest, basicAuth("plat."+failoverAccount, "tok"))
				writeAll(t, conn, []byte(req))
				resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("CONNECT reply = %v, error %v", resp, err)
				}
			} else {
				proxy := NewSocks5Inbound(Socks5InboundConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, Events: events})
				conn, reader, _ = startSocks5Session(t, proxy)
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				writeAll(t, conn, []byte{socks5Version, 1, socks5MethodUserPass})
				if reply := readExactly(t, reader, 2); reply[1] != socks5MethodUserPass {
					t.Fatalf("unexpected SOCKS5 method reply: %v", reply)
				}
				writeAll(t, conn, socks5UserPassPacket("plat."+failoverAccount, "tok"))
				if reply := readExactly(t, reader, 2); reply[1] != socks5UserPassStatusSuccess {
					t.Fatalf("unexpected SOCKS5 auth reply: %v", reply)
				}
				writeAll(t, conn, socks5ConnectIPv4Packet(dest))
				if reply := readExactly(t, reader, 10); reply[1] != socks5ReplySucceeded {
					t.Fatalf("unexpected SOCKS5 connect reply: %v", reply)
				}
			}

			writeAll(t, conn, []byte(payload))
			if echo := readExactly(t, reader, len(payload)); string(echo) != payload {
				t.Fatalf("unexpected echo: %q", echo)
			}
			_ = conn.Close()
			select {
			case log := <-events.logCh:
				if log.NodeHash != winner.Hex() || log.EgressIP != replacementIP {
					t.Fatalf("logged node=%s exit=%s, want %s %s", log.NodeHash, log.EgressIP, winner.Hex(), replacementIP)
				}
			case <-time.After(time.Second):
				t.Fatal("expected completed tunnel log")
			}
			if !slices.Equal(dials, []string{"first", "same", "other"}) {
				t.Fatalf("dial sequence = %v", dials)
			}
			lease := env.router.ReadLease(model.LeaseKey{PlatformID: first.PlatformID, Account: failoverAccount})
			if lease == nil || lease.NodeHash != winner.Hex() || lease.EgressIP != replacementIP || lease.ExpiryNs-lease.CreatedAtNs != int64(time.Hour) {
				t.Fatalf("replacement lease = %+v", lease)
			}
			load := env.router.SnapshotIPLoad(first.PlatformID)
			if load[netip.MustParseAddr(first.EgressIP)] != 0 || load[netip.MustParseAddr(replacementIP)] != 1 {
				t.Fatalf("unexpected per-exit load: %v", load)
			}
		})
	}
}

func TestTunnelFailoverStopsOnCancellation(t *testing.T) {
	env := newProxyE2EEnv(t)
	first := seedFailoverLease(t, env)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setProxyE2EOutboundDialFunc(t, env, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		cancel()
		return nil, context.Canceled
	})
	addFailoverNode(t, env, "backup", first.EgressIP, time.Millisecond, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		t.Error("dialed an alternate after cancellation")
		return nil, errors.New("unexpected dial")
	})
	result := prepareConnectTunnel(ctx, tunnelDeps{router: env.router, pool: env.pool}, "plat", failoverAccount, "example.com:443")
	if !result.canceled || result.proxyErr != nil || result.session != nil {
		t.Fatalf("unexpected canceled result: %+v", result)
	}
}
