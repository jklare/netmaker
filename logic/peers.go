package logic

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/c-robinson/iplib"
	"github.com/gravitl/netmaker/database"
	"github.com/gravitl/netmaker/logger"
	"github.com/gravitl/netmaker/logic/acls/nodeacls"
	"github.com/gravitl/netmaker/models"
	"github.com/gravitl/netmaker/nm-proxy/manager"
	"github.com/gravitl/netmaker/servercfg"
	"golang.org/x/exp/slices"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func GetPeersForProxy(node *models.Node, onlyPeers bool) (manager.ManagerPayload, error) {
	proxyPayload := manager.ManagerPayload{}
	var peers []wgtypes.PeerConfig
	peerConfMap := make(map[string]manager.PeerConf)
	var err error
	currentPeers, err := GetNetworkNodes(node.Network)
	if err != nil {
		return proxyPayload, err
	}
	if !onlyPeers {
		if node.IsRelayed == "yes" {
			relayNode := FindRelay(node)
			relayEndpoint, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", relayNode.Endpoint, relayNode.LocalListenPort))
			if err != nil {
				logger.Log(1, "failed to resolve relay node endpoint: ", err.Error())
			}
			proxyPayload.IsRelayed = true
			proxyPayload.RelayedTo = relayEndpoint

		}
		if node.IsRelay == "yes" {
			relayedNodes, err := GetRelayedNodes(node)
			if err != nil {
				logger.Log(1, "failed to relayed nodes: ", node.Name, err.Error())
				proxyPayload.IsRelay = false
			} else {

				relayPeersMap := make(map[string]manager.RelayedConf)
				for _, relayedNode := range relayedNodes {
					payload, err := GetPeersForProxy(&relayedNode, true)
					if err == nil {
						relayedEndpoint, udpErr := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", relayedNode.Endpoint, relayedNode.LocalListenPort))
						if udpErr == nil {
							relayPeersMap[relayedNode.PublicKey] = manager.RelayedConf{
								RelayedPeerEndpoint: relayedEndpoint,
								RelayedPeerPubKey:   relayedNode.PublicKey,
								Peers:               payload.Peers,
							}
						}

					}
				}
				proxyPayload.IsRelay = true
				proxyPayload.RelayedPeerConf = relayPeersMap
			}
		}

	}

	for _, peer := range currentPeers {
		if peer.ID == node.ID {
			//skip yourself
			continue
		}
		pubkey, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil {
			logger.Log(1, "failed to parse node pub key: ", peer.ID)
			continue
		}
		endpoint, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", peer.Endpoint, peer.LocalListenPort))
		if err != nil {
			logger.Log(1, "failed to resolve udp addr for node: ", peer.ID, peer.Endpoint, err.Error())
			continue
		}
		allowedips := getNodeAllowedIPs(node, &peer)
		var keepalive time.Duration
		if node.PersistentKeepalive != 0 {
			// set_keepalive
			keepalive, _ = time.ParseDuration(strconv.FormatInt(int64(node.PersistentKeepalive), 10) + "s")
		}
		peers = append(peers, wgtypes.PeerConfig{
			PublicKey:                   pubkey,
			Endpoint:                    endpoint,
			AllowedIPs:                  allowedips,
			PersistentKeepaliveInterval: &keepalive,
			ReplaceAllowedIPs:           true,
		})
		if !onlyPeers && peer.IsRelayed == "yes" {
			relayNode := FindRelay(&peer)
			if relayNode != nil {
				relayTo, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", relayNode.Endpoint, relayNode.LocalListenPort))
				if err == nil {
					peerConfMap[peer.PublicKey] = manager.PeerConf{
						IsRelayed: true,
						RelayedTo: relayTo,
						Address:   peer.PrimaryAddress(),
					}
				}

			}

		}
	}
	var extPeers []wgtypes.PeerConfig
	extPeers, peerConfMap, err = getExtPeersForProxy(node, peerConfMap)
	if err == nil {
		peers = append(peers, extPeers...)

	} else if !database.IsEmptyRecord(err) {
		logger.Log(1, "error retrieving external clients:", err.Error())
	}
	proxyPayload.Peers = peers
	proxyPayload.PeerMap = peerConfMap
	proxyPayload.InterfaceName = node.Interface
	return proxyPayload, nil
}

