package libbox_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/daemon"
	"github.com/sagernet/sing-box/experimental/libbox"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/proxy"
)

func hotServer(t *testing.T, config string) *libbox.CommandServer {
	t.Helper()
	ctx := include.Context(service.ContextWithDefaultRegistry(context.Background()))
	return hotServerContext(t, ctx, config)
}

func hotServerContext(t *testing.T, ctx context.Context, config string) *libbox.CommandServer {
	t.Helper()
	ctx = filemanager.WithDefault(ctx, t.TempDir(), t.TempDir(), os.Getuid(), os.Getgid())
	s := daemon.NewStartedService(daemon.ServiceOptions{Context: ctx, LogMaxLines: 100})
	require.NoError(t, s.StartOrReloadService(ctx, config, nil))
	t.Cleanup(func() { require.NoError(t, s.CloseService()); s.Close() })
	return &libbox.CommandServer{StartedService: s}
}

type hotIdleOutbound struct {
	adapter.Outbound
	keep atomic.Bool
}

func (o *hotIdleOutbound) SetKeepIdleConnections(keep bool) { o.keep.Store(keep) }
func (o *hotIdleOutbound) CloseIdleConnections()            {}

func TestHotRulesRefreshOutboundReferences(t *testing.T) {
	ctx := include.Context(service.ContextWithDefaultRegistry(context.Background()))
	registry := service.FromContext[adapter.OutboundRegistry](ctx).(*outbound.Registry)
	outbound.Register(registry, "test-idle", func(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options option.DirectOutboundOptions) (adapter.Outbound, error) {
		o, err := direct.NewOutbound(ctx, router, logger, tag, options)
		return &hotIdleOutbound{Outbound: o}, err
	})
	s := hotServerContext(t, ctx, `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"},{"type":"test-idle","tag":"idle"}],"route":{"final":"direct"}}`)
	o, _ := s.Instance().Box().Outbound().Outbound("idle")
	keeper := o.(*hotIdleOutbound)
	require.False(t, keeper.keep.Load())
	require.NoError(t, s.AddRouteRule(`{"ruleTag":"active","inbound":["a"],"outbound":"idle"}`, true))
	require.True(t, keeper.keep.Load())
	require.NoError(t, s.RemoveRouteRule("active"))
	require.False(t, keeper.keep.Load())
}

func hotPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

func hotEcho(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}

func hotDial(port int, destination string, auth *proxy.Auth) (net.Conn, error) {
	d, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", port), auth, &net.Dialer{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	return d.Dial("tcp", destination)
}

