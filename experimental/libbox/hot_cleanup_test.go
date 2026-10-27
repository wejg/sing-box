package libbox_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/require"
)

// Simulate a closer that fails after stopping; ordinary WireGuard closes cleanly.
type hotCloseFailure struct {
	adapter.Outbound
	kind   string
	closed atomic.Bool
}

func (o *hotCloseFailure) Type() string                   { return o.kind }
func (o *hotCloseFailure) Start(adapter.StartStage) error { return nil }
func (o *hotCloseFailure) Close() error {
	o.closed.Store(true)
	return errors.New("cleanup failed after stopping resource")
}

func TestHotOperationsReportCommittedStateAfterCloseError(t *testing.T) {
	ctx := include.Context(service.ContextWithDefaultRegistry(context.Background()))
	resources := make(map[string]*hotCloseFailure)
	create := func(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag, kind string) *hotCloseFailure {
		resource, err := direct.NewOutbound(ctx, router, logger, tag, option.DirectOutboundOptions{})
		require.NoError(t, err)
		value := &hotCloseFailure{Outbound: resource, kind: kind}
		resources[tag] = value
		return value
	}
	inbound.Register(service.FromContext[adapter.InboundRegistry](ctx).(*inbound.Registry), "test-close", func(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, _ struct{}) (adapter.Inbound, error) {
		return create(ctx, router, logger, tag, "test-close"), nil
	})
	outbound.Register(service.FromContext[adapter.OutboundRegistry](ctx).(*outbound.Registry), "test-close", func(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, _ struct{}) (adapter.Outbound, error) {
		return create(ctx, router, logger, tag, "test-close"), nil
	})
	endpoint.Register(service.FromContext[adapter.EndpointRegistry](ctx).(*endpoint.Registry), "wireguard", func(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, _ option.WireGuardEndpointOptions) (adapter.Endpoint, error) {
		return create(ctx, router, logger, tag, "wireguard"), nil
	})
	s := hotServerContext(t, ctx, `{"log":{"disabled":true},"inbounds":[{"type":"test-close","tag":"in"}],"outbounds":[{"type":"direct","tag":"direct"},{"type":"test-close","tag":"out"}],"endpoints":[{"type":"wireguard","tag":"ep"}],"route":{"final":"direct"}}`)
	previousInbound := resources["in"]
	require.NoError(t, s.ReplaceInbound(`{"type":"test-close","tag":"in"}`))
	currentInbound, exists := s.Instance().Box().Inbound().Get("in")
	require.True(t, exists)
	require.Same(t, resources["in"], currentInbound)
	require.True(t, previousInbound.closed.Load())
	for _, tc := range []struct {
		tag    string
		remove func(string) error
		exists func(string) bool
	}{
		{"in", s.RemoveInbound, func(tag string) bool { _, ok := s.Instance().Box().Inbound().Get(tag); return ok }},
		{"out", s.RemoveOutbound, func(tag string) bool { _, ok := s.Instance().Box().Outbound().Outbound(tag); return ok }},
		{"ep", s.RemoveEndpoint, func(tag string) bool { _, ok := s.Instance().Box().Endpoint().Get(tag); return ok }},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			require.NoError(t, tc.remove(tc.tag), "committed removal must be reported as success so callers record undo")
			require.True(t, resources[tc.tag].closed.Load())
			require.False(t, tc.exists(tc.tag))
			require.Error(t, tc.remove(tc.tag), "a missing tag still rejects removal")
		})
	}
}