// GetPeerUpdate - gets a wireguard peer config for each peer of a node
func GetPeerUpdate(node *models.Node) (models.PeerUpdate, error) {
	var peerUpdate models.PeerUpdate
	var peers []wgtypes.PeerConfig
	var serverNodeAddresses = []models.ServerAddr{}
	var isP2S bool
	network, err := GetNetwork(node.Network)
	if err != nil {
		return peerUpdate, err
	} else if network.IsPointToSite == "yes" && node.IsHub != "yes" {
		isP2S = true
	}
	var peerMap = make(models.PeerMap)

	var metrics *models.Metrics
	if servercfg.Is_EE {
		metrics, _ = GetMetrics(node.ID)
	}
	if metrics == nil {
		metrics = &models.Metrics{}
	}
	if metrics.FailoverPeers == nil {
		metrics.FailoverPeers = make(map[string]string)
	}
	// udppeers = the peers parsed from the local interface
	// gives us correct port to reach
	udppeers, errN := database.GetPeers(node.Network)
	if errN != nil {
		logger.Log(2, errN.Error())
	}

	currentPeers, err := GetNetworkNodes(node.Network)
	if err != nil {
		return models.PeerUpdate{}, err
	}

	// if node.IsRelayed == "yes" {
	// 	return GetPeerUpdateForRelayedNode(node, udppeers)
	// }

	// #1 Set Keepalive values: set_keepalive
	// #2 Set local address: set_local - could be a LOT BETTER and fix some bugs with additional logic
	// #3 Set allowedips: set_allowedips
	for _, peer := range currentPeers {
		if peer.ID == node.ID {
			//skip yourself
			continue
		}
		// on point to site networks -- get peers regularily if you are the hub --- otherwise the only peer is the hub
		if node.NetworkSettings.IsPointToSite == "yes" && node.IsHub == "no" && peer.IsHub == "no" {
			continue
		}
		if node.Connected != "yes" {
			//skip unconnected nodes
			continue
		}

		// if the node is not a server, set the endpoint
		var setEndpoint = !(node.IsServer == "yes")

		// if peer.IsRelayed == "yes" {
		// 	if !(node.IsRelay == "yes" && ncutils.StringSliceContains(node.RelayAddrs, peer.PrimaryAddress())) {
		// 		//skip -- will be added to relay
		// 		continue
		// 	} else if node.IsRelay == "yes" && ncutils.StringSliceContains(node.RelayAddrs, peer.PrimaryAddress()) {
		// 		// dont set peer endpoint if it's relayed by node
		// 		setEndpoint = false
		// 	}
		// }
		if !nodeacls.AreNodesAllowed(nodeacls.NetworkID(node.Network), nodeacls.NodeID(node.ID), nodeacls.NodeID(peer.ID)) {
			//skip if not permitted by acl
			continue
		}
		if isP2S && peer.IsHub != "yes" {
			continue
		}
		if len(metrics.FailoverPeers[peer.ID]) > 0 && IsFailoverPresent(node.Network) {
			logger.Log(2, "peer", peer.Name, peer.PrimaryAddress(), "was found to be in failover peers list for node", node.Name, node.PrimaryAddress())
			continue
		}
		pubkey, err := wgtypes.ParseKey(peer.PublicKey)
		if err != nil {
			return models.PeerUpdate{}, err
		}
		if node.Endpoint == peer.Endpoint {
			//peer is on same network
			// set_local
			if node.LocalAddress != peer.LocalAddress && peer.LocalAddress != "" {
				peer.Endpoint = peer.LocalAddress
				if peer.LocalListenPort != 0 {
					peer.ListenPort = peer.LocalListenPort
				}
			} else {
				continue
			}
		}

		// set address if setEndpoint is true
		// otherwise, will get inserted as empty value
		var address *net.UDPAddr

		// Sets ListenPort to UDP Hole Punching Port assuming:
		// - UDP Hole Punching is enabled
		// - udppeers retrieval did not return an error
		// - the endpoint is valid
		if setEndpoint {

			var setUDPPort = false
			if peer.UDPHolePunch == "yes" && errN == nil && CheckEndpoint(udppeers[peer.PublicKey]) {
				endpointstring := udppeers[peer.PublicKey]
				endpointarr := strings.Split(endpointstring, ":")
				if len(endpointarr) == 2 {
					port, err := strconv.Atoi(endpointarr[1])
					if err == nil {
						setUDPPort = true
						peer.ListenPort = int32(port)
					}
				}
			}
			// if udp hole punching is on, but udp hole punching did not set it, use the LocalListenPort instead
			// or, if port is for some reason zero use the LocalListenPort
			// but only do this if LocalListenPort is not zero
			if ((peer.UDPHolePunch == "yes" && !setUDPPort) || peer.ListenPort == 0) && peer.LocalListenPort != 0 {
				peer.ListenPort = peer.LocalListenPort
			}

			endpoint := peer.Endpoint + ":" + strconv.FormatInt(int64(peer.ListenPort), 10)
			address, err = net.ResolveUDPAddr("udp", endpoint)
			if err != nil {
				return models.PeerUpdate{}, err
			}
		}

		allowedips := GetAllowedIPs(node, &peer, metrics)
		var keepalive time.Duration
		if node.PersistentKeepalive != 0 {
			// set_keepalive
			keepalive, _ = time.ParseDuration(strconv.FormatInt(int64(node.PersistentKeepalive), 10) + "s")
		}
		var peerData = wgtypes.PeerConfig{
			PublicKey:                   pubkey,
			Endpoint:                    address,
			ReplaceAllowedIPs:           true,
			AllowedIPs:                  allowedips,
			PersistentKeepaliveInterval: &keepalive,
		}

		peers = append(peers, peerData)
		peerMap[peer.PublicKey] = models.IDandAddr{
			Name:     peer.Name,
			ID:       peer.ID,
			Address:  peer.PrimaryAddress(),
			IsServer: peer.IsServer,
		}

		if peer.IsServer == "yes" {
			serverNodeAddresses = append(serverNodeAddresses, models.ServerAddr{IsLeader: IsLeader(&peer), Address: peer.Address})
		}
	}
	// if node.IsIngressGateway == "yes" {
	extPeers, idsAndAddr, err := getExtPeers(node)
	if err == nil {
		peers = append(peers, extPeers...)
		for i := range idsAndAddr {
			peerMap[idsAndAddr[i].ID] = idsAndAddr[i]
		}
	} else if !database.IsEmptyRecord(err) {
		logger.Log(1, "error retrieving external clients:", err.Error())
	}
	// }

	peerUpdate.Network = node.Network
	peerUpdate.ServerVersion = servercfg.Version
	peerUpdate.Peers = peers
	peerUpdate.ServerAddrs = serverNodeAddresses
	peerUpdate.DNS = getPeerDNS(node.Network)
	peerUpdate.PeerIDs = peerMap
	return peerUpdate, nil
}

