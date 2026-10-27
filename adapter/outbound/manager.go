package outbound

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/taskmonitor"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
)

var _ adapter.OutboundManager = (*Manager)(nil)

type Manager struct {
	logger                  log.ContextLogger
	registry                adapter.OutboundRegistry
	endpoint                adapter.EndpointManager
	defaultTag              string
	access                  sync.RWMutex
	started                 bool
	stage                   adapter.StartStage
	outbounds               []adapter.Outbound
	outboundByTag           map[string]adapter.Outbound
	dependByTag             map[string][]string
	defaultOutbound         adapter.Outbound
	defaultOutboundFallback func() (adapter.Outbound, error)
}

func NewManager(logger logger.ContextLogger, registry adapter.OutboundRegistry, endpoint adapter.EndpointManager, defaultTag string) *Manager {
	return &Manager{
		logger:        logger,
		registry:      registry,
		endpoint:      endpoint,
		defaultTag:    defaultTag,
		outboundByTag: make(map[string]adapter.Outbound),
		dependByTag:   make(map[string][]string),
	}
}

func (m *Manager) Initialize(defaultOutboundFallback func() (adapter.Outbound, error)) {
	m.defaultOutboundFallback = defaultOutboundFallback
}

func (m *Manager) Start(stage adapter.StartStage) error {
	m.access.Lock()
	if m.started && m.stage >= stage {
		panic("already started")
	}
	m.started = true
	m.stage = stage
	if stage == adapter.StartStateInitialize {
		if m.defaultTag != "" && m.defaultOutbound == nil {
			defaultEndpoint, loaded := m.endpoint.Get(m.defaultTag)
			if !loaded {
				m.access.Unlock()
				return E.New("default outbound not found: ", m.defaultTag)
			}
			m.defaultOutbound = defaultEndpoint
		}
		if m.defaultOutbound == nil {
			directOutbound, err := m.defaultOutboundFallback()
			if err != nil {
				m.access.Unlock()
				return E.Cause(err, "create direct outbound for fallback")
			}
			m.outbounds = append(m.outbounds, directOutbound)
			m.outboundByTag[directOutbound.Tag()] = directOutbound
			m.defaultOutbound = directOutbound
		}
	}
	outbounds := m.outbounds
	m.access.Unlock()
	if stage == adapter.StartStateStart {
		return m.startOutbounds(append(outbounds, common.Map(m.endpoint.Endpoints(), func(it adapter.Endpoint) adapter.Outbound { return it })...))
	}
	for _, outbound := range outbounds {
		name := "outbound/" + outbound.Type() + "[" + outbound.Tag() + "]"
		done := adapter.LogElapsed(m.logger, stage, " ", name)
		err := adapter.LegacyStart(outbound, stage)
		done()
		if err != nil {
			return E.Cause(err, stage, " ", name)
		}
	}
	return nil
}

func (m *Manager) startOutbounds(outbounds []adapter.Outbound) error {
	monitor := taskmonitor.New(m.logger, C.StartTimeout)
	started := make(map[string]bool)
	for {
		canContinue := false
	startOne:
		for _, outboundToStart := range outbounds {
			outboundTag := outboundToStart.Tag()
			if started[outboundTag] {
				continue
			}
			dependencies := outboundToStart.Dependencies()
			for _, dependency := range dependencies {
				if !started[dependency] {
					continue startOne
				}
			}
			started[outboundTag] = true
			canContinue = true
			name := "outbound/" + outboundToStart.Type() + "[" + outboundTag + "]"
			if starter, isStarter := outboundToStart.(adapter.Lifecycle); isStarter {
				done := adapter.LogElapsed(m.logger, "start ", name)
				monitor.Start("start ", name)
				err := starter.Start(adapter.StartStateStart)
				monitor.Finish()
				done()
				if err != nil {
					return E.Cause(err, "start ", name)
				}
			} else if starter, isStarter := outboundToStart.(interface {
				Start() error
			}); isStarter {
				done := adapter.LogElapsed(m.logger, "start ", name)
				monitor.Start("start ", name)
				err := starter.Start()
				monitor.Finish()
				done()
				if err != nil {
					return E.Cause(err, "start ", name)
				}
			}
		}
		if len(started) == len(outbounds) {
			break
		}
		if canContinue {
			continue
		}
		currentOutbound := common.Find(outbounds, func(it adapter.Outbound) bool {
			return !started[it.Tag()]
		})
		var lintOutbound func(oTree []string, oCurrent adapter.Outbound) error
		lintOutbound = func(oTree []string, oCurrent adapter.Outbound) error {
			problemOutboundTag := common.Find(oCurrent.Dependencies(), func(it string) bool {
				return !started[it]
			})
			if common.Contains(oTree, problemOutboundTag) {
				return E.New("circular outbound dependency: ", strings.Join(oTree, " -> "), " -> ", problemOutboundTag)
			}
			problemOutbound := common.Find(outbounds, func(it adapter.Outbound) bool {
				return it.Tag() == problemOutboundTag
			})
			if problemOutbound == nil {
				return E.New("dependency[", problemOutboundTag, "] not found for outbound[", oCurrent.Tag(), "]")
			}
			return lintOutbound(append(oTree, problemOutboundTag), problemOutbound)
		}
		return lintOutbound([]string{currentOutbound.Tag()}, currentOutbound)
	}
	return nil
}

