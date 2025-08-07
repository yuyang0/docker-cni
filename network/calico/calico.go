package calico

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/projecteru2/docker-cni/store"
	log "github.com/sirupsen/logrus"
)

type CalicoNetwork struct{}

func New() *CalicoNetwork {
	return &CalicoNetwork{}
}

func (_ *CalicoNetwork) SimulateCNIAdd(info *store.InterfaceInfo, state *specs.State) (err error) {
	var hasIPv4, hasIPv6 bool
	hostVethName := info.HostIFName
	contVethName := info.IFName
	containerPid := state.Pid // <- change this to your container's PID

	netnsPath := fmt.Sprintf("/proc/%d/ns/net", containerPid)

	ns.WithNetNSPath(netnsPath, func(hostNS ns.NetNS) error {
		// Create veth pair
		veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{
				Name:  contVethName,
				Flags: net.FlagUp,
				// MTU:   1500,
			},
			PeerName: hostVethName,
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return errors.Wrapf(err, "failed to create veth pair %s", hostVethName)
		}
		hostVeth, err := netlink.LinkByName(hostVethName)
		if err != nil {
			return errors.Wrapf(err, "failed to lookup %q", hostVethName)
		}

		if mac, err := net.ParseMAC("EE:EE:EE:EE:EE:EE"); err != nil {
			log.Infof("failed to parse MAC Address: %v. Using kernel generated MAC.", err)
		} else {
			// Set the MAC address on the host side interface so the kernel does not
			// have to generate a persistent address which fails some times.
			if err = netlink.LinkSetHardwareAddr(hostVeth, mac); err != nil {
				log.Warnf("failed to Set MAC of %q: %v. Using kernel generated MAC.", hostVethName, err)
			}
		}
		// Explicitly set the veth to UP state, because netlink doesn't always do that on all the platforms with net.FlagUp.
		// veth won't get a link local address unless it's set to UP state.
		if err = netlink.LinkSetUp(hostVeth); err != nil {
			return fmt.Errorf("failed to set %q up: %v", hostVethName, err)
		}

		contVeth, err := netlink.LinkByName(contVethName)
		if err != nil {
			err = fmt.Errorf("failed to lookup %q: %v", contVethName, err)
			return err
		}

		if mac, err := net.ParseMAC(info.MAC); err != nil {
			log.Infof("failed to parse MAC Address: %v. Using kernel generated MAC.", err)
		} else {
			// Set the MAC address on the host side interface so the kernel does not
			// have to generate a persistent address which fails some times.
			if err = netlink.LinkSetHardwareAddr(contVeth, mac); err != nil {
				log.Warnf("failed to Set MAC of %q: %v. Using kernel generated MAC.", hostVethName, err)
			}
		}
		// Fetch the MAC from the container Veth. This is needed by Calico.
		contVethMAC := contVeth.Attrs().HardwareAddr.String()
		log.WithField("MAC", contVethMAC).Debug("Found MAC for container veth")
		hasIPv4, hasIPv6, err = configureInterface(contVeth, hostVeth, info)
		if err != nil {
			log.Errorf("failed to configure interface %s: %v", contVethName, err)
		}
		// move host veth to host netns
		if err = netlink.LinkSetNsFd(hostVeth, int(hostNS.Fd())); err != nil {
			return fmt.Errorf("failed to move veth to host netns: %v", err)
		}
		return nil
	})

	if err = configureSysctls(hostVethName, hasIPv4, hasIPv6); err != nil {
		return errors.Wrapf(err, "failed to configure sysctls for host veth %s", hostVethName)
	}

	// Moving a veth between namespaces always leaves it in the "DOWN" state. Set it back to "UP" now that we're
	// back in the host namespace.
	hostVeth, err := netlink.LinkByName(hostVethName)
	if err != nil {
		return errors.Wrapf(err, "failed to lookup %q", hostVethName)
	}

	if err = netlink.LinkSetUp(hostVeth); err != nil {
		return errors.Wrapf(err, "failed to set %q up", hostVethName)
	}
	// Now that the host side of the veth is moved, state set to UP, and configured with sysctls, we can add the routes to it in the host namespace.
	err = SetupRoutes(hostVeth, info.IPs)
	if err != nil {
		return fmt.Errorf("error adding host side routes for interface: %s, error: %s", hostVeth.Attrs().Name, err)
	}

	return nil
}

func parseCIDR(ipStr string) (*net.IPNet, error) {
	if strings.Contains(ipStr, "/") {
		_, ipnet, err := net.ParseCIDR(ipStr)
		return ipnet, err
	}
	// Assume /32 or /128 if not provided
	ip := net.ParseIP(ipStr)
	if ip.To4() != nil {
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}, nil
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
}