func getExtPeers(node *models.Node) ([]wgtypes.PeerConfig, []models.IDandAddr, error) {
	var peers []wgtypes.PeerConfig
	var idsAndAddr []models.IDandAddr
	extPeers, err := GetNetworkExtClients(node.Network)
	if err != nil {
		return peers, idsAndAddr, err
	}
	for _, extPeer := range extPeers {
		pubkey, err := wgtypes.ParseKey(extPeer.PublicKey)
		if err != nil {
			logger.Log(1, "error parsing ext pub key:", err.Error())
			continue
		}

		if node.PublicKey == extPeer.PublicKey {
			continue
		}

		var allowedips []net.IPNet
		var peer wgtypes.PeerConfig
		if extPeer.Address != "" {
			var peeraddr = net.IPNet{
				IP:   net.ParseIP(extPeer.Address),
				Mask: net.CIDRMask(32, 32),
			}
			if peeraddr.IP != nil && peeraddr.Mask != nil {
				allowedips = append(allowedips, peeraddr)
			}
		}

		if extPeer.Address6 != "" {
			var addr6 = net.IPNet{
				IP:   net.ParseIP(extPeer.Address6),
				Mask: net.CIDRMask(128, 128),
			}
			if addr6.IP != nil && addr6.Mask != nil {
				allowedips = append(allowedips, addr6)
			}
		}

		primaryAddr := extPeer.Address
		if primaryAddr == "" {
			primaryAddr = extPeer.Address6
		}

		peer = wgtypes.PeerConfig{
			PublicKey:         pubkey,
			ReplaceAllowedIPs: true,
			AllowedIPs:        allowedips,
		}
		peers = append(peers, peer)
		idsAndAddr = append(idsAndAddr, models.IDandAddr{
			ID:      peer.PublicKey.String(),
			Address: primaryAddr,
		})
	}
	return peers, idsAndAddr, nil

}

