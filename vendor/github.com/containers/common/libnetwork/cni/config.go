// +build linux

package cni

import (
	"net"
	"os"

	internalutil "github.com/containers/common/libnetwork/internal/util"
	"github.com/containers/common/libnetwork/types"
	pkgutil "github.com/containers/common/pkg/util"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
)

// NetworkCreate will take a partial filled Network and fill the
// missing fields. It creates the Network and returns the full Network.
// nolint:gocritic
func (n *cniNetwork) NetworkCreate(net types.Network) (types.Network, error) {
	n.lock.Lock()
	defer n.lock.Unlock()
	err := n.loadNetworks()
	if err != nil {
		return types.Network{}, err
	}
	network, err := n.networkCreate(&net, false)
	if err != nil {
		return types.Network{}, err
	}
	// add the new network to the map
	n.networks[network.libpodNet.Name] = network
	return *network.libpodNet, nil
}

// networkCreate will fill out the given network struct and return the new network entry.
// If defaultNet is true it will not validate against used subnets and it will not write the cni config to disk.
func (n *cniNetwork) networkCreate(newNetwork *types.Network, defaultNet bool) (*network, error) {
	// if no driver is set use the default one
	if newNetwork.Driver == "" {
		newNetwork.Driver = types.DefaultNetworkDriver
	}

	// FIXME: Should we use a different type for network create without the ID field?
	// the caller is not allowed to set a specific ID
	if newNetwork.ID != "" {
		return nil, errors.Wrap(types.ErrInvalidArg, "ID can not be set for network create")
	}

	err := internalutil.CommonNetworkCreate(n, newNetwork)
	if err != nil {
		return nil, err
	}

	// Only get the used networks for validation if we do not create the default network.
	// The default network should not be validated against used subnets, we have to ensure
	// that this network can always be created even when a subnet is already used on the host.
	// This could happen if you run a container on this net, then the cni interface will be
	// created on the host and "block" this subnet from being used again.
	// Therefore the next podman command tries to create the default net again and it would
	// fail because it thinks the network is used on the host.
	var usedNetworks []*net.IPNet
	if !defaultNet && newNetwork.Driver == types.BridgeNetworkDriver {
		usedNetworks, err = internalutil.GetUsedSubnets(n)
		if err != nil {
			return nil, err
		}
	}

	switch newNetwork.Driver {
	case types.BridgeNetworkDriver:
		err = internalutil.CreateBridge(n, newNetwork, usedNetworks, n.defaultsubnetPools)
		if err != nil {
			return nil, err
		}
	case types.MacVLANNetworkDriver, types.IPVLANNetworkDriver:
		err = createIPMACVLAN(newNetwork)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.Wrapf(types.ErrInvalidArg, "unsupported driver %s", newNetwork.Driver)
	}

	err = internalutil.ValidateSubnets(newNetwork, !newNetwork.Internal, usedNetworks)
	if err != nil {
		return nil, err
	}

	// generate the network ID
	newNetwork.ID = getNetworkIDFromName(newNetwork.Name)

	// FIXME: Should this be a hard error?
	if newNetwork.DNSEnabled && newNetwork.Internal && hasDNSNamePlugin(n.cniPluginDirs) {
		logrus.Warnf("dnsname and internal networks are incompatible. dnsname plugin not configured for network %s", newNetwork.Name)
		newNetwork.DNSEnabled = false
	}

	cniConf, path, err := n.createCNIConfigListFromNetwork(newNetwork, !defaultNet)
	if err != nil {
		return nil, err
	}
	return &network{cniNet: cniConf, libpodNet: newNetwork, filename: path}, nil
}

// NetworkRemove will remove the Network with the given name or ID.
// It does not ensure that the network is unused.
func (n *cniNetwork) NetworkRemove(nameOrID string) error {
	n.lock.Lock()
	defer n.lock.Unlock()
	err := n.loadNetworks()
	if err != nil {
		return err
	}

	network, err := n.getNetwork(nameOrID)
	if err != nil {
		return err
	}

	// Removing the default network is not allowed.
	if network.libpodNet.Name == n.defaultNetwork {
		return errors.Errorf("default network %s cannot be removed", n.defaultNetwork)
	}

	// Remove the bridge network interface on the host.
	if network.libpodNet.Driver == types.BridgeNetworkDriver {
		link, err := netlink.LinkByName(network.libpodNet.NetworkInterface)
		if err == nil {
			err = netlink.LinkDel(link)
			// only log the error, it is not fatal
			if err != nil {
				logrus.Infof("Failed to remove network interface %s: %v", network.libpodNet.NetworkInterface, err)
			}
		}
	}

	file := network.filename
	delete(n.networks, network.libpodNet.Name)

	// make sure to not error for ErrNotExist
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// NetworkList will return all known Networks. Optionally you can
// supply a list of filter functions. Only if a network matches all
// functions it is returned.
func (n *cniNetwork) NetworkList(filters ...types.FilterFunc) ([]types.Network, error) {
	n.lock.Lock()
	defer n.lock.Unlock()
	err := n.loadNetworks()
	if err != nil {
		return nil, err
	}

	networks := make([]types.Network, 0, len(n.networks))
outer:
	for _, net := range n.networks {
		for _, filter := range filters {
			// All filters have to match, if one does not match we can skip to the next network.
			if !filter(*net.libpodNet) {
				continue outer
			}
		}
		networks = append(networks, *net.libpodNet)
	}
	return networks, nil
}

// NetworkInspect will return the Network with the given name or ID.
func (n *cniNetwork) NetworkInspect(nameOrID string) (types.Network, error) {
	n.lock.Lock()
	defer n.lock.Unlock()
	err := n.loadNetworks()
	if err != nil {
		return types.Network{}, err
	}

	network, err := n.getNetwork(nameOrID)
	if err != nil {
		return types.Network{}, err
	}
	return *network.libpodNet, nil
}

func createIPMACVLAN(network *types.Network) error {
	if network.Internal {
		return errors.New("internal is not supported with macvlan")
	}
	if network.NetworkInterface != "" {
		interfaceNames, err := internalutil.GetLiveNetworkNames()
		if err != nil {
			return err
		}
		if !pkgutil.StringInSlice(network.NetworkInterface, interfaceNames) {
			return errors.Errorf("parent interface %s does not exist", network.NetworkInterface)
		}
	}
	if len(network.Subnets) == 0 {
		network.IPAMOptions["driver"] = types.DHCPIPAMDriver
	} else {
		network.IPAMOptions["driver"] = types.HostLocalIPAMDriver
	}
	return nil
}
