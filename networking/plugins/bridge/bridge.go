// Copyright 2014 CoreOS, Inc.
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
	"fmt"
	"log"
	"net"
	"os"
	"runtime"
	"syscall"

	"github.com/appc/spec/schema/types"
	"github.com/coreos/rocket/Godeps/_workspace/src/github.com/vishvananda/netlink"

	"github.com/coreos/rocket/networking/ipam"
	"github.com/coreos/rocket/networking/util"
)

const defaultBrName = "rkt0"

type netConf struct {
	util.Net
	BrName string `json:"brName"`
	IsGW   bool   `json:"isGW"`
}

func init() {
	// this ensures that main runs only on main thread (thread group leader).
	// since namespace ops (unshare, setns) are done for a single thread, we
	// must ensure that the goroutine does not jump from OS thread to thread
	runtime.LockOSThread()
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

func ensureBridge(brName string, ipn *net.IPNet) (*netlink.Bridge, error) {
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: brName,
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

	if ipn != nil {
		return br, ensureBridgeAddr(br, ipn)
	}

	return br, nil
}

func setupVeth(contID types.UUID, netns string, br *netlink.Bridge, ipn *net.IPNet, ifName string) error {
	var hostVethName string

	err := util.WithNetNSPath(netns, func(hostNS *os.File) error {
		// create the veth pair in the container and move host end into host netns
		hostVeth, _, err := util.SetupVeth(contID.String(), ifName, ipn, hostNS)
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

	return nil
}

func cmdAdd(contID, netns, netCfg, ifName string) error {
	cid, err := types.NewUUID(contID)
	if err != nil {
		return fmt.Errorf("error parsing ContainerID: %v", err)
	}

	conf := netConf{
		BrName: defaultBrName,
	}
	if err := util.LoadNet(netCfg, &conf); err != nil {
		return fmt.Errorf("failed to load %q: %v", netCfg, err)
	}

	ipn, gw, err := ipam.AllocIP(*cid, netCfg, ifName, "")
	if err != nil {
		return err
	}

	var gwn *net.IPNet
	if conf.IsGW && gw != nil {
		gwn = &net.IPNet{
			IP:   gw,
			Mask: ipn.Mask,
		}
	}

	// create bridge if necessary
	br, err := ensureBridge(conf.BrName, gwn)
	if err != nil {
		return fmt.Errorf("failed to create bridge %q: %v", conf.BrName, err)
	}

	if err = setupVeth(*cid, netns, br, ipn, ifName); err != nil {
		return err
	}

	// print to stdout the assigned IP for rkt
	// TODO(eyakubovich): this will need to be JSON per latest proposal
	if _, err = fmt.Print(ipn.String()); err != nil {
		return err
	}

	return nil
}

func cmdDel(contID, netns, netConf, ifName string) error {
	return util.WithNetNSPath(netns, func(hostNS *os.File) error {
		return util.DelLinkByName(ifName)
	})
}

func main() {
	var err error

	cmd := os.Getenv("RKT_NETPLUGIN_COMMAND")
	contID := os.Getenv("RKT_NETPLUGIN_CONTID")
	netns := os.Getenv("RKT_NETPLUGIN_NETNS")
	ifName := os.Getenv("RKT_NETPLUGIN_IFNAME")
	netConf := os.Getenv("RKT_NETPLUGIN_NETCONF")

	if cmd == "" || contID == "" || netns == "" || ifName == "" || netConf == "" {
		log.Printf("Required env variable missing")
		log.Print("Env: ", os.Environ())
		os.Exit(1)
	}

	switch cmd {
	case "ADD":
		err = cmdAdd(contID, netns, netConf, ifName)

	case "DEL":
		err = cmdDel(contID, netns, netConf, ifName)

	default:
		log.Printf("Unknown RKT_NETPLUGIN_COMMAND: %v", cmd)
		os.Exit(1)
	}

	if err != nil {
		log.Printf("%v: %v", os.Args[1], err)
		os.Exit(1)
	}

}