func getExtPeersForProxy(node *models.Node, proxyPeerConf map[string]manager.PeerConf) ([]wgtypes.PeerConfig, map[string]manager.PeerConf, error) {
	var peers []wgtypes.PeerConfig

	extPeers, err := GetNetworkExtClients(node.Network)
	if err != nil {
		return peers, proxyPeerConf, err
	}
	for _, extPeer := range extPeers {
		pubkey, err := wgtypes.ParseKey(extPeer.PublicKey)
		if err != nil {
			logger.Log(1, "error parsing ext pub key:", err.Error())
			continue
		}

		if node.PublicKey == extPeer.PublicKey {
			continue
		}

		var allowedips []net.IPNet
		var peer wgtypes.PeerConfig
		if extPeer.Address != "" {
			var peeraddr = net.IPNet{
				IP:   net.ParseIP(extPeer.Address),
				Mask: net.CIDRMask(32, 32),
			}
			if peeraddr.IP != nil && peeraddr.Mask != nil {
				allowedips = append(allowedips, peeraddr)
			}
		}

		if extPeer.Address6 != "" {
			var addr6 = net.IPNet{
				IP:   net.ParseIP(extPeer.Address6),
				Mask: net.CIDRMask(128, 128),
			}
			if addr6.IP != nil && addr6.Mask != nil {
				allowedips = append(allowedips, addr6)
			}
		}

		peer = wgtypes.PeerConfig{
			PublicKey:         pubkey,
			ReplaceAllowedIPs: true,
			AllowedIPs:        allowedips,
		}

		extConf := manager.PeerConf{
			IsExtClient: true,
			Address:     extPeer.Address,
		}
		if extPeer.IngressGatewayID == node.ID {
			extConf.IsAttachedExtClient = true
		}
		ingGatewayUdpAddr, err := net.ResolveUDPAddr("udp", extPeer.IngressGatewayEndpoint)
		if err == nil {
			extConf.IngressGatewayEndPoint = ingGatewayUdpAddr
		}

		proxyPeerConf[peer.PublicKey.String()] = extConf

		peers = append(peers, peer)
	}
	return peers, proxyPeerConf, nil

}

