package rule

import (
	"context"
	"testing"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/require"
)

func TestSniffOverrideDestinationMatchesLegacyExclusions(t *testing.T) {
	var options option.RuleAction
	require.NoError(t, json.Unmarshal([]byte(`{"action":"sniff","override_destination":true,"override_excluded_domains":["courier.push.apple.com","Mijia Cloud","DLG.IO.MI.COM"]}`), &options))
	action, err := NewRuleAction(context.Background(), log.NewNOPFactory().NewLogger("test"), options)
	require.NoError(t, err)
	matcher := action.(*RuleActionSniff)
	for _, tc := range []struct {
		domain string
		want   bool
	}{
		{"example.com", true},
		{"courier.push.apple.com", false},
		{"x.COURIER.PUSH.APPLE.COM.example", false},
		{"api.dlg.io.mi.com", false},
		{"dlg.io.mi.co", true},
		{"Mijia Cloud", false},
		{"", false},
		{"127.0.0.1", false},
	} {
		t.Run(tc.domain, func(t *testing.T) { require.Equal(t, tc.want, matcher.ShouldOverrideDestination(tc.domain)) })
	}
}