func hotPing(c net.Conn) error {
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := c.Write([]byte("hot")); err != nil {
		return err
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(c, buf); err != nil {
		return err
	}
	if string(buf) != "hot" {
		return fmt.Errorf("unexpected echo: %q", buf)
	}
	return nil
}

func hotConnect(t *testing.T, port int, destination string, auth *proxy.Auth) net.Conn {
	t.Helper()
	c, err := hotDial(port, destination, auth)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	require.NoError(t, hotPing(c))
	return c
}

func TestHotOperationsPreserveOtherConnections(t *testing.T) {
	aPort, bPort := hotPort(t), hotPort(t)
	s := hotServer(t, fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"socks","tag":"a","listen":"127.0.0.1","listen_port":%d}],"outbounds":[{"type":"direct","tag":"a-out"}],"route":{"rules":[{"ruleTag":"cold-a","inbound":["a"],"outbound":"a-out"}],"final":"a-out"}}`, aPort))
	original := s.Instance()
	destination := hotEcho(t)
	a := hotConnect(t, aPort, destination, nil)

	require.NoError(t, s.AddOutbound(`{"type":"direct","tag":"b-out"}`))
	require.Error(t, s.AddOutbound(`{"type":"direct","tag":"b-out"}`))
	rule := `{"ruleTag":"b-route","inbound":["b"],"outbound":"b-out"}`
	require.NoError(t, s.AddRouteRule(rule, true))
	require.Error(t, s.AddRouteRule(rule, true))
	ib := fmt.Sprintf(`{"type":"socks","tag":"b","listen":"127.0.0.1","listen_port":%d}`, bPort)
	require.NoError(t, s.AddInbound(ib))
	require.Error(t, s.AddInbound(ib))
	hotConnect(t, bPort, destination, nil)
	require.NoError(t, hotPing(a))

	// A failed removal must keep B routable and leave the manager unchanged.
	require.Error(t, s.RemoveOutbound("b-out"))
	hotConnect(t, bPort, destination, nil)
	unreachable := hotPort(t)
	require.NoError(t, s.ReplaceOutbound(fmt.Sprintf(`{"type":"socks","tag":"b-out","server":"127.0.0.1","server_port":%d}`, unreachable)))
	blocked, blockedErr := hotDial(bPort, destination, nil)
	if blockedErr == nil {
		blockedErr = hotPing(blocked)
		blocked.Close()
	}
	require.Error(t, blockedErr)
	require.NoError(t, hotPing(a))
	require.NoError(t, s.ReplaceOutbound(`{"type":"direct","tag":"b-out"}`))
	hotConnect(t, bPort, destination, nil)
	require.NoError(t, hotPing(a))

	// Replacing a listener on the same port must apply its new authentication.
	authIB := fmt.Sprintf(`{"type":"socks","tag":"b","listen":"127.0.0.1","listen_port":%d,"users":[{"username":"user","password":"secret"}]}`, bPort)
	require.NoError(t, s.ReplaceInbound(authIB))
	c, err := hotDial(bPort, destination, nil)
	if c != nil {
		c.Close()
	}
	require.Error(t, err)
	hotConnect(t, bPort, destination, &proxy.Auth{User: "user", Password: "secret"})
	require.NoError(t, hotPing(a))

	// A conflicting replacement must restore the previous usable listener.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	require.Error(t, s.ReplaceInbound(fmt.Sprintf(`{"type":"socks","tag":"b","listen":"127.0.0.1","listen_port":%d}`, occupied.Addr().(*net.TCPAddr).Port)))
	hotConnect(t, bPort, destination, &proxy.Auth{User: "user", Password: "secret"})

	require.NoError(t, s.RemoveInbound("b"))
	c, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", bPort), time.Second)
	if c != nil {
		c.Close()
	}
	require.Error(t, err)
	require.NoError(t, s.RemoveRouteRule("b-route"))
	require.NoError(t, s.RemoveOutbound("b-out"))
	require.NoError(t, s.RemoveRouteRule("cold-a"))
	require.NoError(t, hotPing(a))
	require.Same(t, original, s.Instance())
}

func TestHotRoutingChangesNewTrafficAndPreservesFailureState(t *testing.T) {
	port := hotPort(t)
	s := hotServer(t, fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"socks","tag":"a","listen":"127.0.0.1","listen_port":%d}],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`, port))
	destination := hotEcho(t)
	a := hotConnect(t, port, destination, nil)
	require.NoError(t, s.AddRouteRule(`{"ruleTag":"deny","inbound":["a"],"action":"reject"}`, true))
	c, err := hotDial(port, destination, nil)
	if err == nil {
		err = hotPing(c)
		c.Close()
	}
	require.Error(t, err)
	require.NoError(t, hotPing(a))
	require.Error(t, s.AddRouteRule(`{"ruleTag":"bad","outbound":"missing"}`, false))
	require.NoError(t, s.RemoveRouteRule("deny"))
	hotConnect(t, port, destination, nil)
	require.NoError(t, s.AddRouteRule(`{"ruleTag":"old","inbound":["unused"],"action":"reject"}`, true))
	require.NoError(t, s.AddRouteRule(`{"ruleTag":"new","inbound":["unused"],"action":"reject"}`, false))
	require.Error(t, s.RemoveRouteRule("old"))
	require.NoError(t, s.RemoveRouteRule("new"))
}

