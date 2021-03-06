// Copyright 2014 CNI authors
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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/containernetworking/cni/pkg/ip"
	"github.com/containernetworking/cni/pkg/ipam"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/utils"
	"github.com/vishvananda/netlink"
)

const defaultBrName = "cni0"

// NetConf is used to hold the config of the network
type NetConf struct {
	types.NetConf
	BrName          string `json:"bridge"`
	BrSubnet        string `json:"bridgeSubnet"`
	BrIP            string `json:"bridgeIP"`
	LogToFile       string `json:"logToFile"`
	IsGW            bool   `json:"isGateway"`
	IsDefaultGW     bool   `json:"isDefaultGateway"`
	IPMasq          bool   `json:"ipMasq"`
	MTU             int    `json:"mtu"`
	LinkMTUOverhead int    `json:"linkMTUOverhead"`
	HairpinMode     bool   `json:"hairpinMode"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
}

func loadNetConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{
		BrName: defaultBrName,
	}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}
	return n, nil
}

func ensureBridgeAddr(br *netlink.Bridge, ipn *net.IPNet) error {
	addrs, err := netlink.AddrList(br, syscall.AF_INET)
	if err != nil && err != syscall.ENOENT {
		return fmt.Errorf("could not get list of IP addresses: %v", err)
	}

	// if there're no addresses on the bridge, it's ok -- we'll add one
	if len(addrs) > 0 {
		ipnStr := ipn.String()
		for _, a := range addrs {
			// string comp is actually easiest for doing IPNet comps
			if a.IPNet.String() == ipnStr {
				return nil
			}
		}
		return fmt.Errorf("%q already has an IP address different from %v", br.Name, ipn.String())
	}

	addr := &netlink.Addr{IPNet: ipn, Label: ""}
	if err := netlink.AddrAdd(br, addr); err != nil {
		return fmt.Errorf("could not add IP address to %q: %v", br.Name, err)
	}
	return nil
}

func bridgeByName(name string) (*netlink.Bridge, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("could not lookup %q: %v", name, err)
	}
	br, ok := l.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%q already exists but is not a bridge", name)
	}
	return br, nil
}

func ensureBridge(brName string, mtu int) (*netlink.Bridge, error) {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: brName,
			MTU:  mtu,
			// Let kernel use default txqueuelen; leaving it unset
			// means 0, and a zero-length TX queue messes up FIFO
			// traffic shapers which use TX queue length as the
			// default packet limit
			TxQLen: -1,
		},
	}

	if err := netlink.LinkAdd(br); err != nil {
		if err != syscall.EEXIST {
			return nil, fmt.Errorf("could not add %q: %v", brName, err)
		}

		// it's ok if the device already exists as long as config is similar
		br, err = bridgeByName(brName)
		if err != nil {
			return nil, err
		}
	}

	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}

	return br, nil
}

func setupVeth(netns ns.NetNS, br *netlink.Bridge, ifName string, mtu int, hairpinMode bool) error {
	var hostVethName string

	err := netns.Do(func(hostNS ns.NetNS) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, _, err := ip.SetupVeth(ifName, mtu, hostNS)
		if err != nil {
			return err
		}

		hostVethName = hostVeth.Attrs().Name
		return nil
	})
	if err != nil {
		return err
	}

	// need to lookup hostVeth again as its index has changed during ns move
	hostVeth, err := netlink.LinkByName(hostVethName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", hostVethName, err)
	}

	// connect host veth end to the bridge
	if err = netlink.LinkSetMaster(hostVeth, br); err != nil {
		return fmt.Errorf("failed to connect %q to bridge %v: %v", hostVethName, br.Attrs().Name, err)
	}

	// set hairpin mode
	if err = netlink.LinkSetHairpin(hostVeth, hairpinMode); err != nil {
		return fmt.Errorf("failed to setup hairpin mode for %v: %v", hostVethName, err)
	}

	return nil
}

func calcGatewayIP(ipn *net.IPNet) net.IP {
	nid := ipn.IP.Mask(ipn.Mask)
	return ip.NextIP(nid)
}

func calculateBridgeIP(n *NetConf) (*net.IPNet, error) {
	var (
		ip          net.IP
		bridgeIPNet *net.IPNet
		err         error
	)

	if n.BrSubnet == "" {
		return nil, fmt.Errorf("mandatory bridgeSubnet not specified in config")
	}

	_, brNetworkIPNet, err := net.ParseCIDR(n.BrSubnet)
	if err != nil {
		return nil, fmt.Errorf("Invalid bridgeSubnet specified got error: %v", err)
	}

	if n.BrIP != "" {
		ip = net.ParseIP(n.BrIP)
		if ip == nil {
			// Check if we can parse as a CIDR
			ip, _, err = net.ParseCIDR(n.BrIP)
			if err != nil {
				return nil, fmt.Errorf("invalid bridgeIP specified in config")
			}
		}

		if !brNetworkIPNet.Contains(ip) {
			return nil, fmt.Errorf("bridgeIP is not in bridgeSubnet")
		}
		bridgeIPNet = &net.IPNet{IP: ip, Mask: brNetworkIPNet.Mask}
	} else {
		// Use the first IP of the subnet for the bridge
		brNetworkIPTo4 := brNetworkIPNet.IP.To4()

		ip = net.IPv4(
			brNetworkIPTo4[0],
			brNetworkIPTo4[1],
			brNetworkIPTo4[2],
			brNetworkIPTo4[3]+1,
		)
		bridgeIPNet = &net.IPNet{IP: ip, Mask: brNetworkIPNet.Mask}
	}

	return bridgeIPNet, nil
}

func setBridgeIP(n *NetConf) error {

	if n.BrSubnet == "" {
		return fmt.Errorf("mandatory bridgeSubnet not specified in config")
	}

	link, err := netlink.LinkByName(n.BrName)
	if err != nil {
		return fmt.Errorf("failed to lookup %q: %v", n.BrName, err)
	}

	bridgeIPNet, err := calculateBridgeIP(n)
	if err != nil {
		return fmt.Errorf("failed to calculate bridge IP: %v", err)
	}

	addrs, err := netlink.AddrList(link, syscall.AF_INET)
	if err != nil && err != syscall.ENOENT {
		return fmt.Errorf("could not get list of IP addresses: %v", err)
	}
	if len(addrs) > 0 {
		bridgeIPStr := bridgeIPNet.String()
		for _, a := range addrs {
			if a.IPNet.String() == bridgeIPStr {
				// Bridge IP already set, nothing to do
				return nil
			}
		}
	}

	addr := &netlink.Addr{IPNet: bridgeIPNet, Label: ""}
	if err = netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("failed to add IP addr to %q: %v", n.BrName, err)
	}

	return nil
}

func setupBridge(n *NetConf) (*netlink.Bridge, error) {
	// create bridge if necessary
	br, err := ensureBridge(n.BrName, n.MTU)
	if err != nil {
		return nil, fmt.Errorf("failed to create bridge %q: %v", n.BrName, err)
	}

	// Set the bridge IP address
	err = setBridgeIP(n)
	if err != nil {
		return nil, fmt.Errorf("failed to set bridge IP: %v", err)
	}

	return br, nil
}

func checkIfContainerInterfaceExists(args *skel.CmdArgs) bool {
	err := ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		_, err := netlink.LinkByName(args.IfName)
		if err != nil {
			return fmt.Errorf("failed to lookup %q: %v", args.IfName, err)
		}
		return nil
	})

	if err == nil {
		return true
	}
	return false
}

func cmdAdd(args *skel.CmdArgs) error {
	n, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}

	if n.LogToFile != "" {
		f, err := os.OpenFile(n.LogToFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err == nil && f != nil {
			logrus.SetLevel(logrus.DebugLevel)
			logrus.SetOutput(f)
			defer f.Close()
		}
	}

	if n.IsDefaultGW {
		n.IsGW = true
	}

	br, err := setupBridge(n)
	if err != nil {
		return err
	}

	netns, err := ns.GetNS(args.Netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", args.Netns, err)
	}
	defer netns.Close()

	linkMTU := n.MTU - n.LinkMTUOverhead
	// If user error, just use the bridge MTU
	if linkMTU < 0 {
		linkMTU = n.MTU
	}

	// Check if the container interface already exists
	if !checkIfContainerInterfaceExists(args) {
		if err = setupVeth(netns, br, args.IfName, linkMTU, n.HairpinMode); err != nil {
			return err
		}
	} else {
		logrus.Infof("container already has interface: %v, no worries", args.IfName)
	}

	// run the IPAM plugin and get back the config to apply
	result, err := ipam.ExecAdd(n.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	// TODO: make this optional when IPv6 is supported
	if result.IP4 == nil {
		return errors.New("IPAM plugin returned missing IPv4 config")
	}

	if result.IP4.Gateway == nil && n.IsGW {
		result.IP4.Gateway = calcGatewayIP(&result.IP4.IP)
	}

	if err := netns.Do(func(_ ns.NetNS) error {
		// set the default gateway if requested
		if n.IsDefaultGW {
			_, defaultNet, err := net.ParseCIDR("0.0.0.0/0")
			if err != nil {
				return err
			}

			for _, route := range result.IP4.Routes {
				if defaultNet.String() == route.Dst.String() {
					if route.GW != nil && !route.GW.Equal(result.IP4.Gateway) {
						return fmt.Errorf(
							"isDefaultGateway ineffective because IPAM sets default route via %q",
							route.GW,
						)
					}
				}
			}

			result.IP4.Routes = append(
				result.IP4.Routes,
				types.Route{Dst: *defaultNet, GW: result.IP4.Gateway},
			)

			// TODO: IPV6
		}

		return ipam.ConfigureIface(args.IfName, result)
	}); err != nil {
		return err
	}

	if n.IsGW {
		gwn := &net.IPNet{
			IP:   result.IP4.Gateway,
			Mask: result.IP4.IP.Mask,
		}

		if err = ensureBridgeAddr(br, gwn); err != nil {
			return err
		}

		if err := ip.EnableIP4Forward(); err != nil {
			return fmt.Errorf("failed to enable forwarding: %v", err)
		}
	}

	if n.IPMasq {
		chain := utils.FormatChainName(n.Name, args.ContainerID)
		comment := utils.FormatComment(n.Name, args.ContainerID)
		if err = ip.SetupIPMasq(ip.Network(&result.IP4.IP), chain, comment); err != nil {
			return err
		}
	}

	result.DNS = n.DNS
	return result.Print()
}

func cmdDel(args *skel.CmdArgs) error {
	n, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}

	if n.LogToFile != "" {
		f, err := os.OpenFile(n.LogToFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err == nil && f != nil {
			logrus.SetLevel(logrus.DebugLevel)
			logrus.SetOutput(f)
			defer f.Close()
		}
	}

	if err := ipam.ExecDel(n.IPAM.Type, args.StdinData); err != nil {
		return err
	}

	if args.Netns == "" {
		return nil
	}

	var ipn *net.IPNet
	err = ns.WithNetNSPath(args.Netns, func(_ ns.NetNS) error {
		var err error
		ipn, err = ip.DelLinkByNameAddr(args.IfName, netlink.FAMILY_V4)
		return err
	})
	if err != nil {
		return err
	}

	if n.IPMasq {
		chain := utils.FormatChainName(n.Name, args.ContainerID)
		comment := utils.FormatComment(n.Name, args.ContainerID)
		if err = ip.TeardownIPMasq(ipn, chain, comment); err != nil {
			return err
		}
	}

	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel)
}
