package daemon

import (
	"errors"
	"strings"
	"syscall"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route"
	R "github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

// Hot operations share the service lifecycle lock so they cannot race a reload or stop.
func (s *StartedService) withHotInstance(update func(*Instance) error) error {
	s.lifecycleAccess.Lock()
	defer s.lifecycleAccess.Unlock()
	s.serviceAccess.RLock()
	instance := s.instance
	ready := !s.closed && s.serviceStatus.Status == ServiceStatus_STARTED
	s.serviceAccess.RUnlock()
	if !ready || instance == nil || instance.instance == nil {
		return E.New("service is not started")
	}
	if err := update(instance); err != nil {
		return err
	}
	instance.instance.UpdateReferences(instance.options)
	return nil
}

func (s *StartedService) AddInbound(config string) error {
	return s.changeInbound(config, false)
}

func (s *StartedService) ReplaceInbound(config string) error {
	return s.changeInbound(config, true)
}

func (s *StartedService) changeInbound(config string, replace bool) error {
	return s.withHotInstance(func(i *Instance) error {
		options, err := json.UnmarshalExtendedContext[option.Inbound](i.ctx, []byte(config))
		if err != nil {
			return err
		}
		if strings.TrimSpace(options.Tag) == "" {
			return E.New("missing inbound tag")
		}
		manager := i.instance.Inbound()
		previousInbound, exists := manager.Get(options.Tag)
		if options.Type == C.TypeTun || exists && previousInbound.Type() == C.TypeTun {
			return E.New("TUN changes require a service reload")
		}
		if _, endpoint := i.instance.Endpoint().Get(options.Tag); endpoint {
			return E.New("tag belongs to an endpoint: ", options.Tag)
		}
		if exists && !replace {
			return E.New("existing tag found: ", options.Tag)
		}
		index := common.Index(i.options.Inbounds, func(it option.Inbound) bool { return it.Tag == options.Tag })
		if exists && index < 0 {
			return E.New("original inbound configuration unavailable: ", options.Tag)
		}
		create := func(value option.Inbound) error {
			return manager.Create(i.ctx, i.instance.Router(), i.logFactory.NewLogger("inbound/"+value.Type+"["+value.Tag+"]"), value.Tag, value.Type, value.Options)
		}
		err = create(options)
		if err != nil && exists && errors.Is(err, syscall.EADDRINUSE) {
			// The new listener cannot bind the old listener's port until it is closed.
			previous := i.options.Inbounds[index]
			removeErr := manager.Remove(options.Tag)
			if removeErr == nil {
				err = create(options)
			} else {
				err = removeErr
			}
			if err != nil {
				if rollbackErr := create(previous); rollbackErr != nil {
					return E.Errors(err, E.Cause(rollbackErr, "restore previous inbound"))
				}
				return err
			}
		}
		if err != nil {
			return err
		}
		if index >= 0 {
			i.options.Inbounds[index] = options
		} else {
			i.options.Inbounds = append(i.options.Inbounds, options)
		}
		return nil
	})
}

func (s *StartedService) RemoveInbound(tag string) error {
	return s.withHotInstance(func(i *Instance) error {
		if inbound, exists := i.instance.Inbound().Get(tag); exists && inbound.Type() == C.TypeTun {
			return E.New("TUN changes require a service reload")
		}
		if err := i.instance.Inbound().Remove(tag); err != nil {
			return err
		}
		i.options.Inbounds = common.Filter(i.options.Inbounds, func(it option.Inbound) bool { return it.Tag != tag })
		return nil
	})
}

func (s *StartedService) AddOutbound(config string) error {
	return s.changeNode(config, false, false)
}

func (s *StartedService) ReplaceOutbound(config string) error {
	return s.changeNode(config, true, false)
}

func (s *StartedService) AddEndpoint(config string) error {
	return s.changeNode(config, false, true)
}

func (s *StartedService) ReplaceEndpoint(config string) error {
	return s.changeNode(config, true, true)
}

func (s *StartedService) changeNode(config string, replace, endpointOnly bool) error {
	return s.withHotInstance(func(i *Instance) error {
		var header struct {
			Type string `json:"type"`
		}
		if err := json.UnmarshalContext(i.ctx, []byte(config), &header); err != nil {
			return err
		}
		isEndpoint := endpointOnly || header.Type == C.TypeWireGuard
		var options option.Outbound
		if isEndpoint {
			value, err := json.UnmarshalExtendedContext[option.Endpoint](i.ctx, []byte(config))
			if err != nil {
				return err
			}
			if value.Type != C.TypeWireGuard {
				return E.New("only WireGuard endpoints support hot updates")
			}
			if value.Options.(*option.WireGuardEndpointOptions).System {
				return E.New("system WireGuard changes require a service reload")
			}
			options = option.Outbound(value)
		} else {
			value, err := json.UnmarshalExtendedContext[option.Outbound](i.ctx, []byte(config))
			if err != nil {
				return err
			}
			options = value
		}
		if strings.TrimSpace(options.Tag) == "" {
			return E.New("missing outbound or endpoint tag")
		}
		_, exists := i.outboundManager.Outbound(options.Tag)
		_, wasEndpoint := i.endpointManager.Get(options.Tag)
		if exists && !replace {
			return E.New("existing tag found: ", options.Tag)
		}
		if wasEndpoint {
			if err := i.checkHotEndpoint(options.Tag); err != nil {
				return err
			}
		}
		if isEndpoint {
			if _, inbound := i.instance.Inbound().Get(options.Tag); inbound && !wasEndpoint {
				return E.New("tag belongs to an inbound: ", options.Tag)
			}
		}
		if exists {
			if err := i.checkOutboundDependents(options.Tag, true); err != nil {
				return err
			}
		}
		ctx := adapter.WithContext(i.ctx, &adapter.InboundContext{Outbound: options.Tag})
		logger := i.logFactory.NewLogger("outbound/" + options.Type + "[" + options.Tag + "]")
		if isEndpoint {
			if err := i.endpointManager.Create(ctx, i.instance.Router(), logger, options.Tag, options.Type, options.Options); err != nil {
				return err
			}
			if exists && !wasEndpoint {
				if err := i.outboundManager.Remove(options.Tag); err != nil {
					logger.Error(E.Cause(err, "close replaced outbound"))
				}
			}
			value := option.Endpoint(options)
			index := common.Index(i.options.Endpoints, func(it option.Endpoint) bool { return it.Tag == options.Tag })
			if index >= 0 {
				i.options.Endpoints[index] = value
			} else {
				i.options.Endpoints = append(i.options.Endpoints, value)
			}
			i.options.Outbounds = common.Filter(i.options.Outbounds, func(it option.Outbound) bool { return it.Tag != options.Tag })
		} else {
			if err := i.outboundManager.Create(ctx, i.instance.Router(), logger, options.Tag, options.Type, options.Options); err != nil {
				return err
			}
			if wasEndpoint {
				if err := i.endpointManager.Remove(options.Tag); err != nil {
					logger.Error(E.Cause(err, "close replaced endpoint"))
				}
			}
			index := common.Index(i.options.Outbounds, func(it option.Outbound) bool { return it.Tag == options.Tag })
			if index >= 0 {
				i.options.Outbounds[index] = options
			} else {
				i.options.Outbounds = append(i.options.Outbounds, options)
			}
			i.options.Endpoints = common.Filter(i.options.Endpoints, func(it option.Endpoint) bool { return it.Tag != options.Tag })
		}
		return nil
	})
}

func (i *Instance) checkHotEndpoint(tag string) error {
	endpoint, exists := i.endpointManager.Get(tag)
	if !exists {
		return E.New("endpoint not found: ", tag)
	}
	if endpoint.Type() != C.TypeWireGuard {
		return E.New("only WireGuard endpoints support hot updates")
	}
	for _, value := range i.options.Endpoints {
		if value.Tag == tag && value.Options.(*option.WireGuardEndpointOptions).System {
			return E.New("system WireGuard changes require a service reload")
		}
	}
	return nil
}

// Groups and other owners may retain outbound objects; DNS detours resolve them per dial.
func (i *Instance) checkOutboundDependents(tag string, replacing bool) error {
	for _, outbound := range append(i.outboundManager.Outbounds(), common.Map(i.endpointManager.Endpoints(), func(it adapter.Endpoint) adapter.Outbound { return it })...) {
		if outbound.Tag() != tag && common.Contains(outbound.Dependencies(), tag) {
			return E.New("outbound[", tag, "] is depended by ", outbound.Tag())
		}
	}
	referrers := []any{service.FromContext[adapter.HTTPClientManager](i.ctx)}
	for _, inbound := range i.instance.Inbound().Inbounds() {
		referrers = append(referrers, inbound)
	}
	if !replacing {
		for _, transport := range service.FromContext[adapter.DNSTransportManager](i.ctx).Transports() {
			referrers = append(referrers, transport)
		}
	}
	for _, boxService := range service.FromContext[adapter.ServiceManager](i.ctx).Services() {
		referrers = append(referrers, boxService)
	}
	for _, value := range referrers {
		if referrer, ok := value.(adapter.Referrer); ok && common.Contains(referrer.References(), tag) {
			return E.New("outbound is referenced by a running DNS, inbound, service or HTTP client: ", tag)
		}
	}
	if ntp := i.options.NTP; ntp != nil && ntp.Enabled && ntp.Detour == tag {
		return E.New("outbound is referenced by NTP: ", tag)
	}
	return nil
}

func (s *StartedService) RemoveOutbound(tag string) error {
	return s.removeNode(tag, false)
}

func (s *StartedService) RemoveEndpoint(tag string) error {
	return s.removeNode(tag, true)
}

func (s *StartedService) removeNode(tag string, endpointOnly bool) error {
	return s.withHotInstance(func(i *Instance) error {
		_, isEndpoint := i.endpointManager.Get(tag)
		if isEndpoint || endpointOnly {
			if err := i.checkHotEndpoint(tag); err != nil {
				return err
			}
		}
		if err := i.checkOutboundDependents(tag, false); err != nil {
			return err
		}
		if outbound := i.outboundManager.Default(); outbound != nil && outbound.Tag() == tag {
			return E.New("cannot remove the default outbound: ", tag)
		}
		for _, rule := range i.instance.Router().Rules() {
			var target string
			switch action := rule.Action().(type) {
			case *R.RuleActionRoute:
				target = action.Outbound
			case *R.RuleActionBypass:
				target = action.Outbound
			}
			if target == tag {
				return E.New("outbound is still used by a route rule: ", tag)
			}
		}
		if isEndpoint {
			if err := i.endpointManager.Remove(tag); err != nil {
				return err
			}
		} else if err := i.outboundManager.Remove(tag); err != nil {
			return err
		}
		i.options.Endpoints = common.Filter(i.options.Endpoints, func(it option.Endpoint) bool { return it.Tag != tag })
		i.options.Outbounds = common.Filter(i.options.Outbounds, func(it option.Outbound) bool { return it.Tag != tag })
		return nil
	})
}

func (s *StartedService) AddRouteRule(config string, shouldAppend bool) error {
	return s.withHotInstance(func(i *Instance) error {
		options, err := json.UnmarshalExtendedContext[option.Rule](i.ctx, []byte(config))
		if err != nil {
			return err
		}
		router, ok := i.instance.Router().(*route.Router)
		if !ok {
			return E.New("router does not support hot rules")
		}
		if err = router.AddRule(options, shouldAppend); err != nil {
			return err
		}
		if i.options.Route == nil {
			i.options.Route = new(option.RouteOptions)
		}
		if !shouldAppend {
			i.options.Route.Rules = nil
		}
		i.options.Route.Rules = append(i.options.Route.Rules, options)
		return nil
	})
}

func (s *StartedService) RemoveRouteRule(ruleTag string) error {
	return s.withHotInstance(func(i *Instance) error {
		router, ok := i.instance.Router().(*route.Router)
		if !ok {
			return E.New("router does not support hot rules")
		}
		if err := router.RemoveRule(ruleTag); err != nil {
			return err
		}
		i.options.Route.Rules = common.Filter(i.options.Route.Rules, func(it option.Rule) bool { return it.RuleTag != ruleTag })
		return nil
	})
}