// GetAllowedIPs - calculates the wireguard allowedip field for a peer of a node based on the peer and node settings
func GetAllowedIPs(node, peer *models.Node, metrics *models.Metrics) []net.IPNet {
	var allowedips []net.IPNet
	allowedips = getNodeAllowedIPs(peer, node)

	// handle ingress gateway peers
	// if peer.IsIngressGateway == "yes" {
	// 	extPeers, _, err := getExtPeers(peer)
	// 	if err != nil {
	// 		logger.Log(2, "could not retrieve ext peers for ", peer.Name, err.Error())
	// 	}
	// 	for _, extPeer := range extPeers {
	// 		allowedips = append(allowedips, extPeer.AllowedIPs...)
	// 	}
	// 	// if node is a failover node, add allowed ips from nodes it is handling
	// 	if peer.Failover == "yes" && metrics.FailoverPeers != nil {
	// 		// traverse through nodes that need handling
	// 		logger.Log(3, "peer", peer.Name, "was found to be failover for", node.Name, "checking failover peers...")
	// 		for k := range metrics.FailoverPeers {
	// 			// if FailoverNode is me for this node, add allowedips
	// 			if metrics.FailoverPeers[k] == peer.ID {
	// 				// get original node so we can traverse the allowed ips
	// 				nodeToFailover, err := GetNodeByID(k)
	// 				if err == nil {
	// 					failoverNodeMetrics, err := GetMetrics(nodeToFailover.ID)
	// 					if err == nil && failoverNodeMetrics != nil {
	// 						if len(failoverNodeMetrics.NodeName) > 0 {
	// 							allowedips = append(allowedips, getNodeAllowedIPs(&nodeToFailover, peer)...)
	// 							logger.Log(0, "failing over node", nodeToFailover.Name, nodeToFailover.PrimaryAddress(), "to failover node", peer.Name)
	// 						}
	// 					}
	// 				}
	// 			}
	// 		}
	// 	}
	// }
	// handle relay gateway peers
	// if peer.IsRelay == "yes" {
	// 	for _, ip := range peer.RelayAddrs {
	// 		//find node ID of relayed peer
	// 		relayedPeer, err := findNode(ip)
	// 		if err != nil {
	// 			logger.Log(0, "failed to find node for ip ", ip, err.Error())
	// 			continue
	// 		}
	// 		if relayedPeer == nil {
	// 			continue
	// 		}
	// 		if relayedPeer.ID == node.ID {
	// 			//skip self
	// 			continue
	// 		}
	// 		//check if acl permits comms
	// 		if !nodeacls.AreNodesAllowed(nodeacls.NetworkID(node.Network), nodeacls.NodeID(node.ID), nodeacls.NodeID(relayedPeer.ID)) {
	// 			continue
	// 		}
	// 		if iplib.Version(net.ParseIP(ip)) == 4 {
	// 			relayAddr := net.IPNet{
	// 				IP:   net.ParseIP(ip),
	// 				Mask: net.CIDRMask(32, 32),
	// 			}
	// 			allowedips = append(allowedips, relayAddr)
	// 		}
	// 		if iplib.Version(net.ParseIP(ip)) == 6 {
	// 			relayAddr := net.IPNet{
	// 				IP:   net.ParseIP(ip),
	// 				Mask: net.CIDRMask(128, 128),
	// 			}
	// 			allowedips = append(allowedips, relayAddr)
	// 		}
	// 		relayedNode, err := findNode(ip)
	// 		if err != nil {
	// 			logger.Log(1, "unable to find node for relayed address", ip, err.Error())
	// 			continue
	// 		}
	// 		if relayedNode.IsEgressGateway == "yes" {
	// 			extAllowedIPs := getEgressIPs(node, relayedNode)
	// 			allowedips = append(allowedips, extAllowedIPs...)
	// 		}
	// 		if relayedNode.IsIngressGateway == "yes" {
	// 			extPeers, _, err := getExtPeers(relayedNode)
	// 			if err == nil {
	// 				for _, extPeer := range extPeers {
	// 					allowedips = append(allowedips, extPeer.AllowedIPs...)
	// 				}
	// 			} else {
	// 				logger.Log(0, "failed to retrieve extclients from relayed ingress", err.Error())
	// 			}
	// 		}
	// 	}
	// }
	return allowedips
}

func getPeerDNS(network string) string {
	var dns string
	if nodes, err := GetNetworkNodes(network); err == nil {
		for i := range nodes {
			dns = dns + fmt.Sprintf("%s %s.%s\n", nodes[i].Address, nodes[i].Name, nodes[i].Network)
		}
	}

	if customDNSEntries, err := GetCustomDNS(network); err == nil {
		for _, entry := range customDNSEntries {
			// TODO - filter entries based on ACLs / given peers vs nodes in network
			dns = dns + fmt.Sprintf("%s %s.%s\n", entry.Address, entry.Name, entry.Network)
		}
	}
	return dns
}

