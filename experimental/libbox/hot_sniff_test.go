package libbox_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/stretchr/testify/require"
)

type hotSniffTracker struct {
	adapter.ConnectionTracker
	metadata chan adapter.InboundContext
}

func (t *hotSniffTracker) RoutedConnection(_ context.Context, conn net.Conn, metadata adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) net.Conn {
	t.metadata <- metadata
	return conn
}

func TestHotSniffOverrideDestinationHTTP(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Host) }))
	defer httpServer.Close()
	destination := strings.TrimPrefix(httpServer.URL, "http://")
	for _, tc := range []struct {
		name, host, options string
		override            bool
	}{
		{"default keeps IP", "override.example", "", false},
		{"override destination", "override.example", `,"override_destination":true`, true},
		{"excluded exact", "courier.push.apple.com", `,"override_destination":true,"override_excluded_domains":["courier.push.apple.com"]`, false},
		{"excluded substring", "x.courier.push.apple.com.example", `,"override_destination":true,"override_excluded_domains":["courier.push.apple.com"]`, false},
		{"excluded case insensitive", "API.DLG.IO.MI.COM", `,"override_destination":true,"override_excluded_domains":["dlg.io.mi.com"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := hotPort(t)
			s := hotServer(t, fmt.Sprintf(`{"log":{"disabled":true},"dns":{"servers":[{"type":"hosts","tag":"hosts","predefined":{"override.example":["127.0.0.1"]}}]},"inbounds":[{"type":"socks","tag":"in","listen":"127.0.0.1","listen_port":%d}],"outbounds":[{"type":"direct","tag":"direct","domain_resolver":"hosts"}]}`, port))
			tracker := &hotSniffTracker{metadata: make(chan adapter.InboundContext, 1)}
			s.Instance().Box().Router().AppendTracker(tracker)
			require.NoError(t, s.AddRouteRule(`{"ruleTag":"sniff","action":"sniff","sniffer":["http"]`+tc.options+`}`, true))
			conn, err := hotDial(port, destination, nil)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
			_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", tc.host)
			require.NoError(t, err)
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			require.NoError(t, err)
			require.Equal(t, tc.host, string(body), "sniffing must preserve the original HTTP request")
			metadata := <-tracker.metadata
			require.True(t, strings.EqualFold(tc.host, metadata.Domain))
			if tc.override {
				require.Equal(t, tc.host, metadata.Destination.Fqdn)
				require.False(t, metadata.Destination.Addr.IsValid())
			} else {
				require.Equal(t, M.ParseSocksaddr(destination), metadata.Destination)
			}
			require.Equal(t, M.ParseSocksaddr(destination).Port, metadata.Destination.Port)
		})
	}
}
