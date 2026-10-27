package route

import (
	"strings"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/option"
	R "github.com/sagernet/sing-box/route/rule"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
)

// AddRule appends a tagged rule, or replaces the rule table when shouldAppend is false.
func (r *Router) AddRule(options option.Rule, shouldAppend bool) error {
	if strings.TrimSpace(options.RuleTag) == "" {
		return E.New("missing ruleTag")
	}
	r.rulesAccess.Lock()
	defer r.rulesAccess.Unlock()
	if !r.started {
		return E.New("router is not started")
	}
	if _, exists := r.ruleByTag[options.RuleTag]; exists && shouldAppend {
		return E.New("existing rule tag found: ", options.RuleTag)
	}
	if err := R.ValidateNoNestedRuleActions(options); err != nil {
		return err
	}
	// shortcut: unused rule sets are freed after startup; support reloading them before allowing hot rule_set references.
	if hasRule([]option.Rule{options}, func(rule option.DefaultRule) bool { return len(rule.RuleSet) > 0 }) {
		return E.New("hot rule_set references require a service reload")
	}
	if (!r.needFindProcess && hasRule([]option.Rule{options}, isProcessRule)) ||
		(!r.needFindNeighbor && hasRule([]option.Rule{options}, isNeighborRule)) ||
		(!r.network.NeedWIFIState() && hasRule([]option.Rule{options}, isWIFIRule)) {
		return E.New("rule requires metadata monitoring not enabled at startup")
	}
	rule, err := R.NewRule(r.ctx, r.logger, options, false)
	if err != nil {
		return err
	}
	var outboundTag string
	switch action := rule.Action().(type) {
	case *R.RuleActionRoute:
		outboundTag = action.Outbound
	case *R.RuleActionBypass:
		outboundTag = action.Outbound
	}
	if outboundTag != "" {
		if _, exists := r.outbound.Outbound(outboundTag); !exists {
			_ = rule.Close()
			return E.New("outbound not found: ", outboundTag)
		}
	}
	if err = rule.Start(); err != nil {
		_ = rule.Close()
		return err
	}
	if !shouldAppend {
		for _, previous := range r.rules {
			if closeErr := previous.Close(); closeErr != nil {
				r.logger.Warn("close replaced rule: ", closeErr)
			}
		}
		r.rules = nil
		clear(r.ruleByTag)
	}
	r.rules = append(r.rules, rule)
	r.ruleByTag[options.RuleTag] = rule
	return nil
}

// RemoveRule also accepts tags supplied in the startup configuration.
func (r *Router) RemoveRule(ruleTag string) error {
	r.rulesAccess.Lock()
	defer r.rulesAccess.Unlock()
	if !r.started {
		return E.New("router is not started")
	}
	rule, exists := r.ruleByTag[ruleTag]
	if !exists {
		return E.New("rule tag not found: ", ruleTag)
	}
	index := common.Index(r.rules, func(it adapter.Rule) bool { return it == rule })
	r.rules = append(r.rules[:index], r.rules[index+1:]...)
	delete(r.ruleByTag, ruleTag)
	return rule.Close()
}