// GetPeerUpdateForRelayedNode - calculates peer update for a relayed node by getting the relay
// copying the relay node's allowed ips and making appropriate substitutions
func GetPeerUpdateForRelayedNode(node *models.Node, udppeers map[string]string) (models.PeerUpdate, error) {
	var peerUpdate models.PeerUpdate
	var peers []wgtypes.PeerConfig
	var serverNodeAddresses = []models.ServerAddr{}
	var allowedips []net.IPNet
	//find node that is relaying us
	relay := FindRelay(node)
	if relay == nil {
		return models.PeerUpdate{}, errors.New("not found")
	}

	//add relay to lists of allowed ip
	if relay.Address != "" {
		relayIP := net.IPNet{
			IP:   net.ParseIP(relay.Address),
			Mask: net.CIDRMask(32, 32),
		}
		allowedips = append(allowedips, relayIP)
	}
	if relay.Address6 != "" {
		relayIP6 := net.IPNet{
			IP:   net.ParseIP(relay.Address6),
			Mask: net.CIDRMask(128, 128),
		}
		allowedips = append(allowedips, relayIP6)
	}
	//get PeerUpdate for relayed node
	relayPeerUpdate, err := GetPeerUpdate(relay)
	if err != nil {
		return models.PeerUpdate{}, err
	}
	//add the relays allowed ips from all of the relay's peers
	for _, peer := range relayPeerUpdate.Peers {
		allowedips = append(allowedips, peer.AllowedIPs...)
	}
	//delete any ips not permitted by acl
	for i := len(allowedips) - 1; i >= 0; i-- {
		target, err := findNode(allowedips[i].IP.String())
		if err != nil {
			logger.Log(0, "failed to find node for ip", allowedips[i].IP.String(), err.Error())
			continue
		}
		if target == nil {
			logger.Log(0, "failed to find node for ip", allowedips[i].IP.String())
			continue
		}
		if !nodeacls.AreNodesAllowed(nodeacls.NetworkID(node.Network), nodeacls.NodeID(node.ID), nodeacls.NodeID(target.ID)) {
			logger.Log(0, "deleting node from relayednode per acl", node.Name, target.Name)
			allowedips = append(allowedips[:i], allowedips[i+1:]...)
		}
	}
	//delete self from allowed ips
	for i := len(allowedips) - 1; i >= 0; i-- {
		if allowedips[i].IP.String() == node.Address || allowedips[i].IP.String() == node.Address6 {
			allowedips = append(allowedips[:i], allowedips[i+1:]...)
		}
	}
	//delete egressrange from allowedip if we are egress gateway
	if node.IsEgressGateway == "yes" {
		for i := len(allowedips) - 1; i >= 0; i-- {
			if StringSliceContains(node.EgressGatewayRanges, allowedips[i].String()) {
				allowedips = append(allowedips[:i], allowedips[i+1:]...)
			}
		}
	}
	//delete extclients from allowedip if we are ingress gateway
	if node.IsIngressGateway == "yes" {
		for i := len(allowedips) - 1; i >= 0; i-- {
			if strings.Contains(node.IngressGatewayRange, allowedips[i].IP.String()) {
				allowedips = append(allowedips[:i], allowedips[i+1:]...)
			}
		}
	}
	//add egress range if relay is egress
	if relay.IsEgressGateway == "yes" {
		var ip *net.IPNet
		for _, cidr := range relay.EgressGatewayRanges {
			_, ip, err = net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
		}
		allowedips = append(allowedips, *ip)
	}

	pubkey, err := wgtypes.ParseKey(relay.PublicKey)
	if err != nil {
		return models.PeerUpdate{}, err
	}
	var setUDPPort = false
	if relay.UDPHolePunch == "yes" && CheckEndpoint(udppeers[relay.PublicKey]) {
		endpointstring := udppeers[relay.PublicKey]
		endpointarr := strings.Split(endpointstring, ":")
		if len(endpointarr) == 2 {
			port, err := strconv.Atoi(endpointarr[1])
			if err == nil {
				setUDPPort = true
				relay.ListenPort = int32(port)
			}
		}
	}
	// if udp hole punching is on, but udp hole punching did not set it, use the LocalListenPort instead
	// or, if port is for some reason zero use the LocalListenPort
	// but only do this if LocalListenPort is not zero
	if ((relay.UDPHolePunch == "yes" && !setUDPPort) || relay.ListenPort == 0) && relay.LocalListenPort != 0 {
		relay.ListenPort = relay.LocalListenPort
	}

	endpoint := relay.Endpoint + ":" + strconv.FormatInt(int64(relay.ListenPort), 10)
	address, err := net.ResolveUDPAddr("udp", endpoint)
	if err != nil {
		return models.PeerUpdate{}, err
	}
	var keepalive time.Duration
	if node.PersistentKeepalive != 0 {
		// set_keepalive
		keepalive, _ = time.ParseDuration(strconv.FormatInt(int64(node.PersistentKeepalive), 10) + "s")
	}
	var peerData = wgtypes.PeerConfig{
		PublicKey:                   pubkey,
		Endpoint:                    address,
		ReplaceAllowedIPs:           true,
		AllowedIPs:                  allowedips,
		PersistentKeepaliveInterval: &keepalive,
	}
	peers = append(peers, peerData)
	if relay.IsServer == "yes" {
		serverNodeAddresses = append(serverNodeAddresses, models.ServerAddr{IsLeader: IsLeader(relay), Address: relay.Address})
	}
	//if ingress add extclients
	if node.IsIngressGateway == "yes" {
		extPeers, _, err := getExtPeers(node)
		if err == nil {
			peers = append(peers, extPeers...)
		} else {
			logger.Log(2, "could not retrieve ext peers for ", node.Name, err.Error())
		}
	}
	peerUpdate.Network = node.Network
	peerUpdate.ServerVersion = servercfg.Version
	peerUpdate.Peers = peers
	peerUpdate.ServerAddrs = serverNodeAddresses
	peerUpdate.DNS = getPeerDNS(node.Network)
	return peerUpdate, nil
}