func TestHotOperationsRejectInvalidStateAndDependencies(t *testing.T) {
	s := hotServer(t, `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"},{"type":"selector","tag":"group","outbounds":["direct"]}],"route":{"final":"group"}}`)
	require.Error(t, s.RemoveOutbound("direct"))
	require.Error(t, s.ReplaceOutbound(`{"type":"direct","tag":"direct"}`))
	_, exists := s.Instance().Box().Outbound().Outbound("direct")
	require.True(t, exists)
	require.Error(t, s.AddOutbound(`{"type":"direct"}`))
	require.Error(t, s.AddOutbound(`{"protocol":"freedom","tag":"wrong-schema"}`))
	require.Error(t, s.AddRouteRule(`{"action":"reject"}`, true))
	require.Error(t, s.RemoveInbound("missing"))
	require.NoError(t, s.CloseService())
	require.Error(t, s.AddOutbound(`{"type":"direct","tag":"new"}`))
	require.Error(t, s.AddEndpoint(`{"type":"wireguard","tag":"new"}`))
	require.Error(t, s.ReplaceEndpoint(`{"type":"wireguard","tag":"new"}`))
	require.Error(t, s.RemoveEndpoint("new"))
}

func TestHotReplaceImplicitDefaultOutbound(t *testing.T) {
	port := hotPort(t)
	s := hotServer(t, fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"socks","tag":"a","listen":"127.0.0.1","listen_port":%d}],"outbounds":[{"type":"direct","tag":"first"}]}`, port))
	destination := hotEcho(t)
	hotConnect(t, port, destination, nil)
	require.NoError(t, s.ReplaceOutbound(fmt.Sprintf(`{"type":"socks","tag":"first","server":"127.0.0.1","server_port":%d}`, hotPort(t))))
	c, err := hotDial(port, destination, nil)
	if err == nil {
		err = hotPing(c)
		c.Close()
	}
	require.Error(t, err)
	require.NoError(t, s.ReplaceOutbound(`{"type":"direct","tag":"first"}`))
	hotConnect(t, port, destination, nil)
}

func TestHotOperationsRejectStaticResources(t *testing.T) {
	s := hotServer(t, `{"log":{"disabled":true},"dns":{"servers":[{"type":"udp","tag":"dns","server":"127.0.0.1","detour":"proxy"}]},"outbounds":[{"type":"direct","tag":"direct"},{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":1}],"route":{"final":"direct","rule_set":[{"type":"inline","tag":"unused","rules":[{"ip_cidr":["127.0.0.0/8"]}]}]}}`)
	require.NoError(t, s.ReplaceOutbound(`{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":2}`))
	require.ErrorContains(t, s.RemoveOutbound("proxy"), "referenced")
	require.ErrorContains(t, s.AddInbound(`{"type":"tun","tag":"tun"}`), "TUN")
	require.ErrorContains(t, s.AddRouteRule(`{"ruleTag":"set","rule_set":["unused"],"action":"reject"}`, true), "rule_set")
}

func TestHotRoutesDuringConcurrentConnections(t *testing.T) {
	port := hotPort(t)
	s := hotServer(t, fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"socks","tag":"a","listen":"127.0.0.1","listen_port":%d}],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`, port))
	destination := hotEcho(t)
	var wg sync.WaitGroup
	errors := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			c, err := hotDial(port, destination, nil)
			if err == nil {
				err = hotPing(c)
				c.Close()
			}
			if err != nil {
				errors <- err
				return
			}
		}
	}()
	for range 20 {
		require.NoError(t, s.AddRouteRule(`{"ruleTag":"temporary","inbound":["unused"],"action":"reject"}`, true))
		require.NoError(t, s.RemoveRouteRule("temporary"))
	}
	wg.Wait()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
}

func TestHotOperationsSerializeWithServiceLifecycle(t *testing.T) {
	config := `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`
	s := hotServer(t, config)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 30 {
			// Stopped services reject updates; successful ones must target a live instance.
			_ = s.ReplaceOutbound(`{"type":"direct","tag":"direct"}`)
		}
	}()
	for range 3 {
		require.NoError(t, s.CloseService())
		require.NoError(t, s.StartedService.StartOrReloadService(context.Background(), config, nil))
	}
	wg.Wait()
	require.NoError(t, s.ReplaceOutbound(`{"type":"direct","tag":"direct"}`))
}
