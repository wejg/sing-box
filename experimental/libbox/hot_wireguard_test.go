//go:build with_wireguard && with_gvisor

package libbox_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/common/dialer"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"
)

func hotWGKeys(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(key.Bytes()), base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
}

func TestHotWireGuardLifecycleAndCrossKind(t *testing.T) {
	aPort := hotPort(t)
	s := hotServer(t, fmt.Sprintf(`{"log":{"disabled":true},"inbounds":[{"type":"socks","tag":"a","listen":"127.0.0.1","listen_port":%d}],"outbounds":[{"type":"direct","tag":"stable"},{"type":"direct","tag":"proxy"}],"route":{"final":"proxy","rules":[{"inbound":["a"],"outbound":"stable"}]}}`, aPort))
	original := s.Instance()
	stable := hotConnect(t, aPort, hotEcho(t), nil)
	manager := s.Instance().Box().Outbound()
	private, _ := hotWGKeys(t)
	config := fmt.Sprintf(`{"type":"wireguard","tag":"proxy","address":["10.251.0.1/32"],"private_key":%q}`, private)
	require.NoError(t, s.ReplaceOutbound(config))
	wg, found := s.Instance().Box().Endpoint().Get("proxy")
	require.True(t, found)
	require.Same(t, wg, manager.Default())
	require.NoError(t, hotPing(stable))
	require.Error(t, s.AddOutbound(config))
	require.Error(t, s.ReplaceOutbound(`{"type":"wireguard","tag":"proxy","private_key":"invalid"}`))
	unchanged, _ := manager.Outbound("proxy")
	require.Same(t, wg, unchanged)
	require.Error(t, s.RemoveOutbound("proxy"))
	require.NoError(t, s.ReplaceOutbound(`{"type":"direct","tag":"proxy"}`))
	_, found = s.Instance().Box().Endpoint().Get("proxy")
	require.False(t, found)
	require.Equal(t, "direct", manager.Default().Type())
	require.NoError(t, hotPing(stable))

	api, ok := any(s).(interface {
		AddEndpoint(string) error
		ReplaceEndpoint(string) error
		RemoveEndpoint(string) error
	})
	require.True(t, ok, "endpoint methods must be exported on CommandServer")
	config = fmt.Sprintf(`{"type":"wireguard","tag":"wg","address":["10.251.0.2/32"],"private_key":%q}`, private)
	require.NoError(t, api.AddEndpoint(config))
	require.Error(t, api.AddEndpoint(config))
	require.NoError(t, api.ReplaceEndpoint(config))
	require.NoError(t, api.RemoveEndpoint("wg"))
	require.Error(t, api.RemoveEndpoint("wg"))
	require.NoError(t, s.AddOutbound(config))
	require.NoError(t, s.RemoveOutbound("wg"))
	require.Same(t, original, s.Instance())
	require.NoError(t, hotPing(stable))
}

func TestHotWireGuardHandshakeAfterReplacement(t *testing.T) {
	serverPrivate, serverPublic := hotWGKeys(t)
	clientPrivate, clientPublic := hotWGKeys(t)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	port := udp.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, udp.Close())
	hotServer(t, fmt.Sprintf(`{"log":{"disabled":true},"endpoints":[{"type":"wireguard","tag":"server","address":["10.252.0.1/32"],"listen_port":%d,"private_key":%q,"peers":[{"public_key":%q,"allowed_ips":["10.252.0.2/32"]}]}],"outbounds":[{"type":"direct","tag":"direct"}]}`, port, serverPrivate, clientPublic))
	s := hotServer(t, `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"proxy"}]}`)
	config := fmt.Sprintf(`{"type":"wireguard","tag":"proxy","address":["10.252.0.2/32"],"private_key":%q,"peers":[{"address":"127.0.0.1","port":%d,"public_key":%q,"allowed_ips":["10.252.0.1/32"]}]}`, clientPrivate, port, serverPublic)
	_, echoPort, err := net.SplitHostPort(hotEcho(t))
	require.NoError(t, err)
	destination := M.ParseSocksaddr(net.JoinHostPort("10.252.0.1", echoPort))
	detour := dialer.NewDefaultOutboundDetour(s.Instance().Box().Outbound())
	for range 2 {
		require.NoError(t, s.ReplaceOutbound(config))
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		conn, err := detour.DialContext(ctx, "tcp", destination)
		cancel()
		require.NoError(t, err)
		require.NoError(t, hotPing(conn))
		require.NoError(t, conn.Close())
	}
	require.NoError(t, s.ReplaceOutbound(`{"type":"direct","tag":"proxy"}`))
}

func TestHotWireGuardFailurePreservesPreviousNode(t *testing.T) {
	s := hotServer(t, `{"log":{"disabled":true},"outbounds":[{"type":"direct","tag":"proxy"}]}`)
	manager := s.Instance().Box().Outbound()
	previous := manager.Default()
	private, _ := hotWGKeys(t)
	occupied, err := net.ListenPacket("udp", "0.0.0.0:0")
	require.NoError(t, err)
	defer occupied.Close()
	for index, config := range []string{
		`{"type":"wireguard","tag":"proxy","address":["10.251.0.1/32"],"private_key":"AA=="}`,
		fmt.Sprintf(`{"type":"wireguard","tag":"proxy","address":["10.251.0.1/32"],"private_key":%q,"system":true}`, private),
		fmt.Sprintf(`{"type":"wireguard","tag":"proxy","address":["10.251.0.1/32"],"private_key":%q,"detour":"missing"}`, private),
		fmt.Sprintf(`{"type":"wireguard","tag":"proxy","address":["10.251.0.1/32"],"private_key":%q,"detour":"proxy"}`, private),
		fmt.Sprintf(`{"type":"wireguard","tag":"proxy","address":["10.251.0.1/32"],"private_key":%q,"listen_port":%d}`, private, occupied.LocalAddr().(*net.UDPAddr).Port),
	} {
		err := s.ReplaceOutbound(config)
		require.Error(t, err, "case %d", index)
		require.NotContains(t, err.Error(), "private_key=", "errors must not expose WireGuard key material")
		current, exists := manager.Outbound("proxy")
		require.True(t, exists)
		require.Same(t, previous, current)
		_, exists = s.Instance().Box().Endpoint().Get("proxy")
		require.False(t, exists)
	}
	require.NoError(t, occupied.Close())
	config := fmt.Sprintf(`{"type":"wireguard","tag":"proxy","address":["10.251.0.1/32"],"private_key":%q,"listen_port":%d}`, private, occupied.LocalAddr().(*net.UDPAddr).Port)
	require.NoError(t, s.ReplaceOutbound(config))
	previous = manager.Default()
	require.Error(t, s.ReplaceOutbound(config), "a fixed listener cannot be rebound before the previous endpoint is closed")
	require.Same(t, previous, manager.Default())
}