func getEgressIPs(node, peer *models.Node) []net.IPNet {
	//check for internet gateway
	internetGateway := false
	if slices.Contains(peer.EgressGatewayRanges, "0.0.0.0/0") || slices.Contains(peer.EgressGatewayRanges, "::/0") {
		internetGateway = true
	}
	allowedips := []net.IPNet{}
	for _, iprange := range peer.EgressGatewayRanges { // go through each cidr for egress gateway
		_, ipnet, err := net.ParseCIDR(iprange) // confirming it's valid cidr
		if err != nil {
			logger.Log(1, "could not parse gateway IP range. Not adding ", iprange)
			continue // if can't parse CIDR
		}
		nodeEndpointArr := strings.Split(peer.Endpoint, ":")                     // getting the public ip of node
		if ipnet.Contains(net.ParseIP(nodeEndpointArr[0])) && !internetGateway { // ensuring egress gateway range does not contain endpoint of node
			logger.Log(2, "egress IP range of ", iprange, " overlaps with ", node.Endpoint, ", omitting")
			continue // skip adding egress range if overlaps with node's ip
		}
		// TODO: Could put in a lot of great logic to avoid conflicts / bad routes
		if ipnet.Contains(net.ParseIP(node.LocalAddress)) && !internetGateway { // ensuring egress gateway range does not contain public ip of node
			logger.Log(2, "egress IP range of ", iprange, " overlaps with ", node.LocalAddress, ", omitting")
			continue // skip adding egress range if overlaps with node's local ip
		}
		if err != nil {
			logger.Log(1, "error encountered when setting egress range", err.Error())
		} else {
			allowedips = append(allowedips, *ipnet)
		}
	}
	return allowedips
}

func getNodeAllowedIPs(peer, node *models.Node) []net.IPNet {
	var allowedips = []net.IPNet{}

	if peer.Address != "" {
		var peeraddr = net.IPNet{
			IP:   net.ParseIP(peer.Address),
			Mask: net.CIDRMask(32, 32),
		}
		allowedips = append(allowedips, peeraddr)
	}

	if peer.Address6 != "" {
		var addr6 = net.IPNet{
			IP:   net.ParseIP(peer.Address6),
			Mask: net.CIDRMask(128, 128),
		}
		allowedips = append(allowedips, addr6)
	}
	// handle manually set peers
	for _, allowedIp := range peer.AllowedIPs {

		// parsing as a CIDR first. If valid CIDR, append
		if _, ipnet, err := net.ParseCIDR(allowedIp); err == nil {
			nodeEndpointArr := strings.Split(node.Endpoint, ":")
			if !ipnet.Contains(net.IP(nodeEndpointArr[0])) && ipnet.IP.String() != peer.Address { // don't need to add an allowed ip that already exists..
				allowedips = append(allowedips, *ipnet)
			}

		} else { // parsing as an IP second. If valid IP, check if ipv4 or ipv6, then append
			if iplib.Version(net.ParseIP(allowedIp)) == 4 && allowedIp != peer.Address {
				ipnet := net.IPNet{
					IP:   net.ParseIP(allowedIp),
					Mask: net.CIDRMask(32, 32),
				}
				allowedips = append(allowedips, ipnet)
			} else if iplib.Version(net.ParseIP(allowedIp)) == 6 && allowedIp != peer.Address6 {
				ipnet := net.IPNet{
					IP:   net.ParseIP(allowedIp),
					Mask: net.CIDRMask(128, 128),
				}
				allowedips = append(allowedips, ipnet)
			}
		}
	}
	// handle egress gateway peers
	if peer.IsEgressGateway == "yes" {
		//hasGateway = true
		egressIPs := getEgressIPs(node, peer)
		// remove internet gateway if server
		if node.IsServer == "yes" {
			for i := len(egressIPs) - 1; i >= 0; i-- {
				if egressIPs[i].String() == "0.0.0.0/0" || egressIPs[i].String() == "::/0" {
					egressIPs = append(egressIPs[:i], egressIPs[i+1:]...)
				}
			}
		}
		allowedips = append(allowedips, egressIPs...)
	}
	return allowedips
}
