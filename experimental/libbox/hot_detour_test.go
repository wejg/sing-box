package libbox_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	mDNS "github.com/miekg/dns"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/dns"
	"github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"
)

func TestHotDetourFollowsReplacement(t *testing.T) {
	s := hotServer(t, `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"proxy"}]}`)
	d := dialer.NewDetour(s.Instance().Box().Outbound(), "proxy", true)
	dest := M.ParseSocksaddr(hotEcho(t))
	c, err := d.DialContext(context.Background(), "tcp", dest)
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, hotPing(c))
	require.NoError(t, s.ReplaceOutbound(fmt.Sprintf(`{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":%d}`, hotPort(t))))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	replacement, err := d.DialContext(ctx, "tcp", dest)
	if replacement != nil {
		replacement.Close()
	}
	require.Error(t, err, "new detour dial must use replaced SOCKS outbound")
	require.NoError(t, hotPing(c))
}

func TestHotDNSDetourFollowsReplacement(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	dnsServer := &mDNS.Server{Listener: listener, Handler: mDNS.HandlerFunc(func(w mDNS.ResponseWriter, q *mDNS.Msg) {
		answer := new(mDNS.Msg).SetReply(q)
		answer.Answer = []mDNS.RR{&mDNS.A{Hdr: mDNS.RR_Header{Name: q.Question[0].Name, Rrtype: mDNS.TypeA, Class: mDNS.ClassINET}, A: net.IPv4(192, 0, 2, 1)}}
		_ = w.WriteMsg(answer)
	})}
	go dnsServer.ActivateAndServe()
	t.Cleanup(func() { _ = dnsServer.Shutdown() })
	proxyPort := hotPort(t)
	hotServer(t, fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"socks","tag":"socks","listen":"127.0.0.1","listen_port":%d}],"outbounds":[{"type":"direct","tag":"direct"}]}`, proxyPort))
	ctx := include.Context(service.ContextWithDefaultRegistry(context.Background()))
	var dnsTransport adapter.DNSTransport
	registry := service.FromContext[adapter.DNSTransportRegistry](ctx).(*dns.TransportRegistry)
	dns.RegisterTransport(registry, "test-tcp", func(ctx context.Context, logger log.ContextLogger, tag string, options option.RemoteDNSServerOptions) (adapter.DNSTransport, error) {
		value, err := transport.NewTCP(ctx, logger, tag, options)
		dnsTransport = value
		return value, err
	})
	s := hotServerContext(t, ctx, fmt.Sprintf(`{"log":{"disabled":true},"dns":{"servers":[{"type":"test-tcp","tag":"dns","server":"127.0.0.1","server_port":%d,"detour":"proxy"}]},"outbounds":[{"type":"direct","tag":"direct"},{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":%d}],"route":{"final":"direct"}}`, listener.Addr().(*net.TCPAddr).Port, proxyPort))
	exchange := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := dnsTransport.Exchange(ctx, new(mDNS.Msg).SetQuestion("hot.example.", mDNS.TypeA))
		return err
	}
	require.NoError(t, exchange())
	require.NoError(t, s.ReplaceOutbound(fmt.Sprintf(`{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":%d}`, hotPort(t))))
	dnsTransport.Reset() // Force a new dial; established pooled connections may keep using the previous path.
	require.Error(t, exchange())
	require.NoError(t, s.ReplaceOutbound(fmt.Sprintf(`{"type":"socks","tag":"proxy","server":"127.0.0.1","server_port":%d}`, proxyPort)))
	require.NoError(t, exchange())
	require.Error(t, s.RemoveOutbound("proxy"))
}