func (m *Manager) Close() error {
	monitor := taskmonitor.New(m.logger, C.StopTimeout)
	m.access.Lock()
	if !m.started {
		m.access.Unlock()
		return nil
	}
	m.started = false
	outbounds := m.outbounds
	m.outbounds = nil
	m.access.Unlock()
	var err error
	for _, outbound := range outbounds {
		if closer, isCloser := outbound.(io.Closer); isCloser {
			name := "outbound/" + outbound.Type() + "[" + outbound.Tag() + "]"
			done := adapter.LogElapsed(m.logger, "close ", name)
			monitor.Start("close ", name)
			err = E.Append(err, closer.Close(), func(err error) error {
				return E.Cause(err, "close ", name)
			})
			monitor.Finish()
			done()
		}
	}
	return nil
}

func (m *Manager) Outbounds() []adapter.Outbound {
	m.access.RLock()
	defer m.access.RUnlock()
	return append([]adapter.Outbound(nil), m.outbounds...)
}

func (m *Manager) Outbound(tag string) (adapter.Outbound, bool) {
	m.access.RLock()
	outbound, found := m.outboundByTag[tag]
	m.access.RUnlock()
	if found {
		return outbound, true
	}
	return m.endpoint.Get(tag)
}

func (m *Manager) Default() adapter.Outbound {
	m.access.RLock()
	current := m.defaultOutbound
	m.access.RUnlock()
	if current != nil {
		// Endpoints can be replaced by their own manager, so resolve the current object by tag.
		if outbound, exists := m.Outbound(current.Tag()); exists {
			return outbound
		}
	}
	return current
}

func (m *Manager) Remove(tag string) error {
	m.access.Lock()
	defer m.access.Unlock()
	outbound, found := m.outboundByTag[tag]
	if !found {
		return os.ErrInvalid
	}
	if dependBy := m.dependByTag[tag]; len(dependBy) > 0 {
		return E.New("outbound[", tag, "] is depended by ", strings.Join(dependBy, ", "))
	}
	delete(m.outboundByTag, tag)
	index := common.Index(m.outbounds, func(it adapter.Outbound) bool {
		return it == outbound
	})
	if index == -1 {
		panic("invalid inbound index")
	}
	m.outbounds = append(m.outbounds[:index], m.outbounds[index+1:]...)
	started := m.started
	if m.defaultOutbound == outbound {
		if endpoint, exists := m.endpoint.Get(tag); exists {
			m.defaultOutbound = endpoint
		} else if len(m.outbounds) > 0 {
			m.defaultOutbound = m.outbounds[0]
			m.logger.Info("updated default outbound to ", m.defaultOutbound.Tag())
		} else {
			m.defaultOutbound = nil
		}
	}
	dependencies := outbound.Dependencies()
	for _, dependency := range dependencies {
		if len(m.dependByTag[dependency]) == 1 {
			delete(m.dependByTag, dependency)
		} else {
			m.dependByTag[dependency] = common.Filter(m.dependByTag[dependency], func(it string) bool {
				return it != tag
			})
		}
	}
	if started {
		if err := common.Close(outbound); err != nil {
			m.logger.Error(E.Cause(err, "close removed outbound/", outbound.Type(), "[", outbound.Tag(), "]"))
		}
	}
	return nil
}

func (m *Manager) Create(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, inboundType string, options any) error {
	if tag == "" {
		return os.ErrInvalid
	}
	outbound, err := m.registry.CreateOutbound(ctx, router, logger, tag, inboundType, options)
	if err != nil {
		return err
	}
	if m.started {
		for _, dependency := range outbound.Dependencies() {
			if _, exists := m.Outbound(dependency); dependency == tag || !exists {
				_ = common.Close(outbound)
				return E.New("invalid dependency[", dependency, "] for outbound[", tag, "]")
			}
		}
		name := "outbound/" + outbound.Type() + "[" + outbound.Tag() + "]"
		for _, stage := range adapter.ListStartStages {
			done := adapter.LogElapsed(m.logger, stage, " ", name)
			err = adapter.LegacyStart(outbound, stage)
			done()
			if err != nil {
				_ = common.Close(outbound)
				return E.Cause(err, stage, " ", name)
			}
		}
	}
	m.access.Lock()
	previous := m.outboundByTag[tag]
	if existsOutbound := previous; existsOutbound != nil {
		existsIndex := common.Index(m.outbounds, func(it adapter.Outbound) bool {
			return it == existsOutbound
		})
		if existsIndex == -1 {
			panic("invalid inbound index")
		}
		m.outbounds = append(m.outbounds[:existsIndex], m.outbounds[existsIndex+1:]...)
		for _, dependency := range existsOutbound.Dependencies() {
			m.dependByTag[dependency] = common.Filter(m.dependByTag[dependency], func(it string) bool { return it != tag })
		}
		if m.defaultOutbound == existsOutbound {
			m.defaultOutbound = outbound
		}
	}
	m.outbounds = append(m.outbounds, outbound)
	m.outboundByTag[tag] = outbound
	dependencies := outbound.Dependencies()
	for _, dependency := range dependencies {
		m.dependByTag[dependency] = append(m.dependByTag[dependency], tag)
	}
	if tag == m.defaultTag || (m.defaultOutbound != nil && m.defaultOutbound.Tag() == tag) || (m.defaultTag == "" && m.defaultOutbound == nil) {
		m.defaultOutbound = outbound
		if m.started {
			m.logger.Info("updated default outbound to ", outbound.Tag())
		}
	}
	started := m.started
	m.access.Unlock()
	if started && previous != nil {
		if err := common.Close(previous); err != nil {
			m.logger.Error(E.Cause(err, "close replaced outbound/", previous.Type(), "[", previous.Tag(), "]"))
		}
	}
	return nil
}