func configureInterface(contVeth, hostVeth netlink.Link, info *store.InterfaceInfo) (hasIPv4 bool, hasIPv6 bool, err error) {
	// At this point, the virtual ethernet pair has been created, and both ends have the right names.
	// Both ends of the veth are still in the container's network namespace.

	for _, ipStr := range info.IPs {
		ipnet, err := parseCIDR(ipStr)
		if err != nil {
			return hasIPv4, hasIPv6, fmt.Errorf("invalid ip: %s: %w", ipStr, err)
		}
		version := "6"
		if ipnet.IP.To4() != nil {
			version = "4"
		}
		// Before returning, create the routes inside the namespace, first for IPv4 then IPv6.
		if version == "4" {
			// Add a connected route to a dummy next hop so that a default route can be set
			gw := net.IPv4(169, 254, 1, 1)
			gwNet := &net.IPNet{IP: gw, Mask: net.CIDRMask(32, 32)}
			err := netlink.RouteAdd(
				&netlink.Route{
					LinkIndex: contVeth.Attrs().Index,
					Scope:     netlink.SCOPE_LINK,
					Dst:       gwNet,
				},
			)

			if err != nil {
				return hasIPv4, hasIPv6, fmt.Errorf("failed to add route inside the container: %v", err)
			}

			if err = ip.AddDefaultRoute(gw, contVeth); err != nil {
				return hasIPv4, hasIPv6, fmt.Errorf("failed to add the default route inside the container: %v", err)
			}

			if err = netlink.AddrAdd(contVeth, &netlink.Addr{IPNet: ipnet}); err != nil {
				return hasIPv4, hasIPv6, fmt.Errorf("failed to add IP addr to container interface: %v", err)
			}
			// Set hasIPv4 to true so sysctls for IPv4 can be programmed when the host side of
			// the veth finishes moving to the host namespace.
			hasIPv4 = true
		}

		// Handle IPv6 routes
		if version == "6" {
			// Make sure ipv6 is enabled in the container/pod network namespace.
			// Without these sysctls enabled, interfaces will come up but they won't get a link local IPv6 address
			// which is required to add the default IPv6 route.
			if err = writeProcSys("/proc/sys/net/ipv6/conf/all/disable_ipv6", "0"); err != nil {
				return hasIPv4, hasIPv6, fmt.Errorf("failed to set net.ipv6.conf.all.disable_ipv6=0: %s", err)
			}

			if err = writeProcSys("/proc/sys/net/ipv6/conf/default/disable_ipv6", "0"); err != nil {
				return hasIPv4, hasIPv6, fmt.Errorf("failed to set net.ipv6.conf.default.disable_ipv6=0: %s", err)
			}

			if err = writeProcSys("/proc/sys/net/ipv6/conf/lo/disable_ipv6", "0"); err != nil {
				return hasIPv4, hasIPv6, fmt.Errorf("failed to set net.ipv6.conf.lo.disable_ipv6=0: %s", err)
			}

			// No need to add a dummy next hop route as the host veth device will already have an IPv6
			// link local address that can be used as a next hop.
			// Just fetch the address of the host end of the veth and use it as the next hop.
			addresses, err := netlink.AddrList(hostVeth, netlink.FAMILY_V6)
			if err != nil {
				log.Errorf("Error listing IPv6 addresses for the host side of the veth pair: %s", err)
				return hasIPv4, hasIPv6, err
			}

			if len(addresses) < 1 {
				// If the hostVeth doesn't have an IPv6 address then this host probably doesn't
				// support IPv6. Since a IPv6 address has been allocated that can't be used,
				// return an error.
				return hasIPv4, hasIPv6, fmt.Errorf("failed to get IPv6 addresses for host side of the veth pair")
			}

			hostIPv6Addr := addresses[0].IP

			_, defNet, _ := net.ParseCIDR("::/0")
			if err = ip.AddRoute(defNet, hostIPv6Addr, contVeth); err != nil {
				return hasIPv4, hasIPv6, fmt.Errorf("failed to add IPv6 default gateway to %v %v", hostIPv6Addr, err)
			}

			if err = netlink.AddrAdd(contVeth, &netlink.Addr{IPNet: ipnet}); err != nil {
				return hasIPv4, hasIPv6, fmt.Errorf("failed to add IPv6 addr to %q: %v", contVeth, err)
			}

			// Set hasIPv6 to true so sysctls for IPv6 can be programmed when the host side of
			// the veth finishes moving to the host namespace.
			hasIPv6 = true
		}
	}
	return hasIPv4, hasIPv6, nil
}

