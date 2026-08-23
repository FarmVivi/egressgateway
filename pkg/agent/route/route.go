// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package route

import (
	"net"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"

	"github.com/spidernet-io/egressgateway/pkg/markallocator"
)

// NewRuleRoute creates a new RuleRoute with the provided options.
func NewRuleRoute(options ...Option) *RuleRoute {
	// enableIPv4/enableIPv6 default to true so that callers which do not set
	// them keep the previous behaviour of handling both families.
	r := &RuleRoute{priority: 99, enableIPv4: true, enableIPv6: true}
	for _, o := range options {
		o(r)
	}
	return r
}

type Option func(*RuleRoute)

// WithPriority sets the priority of the RuleRoute.
func WithPriority(priority int) Option {
	return func(r *RuleRoute) {
		r.priority = priority
	}
}

// WithLogger sets the logger of the RuleRoute.
func WithLogger(logger logr.Logger) Option {
	return func(r *RuleRoute) {
		r.log = logger
	}
}

// WithIPv4 enables or disables handling of the IPv4 family.
func WithIPv4(enable bool) Option {
	return func(r *RuleRoute) {
		r.enableIPv4 = enable
	}
}

// WithIPv6 enables or disables handling of the IPv6 family. It must be set to
// false when the cluster runs without IPv6: on a host booted with
// `ipv6.disable=1` the kernel refuses every AF_INET6 netlink request with
// EAFNOSUPPORT ("address family not supported by protocol"), which would make
// PurgeStaleRules fail on every reconcile loop.
func WithIPv6(enable bool) Option {
	return func(r *RuleRoute) {
		r.enableIPv6 = enable
	}
}

type RuleRoute struct {
	log        logr.Logger
	priority   int
	enableIPv4 bool
	enableIPv6 bool
}

func (r *RuleRoute) PurgeStaleRules(marks map[int]struct{}, baseMark string) error {
	start, end, err := markallocator.RangeSize(baseMark)
	if err != nil {
		return err
	}

	clean := func(rules []netlink.Rule, family int) error {
		for _, rule := range rules {
			rule.Family = family

			if _, ok := marks[rule.Mark]; !ok {
				if int(start) <= rule.Mark && int(end) >= rule.Mark {
					err := netlink.RuleDel(&rule)
					if err != nil {
						return err
					}
				}
			}
		}
		return nil
	}

	// Both families are guarded: querying a family that the kernel does not
	// support returns EAFNOSUPPORT, which would abort the whole purge. Every
	// other IP-family-dependent code path in the agent is already guarded the
	// same way against EnableIPv4/EnableIPv6.
	if r.enableIPv4 {
		rules, err := netlink.RuleListFiltered(netlink.FAMILY_V4, nil, netlink.RT_FILTER_MARK)
		if err != nil {
			return err
		}
		if err := clean(rules, netlink.FAMILY_V4); err != nil {
			return err
		}
	}

	if r.enableIPv6 {
		rules, err := netlink.RuleListFiltered(netlink.FAMILY_V6, nil, netlink.RT_FILTER_MARK)
		if err != nil {
			return err
		}
		if err := clean(rules, netlink.FAMILY_V6); err != nil {
			return err
		}
	}

	return nil
}

func (r *RuleRoute) Ensure(linkName string, ipv4, ipv6 *net.IP, table int, mark int) error {
	if mark == 0 {
		return nil
	}

	log := r.log.WithValues("linkName", linkName, "table", table, "mark", mark)

	if ipv4 != nil {
		err := r.EnsureRule(netlink.FAMILY_V4, table, mark, log)
		if err != nil {
			return err
		}
	}

	if ipv6 != nil {
		err := r.EnsureRule(netlink.FAMILY_V6, table, mark, log)
		if err != nil {
			return err
		}
	}

	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return err
	}

	log.V(1).Info("get link")

	if ipv4 != nil {
		err = r.EnsureRoute(link, ipv4, netlink.FAMILY_V4, table, log)
		if err != nil {
			return err
		}
	}
	if ipv6 != nil {
		err = r.EnsureRoute(link, ipv6, netlink.FAMILY_V6, table, log)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *RuleRoute) EnsureRoute(link netlink.Link, ip *net.IP, family int, table int, log logr.Logger) error {
	log = log.WithValues("family", family, "ip", ip)
	log.V(1).Info("ensure route")

	routeFilter := &netlink.Route{Table: table}
	routes, err := netlink.RouteListFiltered(family, routeFilter, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}

	var find bool
	for _, route := range routes {
		if route.Table == table {
			if ip == nil || route.Gw.String() != ip.String() {
				log.Info("delete route", "route", route.String())
				err := netlink.RouteDel(&route)
				if err != nil {
					return err
				}
				continue
			}
			find = true
		}
	}

	if ip == nil {
		return nil
	}

	if !find {
		index := link.Attrs().Index
		err = netlink.RouteAdd(&netlink.Route{LinkIndex: index, Gw: *ip, Table: table})
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *RuleRoute) EnsureRule(family int, table int, mark int, log logr.Logger) error {
	log = log.WithValues("family", family)
	log.V(1).Info("ensure rule")

	t := netlink.NewRule()
	t.Mark = mark
	t.Family = family
	rules, err := netlink.RuleListFiltered(family, t, netlink.RT_FILTER_MARK)
	if err != nil {
		return err
	}
	r.log.V(1).Info("list rule", "count", len(rules))

	found := false
	for _, rule := range rules {
		del := false
		if rule.Table != table {
			del = true
		}
		if rule.Priority != r.priority {
			del = true
		}
		if found {
			del = true
		}
		if del {
			r.log.V(1).Info("delete rule", "rule", rule.String())
			rule.Family = family
			err = netlink.RuleDel(&rule)
			if err != nil {
				return err
			}
			continue
		}
		found = true
	}
	if found {
		return nil
	}

	// not found
	r.log.V(1).Info("rule not match, try add it")
	rule := netlink.NewRule()
	rule.Table = table
	rule.Mark = mark
	rule.Family = family
	rule.Priority = r.priority

	r.log.V(1).Info("add rule", "rule", rule.String())
	err = netlink.RuleAdd(rule)
	if err != nil {
		return err
	}
	return nil
}
