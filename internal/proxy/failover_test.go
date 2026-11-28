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

func serveFailoverHTTP(env *proxyE2EEnv, kind, method, upstreamURL string, body io.Reader, events EventEmitter) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	if kind == "reverse" {
		proxy := NewReverseProxy(ReverseProxyConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, PlatformLookup: env.pool, Events: events})
		path := "/tok/plat:" + failoverAccount + "/http/" + strings.TrimPrefix(upstreamURL, "http://") + "/upload"
		proxy.ServeHTTP(w, httptest.NewRequest(method, path, body))
	} else {
		proxy := NewForwardProxy(ForwardProxyConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, Events: events})
		req := httptest.NewRequest(method, upstreamURL+"/upload", body)
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
			w := serveFailoverHTTP(env, kind, http.MethodPost, upstream.URL, &closeTrackingBody{Reader: strings.NewReader(payload)}, events)
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

			w := serveFailoverHTTP(env, kind, http.MethodPost, upstream.URL, strings.NewReader("record a transaction"), NoOpEventEmitter{})
			if w.Code != http.StatusBadGateway || posts.Load() != 1 || backupDials.Load() != 0 {
				t.Fatalf("status=%d, upstream posts=%d, backup dials=%d", w.Code, posts.Load(), backupDials.Load())
			}
		})
	}
}

func TestHTTPProxy_FailoverAfterEmptyResponseOnlyForSafeMethods(t *testing.T) {
	for _, kind := range []string{"forward", "reverse"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost} {
			t.Run(kind+"/"+method, func(t *testing.T) {
				env := newProxyE2EEnv(t)
				first := seedFailoverLease(t, env)
				const firstHopWait = 20 * time.Millisecond
				setProxyE2EOutboundDialFunc(t, env, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
					client, server := net.Pipe()
					go func() {
						defer server.Close()
						if _, err := http.ReadRequest(bufio.NewReader(server)); err == nil {
							time.Sleep(firstHopWait)
						}
					}()
					return client, nil
				})
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}))
				defer upstream.Close()
				winner := addFailoverNode(t, env, "backup", first.EgressIP, time.Millisecond,
					func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, network, dest.String())
					})
				events := newMockEventEmitter()
				w := serveFailoverHTTP(env, kind, method, upstream.URL, nil, events)
				log := <-events.logCh
				if method == http.MethodPost {
					if w.Code != http.StatusBadGateway || log.RetryCount != 0 {
						t.Fatalf("unsafe HTTP request replayed: status=%d retries=%d", w.Code, log.RetryCount)
					}
					return
				}
				if w.Code != http.StatusNoContent || log.NodeHash != winner.Hex() || log.RetryCount != 1 {
					t.Fatalf("status=%d log=%+v", w.Code, log)
				}
				if log.FirstFailureNodeHash != first.NodeHash || !strings.Contains(log.FirstFailureError, "EOF") {
					t.Fatalf("missing first failure: %+v", log)
				}
				if log.UpstreamFirstByteDurationNs <= 0 ||
					log.FirstByteDurationNs-log.UpstreamFirstByteDurationNs < int64(firstHopWait) {
					t.Fatalf("request TTFB=%d final hop TTFB=%d", log.FirstByteDurationNs, log.UpstreamFirstByteDurationNs)
				}
			})
		}
	}
}

func TestHTTPProxy_FailoverAfterStaleConnectionAndRedialFailure(t *testing.T) {
	env := newProxyE2EEnv(t)
	first := seedFailoverLease(t, env)
	var primaryDials atomic.Int32
	setProxyE2EOutboundDialFunc(t, env, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
		if primaryDials.Add(1) > 1 {
			return nil, errors.New("primary redial failed")
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			reader := bufio.NewReader(server)
			if _, err := http.ReadRequest(reader); err == nil {
				_, _ = io.WriteString(server, "HTTP/1.1 204 No Content\r\n\r\n")
				_, _ = http.ReadRequest(reader)
			}
		}()
		return client, nil
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	backup := addFailoverNode(t, env, "backup", first.EgressIP, time.Millisecond,
		func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, dest.String())
		})
	events := newMockEventEmitter()
	proxy := NewForwardProxy(ForwardProxyConfig{ProxyToken: "tok", Router: env.router, Pool: env.pool, Events: events})
	for _, wantNode := range []string{first.NodeHash, backup.Hex()} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, upstream.URL, nil).WithContext(ctx)
		req.Header.Set("Proxy-Authorization", basicAuth("plat."+failoverAccount, "tok"))
		proxy.ServeHTTP(w, req)
		cancel()
		log := <-events.logCh
		if w.Code != http.StatusNoContent || log.NodeHash != wantNode {
			t.Fatalf("status=%d log=%+v, want node %s", w.Code, log, wantNode)
		}
		if wantNode == backup.Hex() &&
			(log.RetryCount != 1 || log.FirstFailureNodeHash != first.NodeHash ||
				!strings.Contains(log.FirstFailureError, "primary redial failed") || primaryDials.Load() != 2) {
			t.Fatalf("redial was not followed by failover: dials=%d log=%+v", primaryDials.Load(), log)
		}
	}
}

