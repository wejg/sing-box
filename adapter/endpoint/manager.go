package endpoint

import (
	"context"
	"os"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/taskmonitor"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/service"
)

var _ adapter.EndpointManager = (*Manager)(nil)

type Manager struct {
	logger        log.ContextLogger
	registry      adapter.EndpointRegistry
	access        sync.Mutex
	started       bool
	stage         adapter.StartStage
	endpoints     []adapter.Endpoint
	endpointByTag map[string]adapter.Endpoint
}

func NewManager(logger log.ContextLogger, registry adapter.EndpointRegistry) *Manager {
	return &Manager{
		logger:        logger,
		registry:      registry,
		endpointByTag: make(map[string]adapter.Endpoint),
	}
}

func (m *Manager) Start(stage adapter.StartStage) error {
	m.access.Lock()
	defer m.access.Unlock()
	if m.started && m.stage >= stage {
		panic("already started")
	}
	m.started = true
	m.stage = stage
	if stage == adapter.StartStateStart {
		// started with outbound manager
		return nil
	}
	for _, endpoint := range m.endpoints {
		name := "endpoint/" + endpoint.Type() + "[" + endpoint.Tag() + "]"
		done := adapter.LogElapsed(m.logger, stage, " ", name)
		err := adapter.LegacyStart(endpoint, stage)
		done()
		if err != nil {
			return E.Cause(err, stage, " ", name)
		}
	}
	return nil
}

func (m *Manager) Close() error {
	m.access.Lock()
	defer m.access.Unlock()
	if !m.started {
		return nil
	}
	m.started = false
	endpoints := m.endpoints
	m.endpoints = nil
	monitor := taskmonitor.New(m.logger, C.StopTimeout)
	var err error
	for _, endpoint := range endpoints {
		name := "endpoint/" + endpoint.Type() + "[" + endpoint.Tag() + "]"
		done := adapter.LogElapsed(m.logger, "close ", name)
		monitor.Start("close ", name)
		err = E.Append(err, endpoint.Close(), func(err error) error {
			return E.Cause(err, "close ", name)
		})
		monitor.Finish()
		done()
	}
	return nil
}

func (m *Manager) Endpoints() []adapter.Endpoint {
	m.access.Lock()
	defer m.access.Unlock()
	return append([]adapter.Endpoint(nil), m.endpoints...)
}

func (m *Manager) Get(tag string) (adapter.Endpoint, bool) {
	m.access.Lock()
	defer m.access.Unlock()
	endpoint, found := m.endpointByTag[tag]
	return endpoint, found
}

func (m *Manager) Remove(tag string) error {
	m.access.Lock()
	endpoint, found := m.endpointByTag[tag]
	if !found {
		m.access.Unlock()
		return os.ErrInvalid
	}
	delete(m.endpointByTag, tag)
	index := common.Index(m.endpoints, func(it adapter.Endpoint) bool {
		return it == endpoint
	})
	if index == -1 {
		panic("invalid endpoint index")
	}
	m.endpoints = append(m.endpoints[:index], m.endpoints[index+1:]...)
	started := m.started
	m.access.Unlock()
	if started {
		if err := endpoint.Close(); err != nil {
			m.logger.Error(E.Cause(err, "close removed endpoint/", endpoint.Type(), "[", endpoint.Tag(), "]"))
		}
	}
	return nil
}

func (m *Manager) Create(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, outboundType string, options any) error {
	endpoint, err := m.registry.Create(ctx, router, logger, tag, outboundType, options)
	if err != nil {
		return err
	}
	m.access.Lock()
	started := m.started
	m.access.Unlock()
	if started {
		manager := service.FromContext[adapter.OutboundManager](ctx)
		for _, dependency := range endpoint.Dependencies() {
			if _, exists := manager.Outbound(dependency); dependency == tag || !exists {
				_ = endpoint.Close()
				return E.New("invalid dependency[", dependency, "] for endpoint[", tag, "]")
			}
		}
		name := "endpoint/" + endpoint.Type() + "[" + endpoint.Tag() + "]"
		for _, stage := range adapter.ListStartStages {
			done := adapter.LogElapsed(m.logger, stage, " ", name)
			err = adapter.LegacyStart(endpoint, stage)
			done()
			if err != nil {
				_ = endpoint.Close()
				return E.Cause(err, stage, " ", name)
			}
		}
	}
	m.access.Lock()
	previous := m.endpointByTag[tag]
	if existsEndpoint := previous; existsEndpoint != nil {
		existsIndex := common.Index(m.endpoints, func(it adapter.Endpoint) bool {
			return it == existsEndpoint
		})
		if existsIndex == -1 {
			panic("invalid endpoint index")
		}
		m.endpoints = append(m.endpoints[:existsIndex], m.endpoints[existsIndex+1:]...)
	}
	m.endpoints = append(m.endpoints, endpoint)
	m.endpointByTag[tag] = endpoint
	m.access.Unlock()
	if started && previous != nil {
		if err := previous.Close(); err != nil {
			m.logger.Error(E.Cause(err, "close replaced endpoint/", previous.Type(), "[", previous.Tag(), "]"))
		}
	}
	return nil
}