// configureSysctls configures necessary sysctls required for the host side of the veth pair for IPv4 and/or IPv6.
func configureSysctls(hostVethName string, hasIPv4, hasIPv6 bool) error {
	var err error

	if hasIPv4 {
		// Enable proxy ARP, this makes the host respond to all ARP requests with its own
		// MAC. We install explicit routes into the containers network
		// namespace and we use a link-local address for the gateway.  Turing on proxy ARP
		// means that we don't need to assign the link local address explicitly to each
		// host side of the veth, which is one fewer thing to maintain and one fewer
		// thing we may clash over.
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", hostVethName), "1"); err != nil {
			return fmt.Errorf("failed to set net.ipv4.conf.%s.proxy_arp=1: %s", hostVethName, err)
		}

		// Normally, the kernel has a delay before responding to proxy ARP but we know
		// that's not needed in a Calico network so we disable it.
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/neigh/%s/proxy_delay", hostVethName), "0"); err != nil {
			return fmt.Errorf("failed to set net.ipv4.neigh.%s.proxy_delay=0: %s", hostVethName, err)
		}

		// Enable IP forwarding of packets coming _from_ this interface.  For packets to
		// be forwarded in both directions we need this flag to be set on the fabric-facing
		// interface too (or for the global default to be set).
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/forwarding", hostVethName), "1"); err != nil {
			return fmt.Errorf("failed to set net.ipv4.conf.%s.forwarding=1: %s", hostVethName, err)
		}
	}

	if hasIPv6 {
		// Make sure ipv6 is enabled on the hostVeth interface in the host network namespace.
		// Interfaces won't get a link local address without this sysctl set to 0.
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", hostVethName), "0"); err != nil {
			return fmt.Errorf("failed to set net.ipv6.conf.%s.disable_ipv6=0: %s", hostVethName, err)
		}

		// Enable proxy NDP, similarly to proxy ARP, described above in IPv4 section.
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/proxy_ndp", hostVethName), "1"); err != nil {
			return fmt.Errorf("failed to set net.ipv6.conf.%s.proxy_ndp=1: %s", hostVethName, err)
		}

		// Enable IP forwarding of packets coming _from_ this interface.  For packets to
		// be forwarded in both directions we need this flag to be set on the fabric-facing
		// interface too (or for the global default to be set).
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", hostVethName), "1"); err != nil {
			return fmt.Errorf("failed to set net.ipv6.conf.%s.forwarding=1: %s", hostVethName, err)
		}
	}

	return nil
}

// writeProcSys takes the sysctl path and a string value to set i.e. "0" or "1" and sets the sysctl.
func writeProcSys(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	n, err := f.Write([]byte(value))
	if err == nil && n < len(value) {
		err = io.ErrShortWrite
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}

// SetupRoutes sets up the routes for the host side of the veth pair.
func SetupRoutes(hostVeth netlink.Link, IPs []string) error {

	// Go through all the IPs and add routes for each IP in the result.
	for _, ipStr := range IPs {

		ipnet, err := parseCIDR(ipStr)
		if err != nil {
			return errors.Wrapf(err, "invalid ip: %s", ipStr)
		}
		route := netlink.Route{
			LinkIndex: hostVeth.Attrs().Index,
			Scope:     netlink.SCOPE_LINK,
			Dst:       ipnet,
		}
		err = netlink.RouteAdd(&route)

		if err != nil {
			switch err {

			// Route already exists, but not necessarily pointing to the same interface.
			case syscall.EEXIST:
				// List all the routes for the interface.
				routes, err := netlink.RouteList(hostVeth, netlink.FAMILY_ALL)
				if err != nil {
					return fmt.Errorf("error listing routes")
				}

				// Go through all the routes pointing to the interface, and see if any of them is
				// exactly what we are intending to program.
				// If the route we want is already there then most likely it's programmed by Felix, so we ignore it,
				// and we return an error if none of the routes match the route we're trying to program.
				log.WithFields(log.Fields{"route": route, "scope": route.Scope}).Debug("Constructed route")
				for _, r := range routes {
					log.WithFields(log.Fields{"interface": hostVeth.Attrs().Name, "route": r, "scope": r.Scope}).Debug("Routes for the interface")
					if r.LinkIndex == route.LinkIndex && r.Dst.IP.Equal(route.Dst.IP) && r.Scope == route.Scope {
						// Route was already present on the host.
						log.WithFields(log.Fields{"interface": hostVeth.Attrs().Name}).Infof("CNI skipping add route. Route already exists")
						return nil
					}
				}
				return fmt.Errorf("route (Ifindex: %d, Dst: %s, Scope: %v) already exists for an interface other than '%s'",
					route.LinkIndex, route.Dst.String(), route.Scope, hostVeth.Attrs().Name)
			default:
				return fmt.Errorf("failed to add route (Ifindex: %d, Dst: %s, Scope: %v, Iface: %s): %v",
					route.LinkIndex, route.Dst.String(), route.Scope, hostVeth.Attrs().Name, err)
			}
		}

		log.WithFields(log.Fields{"interface": hostVeth, "IP": ipStr}).Debugf("CNI adding route")
	}
	return nil
}
