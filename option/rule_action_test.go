package option

import (
	"context"
	"testing"

	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing/common/json"

	"github.com/stretchr/testify/require"
)

func TestDNSRuleActionRespondUnmarshalJSON(t *testing.T) {
	t.Parallel()

	var action DNSRuleAction
	err := json.UnmarshalContext(context.Background(), []byte(`{"action":"respond"}`), &action)
	require.NoError(t, err)
	require.Equal(t, C.RuleActionTypeRespond, action.Action)
	require.Equal(t, DNSRouteActionOptions{}, action.RouteOptions)
}

func TestDNSRuleActionRespondRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	var action DNSRuleAction
	err := json.UnmarshalContext(context.Background(), []byte(`{"action":"respond","disable_cache":true}`), &action)
	require.ErrorContains(t, err, "unknown field")
}

func TestSniffOverrideOptionsRoundTrip(t *testing.T) {
	content := `{"action":"sniff","sniffer":["http","tls"],"override_destination":true,"override_excluded_domains":["courier.push.apple.com","Mijia Cloud","dlg.io.mi.com"]}`
	var action RuleAction
	require.NoError(t, json.Unmarshal([]byte(content), &action))
	encoded, err := json.Marshal(action)
	require.NoError(t, err)
	require.JSONEq(t, content, string(encoded))
}
