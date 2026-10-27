package libbox

// These in-process methods are bindable by gomobile. JSON uses sing-box's schema.
// They update the running instance without restarting the core or the TUN.

func (s *CommandServer) AddInbound(inboundJSON string) error {
	return s.StartedService.AddInbound(inboundJSON)
}

func (s *CommandServer) ReplaceInbound(inboundJSON string) error {
	return s.StartedService.ReplaceInbound(inboundJSON)
}

func (s *CommandServer) RemoveInbound(tag string) error {
	return s.StartedService.RemoveInbound(tag)
}

func (s *CommandServer) AddOutbound(outboundJSON string) error {
	return s.StartedService.AddOutbound(outboundJSON)
}

func (s *CommandServer) ReplaceOutbound(outboundJSON string) error {
	return s.StartedService.ReplaceOutbound(outboundJSON)
}

func (s *CommandServer) RemoveOutbound(tag string) error {
	return s.StartedService.RemoveOutbound(tag)
}

func (s *CommandServer) AddRouteRule(ruleJSON string, shouldAppend bool) error {
	return s.StartedService.AddRouteRule(ruleJSON, shouldAppend)
}

func (s *CommandServer) RemoveRouteRule(ruleTag string) error {
	return s.StartedService.RemoveRouteRule(ruleTag)
}

func (s *CommandServer) AddEndpoint(endpointJSON string) error {
	return s.StartedService.AddEndpoint(endpointJSON)
}

func (s *CommandServer) ReplaceEndpoint(endpointJSON string) error {
	return s.StartedService.ReplaceEndpoint(endpointJSON)
}

func (s *CommandServer) RemoveEndpoint(tag string) error {
	return s.StartedService.RemoveEndpoint(tag)
}
