// Copyright 2016-2018 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package node

import (
	"fmt"
	"net"

	"github.com/cilium/cilium/pkg/logging/logfields"

	"github.com/vishvananda/netlink"
)

type route struct {
	link   string
	prefix *net.IPNet
	via    net.IP
	source net.IP
}

func (r route) Equal(o route) bool {
	return r.link == o.link &&
		r.via.Equal(o.via) &&
		r.source.Equal(o.source) &&
		(r.prefix == o.prefix ||
			(r.prefix != nil && o.prefix != nil && r.prefix.String() == o.prefix.String()))
}

func (r route) getNetlinkRoute() (netlink.Route, error) {
	route := netlink.Route{Dst: r.prefix, Gw: r.via}

	if r.source != nil {
		route.Src = r.source
	}

	if r.link != "" {
		link, err := netlink.LinkByName(HostDevice)
		if err != nil {
			return netlink.Route{}, err
		}

		route.LinkIndex = link.Attrs().Index
	}

	return route, nil
}

// updateIPRoute updates the IP routing entry for the given node n via the
// network interface that as ownAddr.
func (r route) add() error {
	route, err := r.getNetlinkRoute()
	if err != nil {
		return err
	}

	if err := netlink.RouteReplace(&route); err != nil {
		return err
	}

	log.WithField(logfields.Route, route).Debug("Installed route")

	return nil
}

func (r route) delete() error {
	route, err := r.getNetlinkRoute()
	if err != nil {
		return err
	}

	if err := netlink.RouteDel(&route); err != nil {
		return err
	}

	log.WithField(logfields.Route, route).Debug("Deleted route")

	return nil
}

func (r route) String() string {
	prefix := ""
	if r.prefix != nil {
		prefix = r.prefix.String()
	}

	return fmt.Sprintf("prefix=%s link=%s via=%s source=%s", prefix, r.link, r.via, r.source)
}

// EncapsulationEnabled returns true if any kind of encapsulation is enabled
func (n *Node) EncapsulationEnabled() bool {
	return n != nil && n.Routing.EncapsulationEnabled()
}

func ipFamily(ip net.IP) int {
	if ip.To4() == nil {
		return netlink.FAMILY_V6
	}

	return netlink.FAMILY_V4
}

// findRoute finds a particular route as specified by the filter which points
// to the specified device. The filter route can have the following fields set:
//  - Dst
//  - LinkIndex
//  - Scope
//  - Gw
func findRoute(link netlink.Link, route *netlink.Route) *netlink.Route {
	routes, err := netlink.RouteList(link, ipFamily(route.Dst.IP))
	if err != nil {
		return nil
	}

	for _, r := range routes {
		if r.Dst != nil && route.Dst == nil {
			continue
		}

		if route.Dst != nil && r.Dst == nil {
			continue
		}

		aMaskLen, aMaskBits := r.Dst.Mask.Size()
		bMaskLen, bMaskBits := route.Dst.Mask.Size()
		if r.LinkIndex == route.LinkIndex && r.Scope == route.Scope &&
			aMaskLen == bMaskLen && aMaskBits == bMaskBits &&
			r.Dst.IP.Equal(route.Dst.IP) && r.Gw.Equal(route.Gw) {
			return &r
		}
	}

	return nil
}

// replaceNodeRoute verifies that the L2 route for the router IP which is used
// as nexthop for all node routes is properly installed. If unavailable or
// incorrect, it will be replaced with the proper L2 route.
func replaceNexthopRoute(link netlink.Link, routerNet *net.IPNet) error {
	// This is the L2 route which makes the Cilium router IP available behind
	// the "cilium_host" interface. All other routes will use this router IP
	// as nexthop.
	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       routerNet,
		Scope:     netlink.SCOPE_LINK,
	}

	if findRoute(link, route) == nil {
		scopedLog := log.WithField(logfields.Route, route)

		if err := netlink.RouteReplace(route); err != nil {
			scopedLog.WithError(err).Error("Unable to add L2 nexthop route")
			return fmt.Errorf("unable to add L2 nexthop route")
		}

		scopedLog.Debug("Added L2 nexthop route")
	}

	return nil
}