func TestTunnelEarlyCloseRotatesLeaseWithPassiveCircuitBreakerDisabled(t *testing.T) {
	for _, requireBidirectional := range []bool{true, false} {
		t.Run(fmt.Sprintf("requireBidirectional=%t", requireBidirectional), func(t *testing.T) {
			env := newProxyE2EEnv(t)
			first := seedFailoverLease(t, env)
			plat, _ := env.pool.GetPlatform(first.PlatformID)
			plat.PassiveCircuitBreakerDisabled = true

			const payload = "first request bytes"
			setProxyE2EOutboundDialFunc(t, env, func(context.Context, string, M.Socksaddr) (net.Conn, error) {
				client, server := net.Pipe()
				go func() {
					defer server.Close()
					_, _ = io.ReadFull(server, make([]byte, len(payload)))
				}()
				return client, nil
			})
			backup := addFailoverNode(t, env, "backup", first.EgressIP, time.Millisecond, nil)
			prepare := prepareConnectTunnel(context.Background(), tunnelDeps{
				router: env.router, pool: env.pool, health: env.pool,
			}, "plat", failoverAccount, "example.com:443", nil)
			if prepare.session == nil {
				t.Fatalf("tunnel preparation failed: %+v", prepare)
			}
			proxyConn, clientConn := net.Pipe()
			defer clientConn.Close()
			_ = clientConn.SetDeadline(time.Now().Add(time.Second))
			// Keep the client write side open while waiting for a response.
			go func() { _, _ = io.WriteString(clientConn, payload) }()
			relay := pumpPreparedTunnel(proxyConn, nil, prepare.session, tunnelPumpOptions{
				requireBidirectionalTraffic: requireBidirectional,
			})
			if relay.netOK || !relay.earlyUpstreamFailure || relay.upstreamStage != "connect_no_ingress_traffic" {
				t.Fatalf("unexpected relay outcome: %+v", relay)
			}
			prepare.session.finish(relay)
			route, err := env.router.RouteRequest("plat", failoverAccount, "example.com")
			if err != nil || route.NodeHash != backup {
				t.Fatalf("account did not rotate to same exit: route=%+v err=%v", route, err)
			}
			lease := env.router.ReadLease(model.LeaseKey{PlatformID: first.PlatformID, Account: failoverAccount})
			if lease == nil || lease.ExpiryNs != first.ExpiryNs {
				t.Fatalf("same exit rotation changed lease expiry: %+v", lease)
			}
		})
	}
}

func TestTunnelFailoverStaysWithinBudgetAndPlatform(t *testing.T) {
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
	health := &mockHealthRecorder{}
	result := prepareConnectTunnel(context.Background(), tunnelDeps{router: env.router, pool: env.pool, health: health}, "plat", failoverAccount, "example.com:443", nil)
	if result.proxyErr != ErrUpstreamConnectFailed || result.upstreamErr != dialErr || result.session != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(dials) != maxOutboundAttempts || !slices.Equal(dials[:2], []string{"first", "same"}) {
		t.Fatalf("dial sequence = %v", dials)
	}
	if dials[2] != "other" && dials[2] != "other2" {
		t.Fatalf("different exit dials = %v", dials[2:])
	}
	lease := env.router.ReadLease(model.LeaseKey{PlatformID: first.PlatformID, Account: failoverAccount})
	if lease == nil || lease.NodeHash != first.NodeHash || lease.ExpiryNs != first.ExpiryNs {
		t.Fatalf("failed alternate replaced lease: %+v", lease)
	}
	if health.resultCalls.Load() != 0 || health.latencyCalls.Load() != 0 {
		t.Fatal("ambiguous failures across several exits generated health feedback")
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

			key := model.LeaseKey{PlatformID: first.PlatformID, Account: failoverAccount}
			if lease := env.router.ReadLease(key); lease == nil || lease.NodeHash != first.NodeHash {
				t.Fatalf("updated the binding before upstream sent bytes: %+v", lease)
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
	result := prepareConnectTunnel(ctx, tunnelDeps{router: env.router, pool: env.pool}, "plat", failoverAccount, "example.com:443", nil)
	if !result.canceled || result.proxyErr != nil || result.session != nil {
		t.Fatalf("unexpected canceled result: %+v", result)
	}
}

func TestOutboundDialTimeoutStopsAtConnectionAcquisition(t *testing.T) {
	const budget = 20 * time.Millisecond
	target := M.ParseSocksaddr("example.com:443")
	t.Run("blocking dial", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		ob := &mockOutbound{dialFunc: func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		conn, err := dialOutbound(parent, budget, ob, nil, "tcp", target)
		if conn != nil || !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil {
			t.Fatalf("dial budget returned conn=%v err=%v parent err=%v", conn, err, parent.Err())
		}
	})

	t.Run("connected stream", func(t *testing.T) {
		var streamCtx context.Context
		client, server := net.Pipe()
		defer server.Close()
		ob := &mockOutbound{dialFunc: func(ctx context.Context, _ string, _ M.Socksaddr) (net.Conn, error) {
			streamCtx = ctx
			return client, nil
		}}
		conn, err := dialOutbound(context.Background(), budget, ob, nil, "tcp", target)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-streamCtx.Done():
			t.Fatalf("dial ended the established stream: %v", streamCtx.Err())
		case <-time.After(2 * budget):
		}
		_ = conn.Close()
		if !errors.Is(streamCtx.Err(), context.Canceled) {
			t.Fatalf("closing the stream left its context active: %v", streamCtx.Err())
		}
	})

	t.Run("outbound ignores cancellation", func(t *testing.T) {
		var stalled atomic.Int32
		release := make(chan struct{})
		client, server := net.Pipe()
		defer server.Close()
		ob := &mockOutbound{dialFunc: func(context.Context, string, M.Socksaddr) (net.Conn, error) {
			select {
			case <-release:
			case <-time.After(time.Second):
			}
			return client, nil
		}}
		conn, err := dialOutbound(context.Background(), budget, ob, &stalled, "tcp", target)
		pending := stalled.Load()
		close(release)
		if conn != nil || !errors.Is(err, context.DeadlineExceeded) || pending != 1 {
			t.Fatalf("blocked outbound returned conn=%v err=%v stalled=%d", conn, err, pending)
		}
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := server.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("late outbound connection was not closed: %v", err)
		}
	})
}