// replaceNodeRoute verifies whether the specified node CIDR is properly
// covered by a route installed in the host's routing table. If unavailable,
// the route is installed on the host.
func replaceNodeRoute(ip *net.IPNet) {
	if ip == nil {
		return
	}

	link, err := netlink.LinkByName(HostDevice)
	if err != nil {
		log.WithError(err).WithField(logfields.Interface, HostDevice).Error("Unable to lookup interface")
		return
	}

	var routerNet *net.IPNet
	var via, local net.IP
	if ip.IP.To4() != nil {
		via = GetInternalIPv4()
		routerNet = &net.IPNet{IP: via, Mask: net.CIDRMask(32, 32)}
		local = GetInternalIPv4()
	} else {
		via = GetIPv6Router()
		routerNet = &net.IPNet{IP: via, Mask: net.CIDRMask(128, 128)}
		local = GetIPv6()
	}

	if err := replaceNexthopRoute(link, routerNet); err != nil {
		log.WithError(err).Error("Unable to add nexthop route")
	}

	route := netlink.Route{LinkIndex: link.Attrs().Index, Dst: ip, Gw: via, Src: local}
	scopedLog := log.WithField(logfields.Route, route)

	if err := netlink.RouteReplace(&route); err != nil {
		scopedLog.WithError(err).Error("Unable to add node route")
	} else {
		scopedLog.Debug("Replaced node route")
	}
}

func validateDirectL2Route(nodeIP net.IP) error {
	if nodeIP == nil {
		return fmt.Errorf("node IP of address family is not known")
	}

	viaRoutes, err := netlink.RouteGet(nodeIP)
	if err != nil {
		return fmt.Errorf("unable to determine route for node IP %s: %s", nodeIP, err)
	}

	if len(viaRoutes) == 0 {
		return fmt.Errorf("no route exists to node IP %s", nodeIP.String())
	}

	if viaRoutes[0].Gw != nil {
		return fmt.Errorf("node IP %s is routed via another gateway %s", nodeIP, viaRoutes[0].Gw.String())
	}

	return nil
}

func (n *Node) generateRouteForIP(route *route, nodeIP, routerIP, sourceIP net.IP) {
	localNode := GetLocalNode()

	// Only consider automatic direct routes if mode is enabled, the node
	// in question is not the local node and an IP of the node is known for
	// the address family in question
	if localNode.Routing != nil && localNode.Routing.DirectRouting.InstallRoutes && !n.IsLocalNode() {
		err := validateDirectL2Route(nodeIP)
		if err == nil {
			route.via = nodeIP
			return
		}

		log.Warningf("Unable to use direct route to CIDR %s: %s", route.prefix, err)
	}

	if localNode.cluster.usePerNodeRoutes || n.IsLocalNode() {
		// Install per node route via device when configured to do so or if the
		// node is the local node
		route.link = HostDevice
		route.via = routerIP
		route.source = sourceIP
	} else {
		// Fall back to cluster wide route
	}
}

func (n *Node) generateRoute(prefix *net.IPNet) route {
	route := route{prefix: prefix}

	if prefix.IP.To4() != nil {
		n.generateRouteForIP(&route, n.GetIPv4(), GetInternalIPv4(), GetInternalIPv4())
	} else {
		n.generateRouteForIP(&route, n.GetIPv6(), GetIPv6Router(), GetIPv6())
	}

	log.Debugf("Generated route for node %s and prefix %+v: %+v", n.Name, prefix, route)

	return route
}

// syncClusterRouting is called periodically by a controller to synchronize the
// routes
func (cc *clusterConfiguation) syncClusterRouting() error {
	cc.Lock()
	defer cc.Unlock()

	link, err := netlink.LinkByName(HostDevice)
	if err != nil {
		return err
	}

	routerNet4 := &net.IPNet{IP: GetInternalIPv4(), Mask: net.CIDRMask(32, 32)}
	if err := replaceNexthopRoute(link, routerNet4); err != nil {
		return err
	}

	routerNet6 := &net.IPNet{IP: GetIPv6Router(), Mask: net.CIDRMask(128, 128)}
	if err := replaceNexthopRoute(link, routerNet6); err != nil {
		return err
	}

	for _, ns := range cc.nodes {
		ns.synchronizeToDatapath()
	}

	if !cc.usePerNodeRoutes {
		replaceNodeRoute(GetIPv4AllocRange())
		replaceNodeRoute(GetIPv6AllocRange())
	}

	for _, prefix := range cc.auxPrefixes {
		replaceNodeRoute(prefix)
	}

	return nil
}