// Copyright 2019 Michael Schubert <schu@schu.io>
// Copyright 2017 CNI authors
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

// This is a sample chained plugin that supports multiple CNI versions. It
// parses prevResult according to the cniVersion
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/schu/wireguard-cni/pkg/k8sutil"
	wgnetlink "github.com/schu/wireguard-cni/pkg/netlink"
	"github.com/schu/wireguard-cni/pkg/util"
)

func init() {
	log.SetPrefix("[wg-cni] ")
}

// PluginConf is whatever you expect your configuration json to be. This is whatever
// is passed in on stdin. Your plugin may wish to expose its functionality via
// runtime args, see CONVENTIONS.md in the CNI spec.
type PluginConf struct {
	types.NetConf // You may wish to not nest this type
	RuntimeConfig *struct {
		SampleConfig map[string]interface{} `json:"sample"`
	} `json:"runtimeConfig"`

	// This is the previous result, when called in the context of a chained
	// plugin. Because this plugin supports multiple versions, we'll have to
	// parse this in two passes. If your plugin is not chained, this can be
	// removed (though you may wish to error if a non-chainable plugin is
	// chained.
	// If you need to modify the result before returning it, you will need
	// to actually convert it to a concrete versioned struct.
	RawPrevResult *map[string]interface{} `json:"prevResult"`
	PrevResult    *current.Result         `json:"-"`

	// Add plugin-specifc flags here
	KubeConfigPath   string `json: "kubeConfigPath"`
	StaticConfigPath string `json: "staticConfigPath"`
}

// parseConfig parses the supplied configuration (and prevResult) from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	// Parse previous result. Remove this if your plugin is not chained.
	if conf.RawPrevResult != nil {
		resultBytes, err := json.Marshal(conf.RawPrevResult)
		if err != nil {
			return nil, fmt.Errorf("could not serialize prevResult: %v", err)
		}
		res, err := version.NewResult(conf.CNIVersion, resultBytes)
		if err != nil {
			return nil, fmt.Errorf("could not parse prevResult: %v", err)
		}
		conf.RawPrevResult = nil
		conf.PrevResult, err = current.NewResultFromResult(res)
		if err != nil {
			return nil, fmt.Errorf("could not convert result to current version: %v", err)
		}
	}
	// End previous result parsing

	// Do any validation here
	if conf.KubeConfigPath == "" && conf.StaticConfigPath == "" {
		return nil, fmt.Errorf("neither 'kubeConfigPath' nor 'staticConfigPath' given")
	}

	return &conf, nil
}

type kubernetesArgs struct {
	types.CommonArgs

	// Variable names must match CNI argument keys
	K8S_POD_NAMESPACE types.UnmarshallableString
	K8S_POD_NAME      types.UnmarshallableString
}

type wgCNIConfig struct {
	Address    string `json:"address"`
	PrivateKey string `json:"privateKey"`
	Peers      []struct {
		Endpoint            string   `json:"endpoint"`
		PublicKey           string   `json:"publicKey"`
		PresharedKey        string   `json:"presharedKey,omitempty"`
		PersistentKeepalive string   `json:"persistentKeepalive"`
		AllowedIPs          []string `json:"allowedIPs"`
	} `json:"peers"`
}

// cmdAdd is called for ADD requests
func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	if conf.PrevResult == nil {
		return fmt.Errorf("must be called as chained plugin")
	}

	var wgConfig wgCNIConfig
	if conf.KubeConfigPath != "" {
		clientset, err := k8sutil.NewClientset(conf.KubeConfigPath)
		if err != nil {
			return fmt.Errorf("could not get k8s clientset: %v", err)
		}

		var k8sArgs kubernetesArgs
		if err := types.LoadArgs(args.Args, &k8sArgs); err != nil {
			return fmt.Errorf("could not load CNI args %q: %v", args.Args, err)
		}

		podNamespace := string(k8sArgs.K8S_POD_NAMESPACE)
		podName := string(k8sArgs.K8S_POD_NAME)

		podSpec, err := clientset.CoreV1().Pods(podNamespace).Get(podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("could not get pod spec: %v", err)
		}

		if podSpec.ObjectMeta.Annotations == nil ||
			podSpec.ObjectMeta.Annotations["wgcni.schu.io/configsecret"] == "" {
			// This pod is not annoted to be configured
			// with wg-cni - nothing to do
			return types.PrintResult(conf.PrevResult, conf.CNIVersion)
		}

		configSecretName := podSpec.ObjectMeta.Annotations["wgcni.schu.io/configsecret"]

		wgConfigJSON, err := clientset.CoreV1().Secrets(podNamespace).Get(configSecretName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("could not get secret '%q' with wg-cni config: %v", configSecretName, err)
		}

		if err := json.Unmarshal(wgConfigJSON.Data["config.json"], &wgConfig); err != nil {
			return fmt.Errorf("could not unmarshal wg-cni config: %v", err)
		}
	}

	privateKey, err := wgtypes.ParseKey(wgConfig.PrivateKey)
	if err != nil {
		return fmt.Errorf("could not parse private key: %v", err)
	}

	var peers []wgtypes.PeerConfig
	for _, peerConf := range wgConfig.Peers {
		var peer wgtypes.PeerConfig

		peer.PublicKey, err = wgtypes.ParseKey(peerConf.PublicKey)
		if err != nil {
			return fmt.Errorf("could not parse public key: %v", err)
		}

		if peerConf.PresharedKey != "" {
			PresharedKey, err := wgtypes.ParseKey(peerConf.PresharedKey)
			if err != nil {
				return fmt.Errorf("could not parse preshared key: %v", err)
			}
			peer.PresharedKey = &PresharedKey
		}

		keepaliveInterval, err := time.ParseDuration(peerConf.PersistentKeepalive)
		if err != nil {
			return fmt.Errorf("could not parse keepalive duration string %q: %v", peerConf.PersistentKeepalive, err)
		}
		peer.PersistentKeepaliveInterval = &keepaliveInterval

		peer.Endpoint, err = net.ResolveUDPAddr("udp", peerConf.Endpoint)
		if err != nil {
			return fmt.Errorf("could not parse endpoint %q: %v", peerConf.Endpoint, err)
		}

		for _, allowedIP := range peerConf.AllowedIPs {
			_, ipnet, err := net.ParseCIDR(allowedIP)
			if err != nil {
				return fmt.Errorf("could not parse CIDR %q: %v", allowedIP, err)
			}

			peer.AllowedIPs = append(peer.AllowedIPs, *ipnet)
		}

		peers = append(peers, peer)
	}

	wgctrlConfig := wgtypes.Config{
		PrivateKey: &privateKey,
		Peers:      peers,
	}

	netnsHandle, err := netns.GetFromPath(args.Netns)
	if err != nil {
		return fmt.Errorf("could not get container net ns handle: %v", err)
	}

	linkName := "wg" + util.RandString(6)

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.Name = linkName

	wgLink := &wgnetlink.Wireguard{
		LinkAttrs: linkAttrs,
	}
	if err := netlink.LinkAdd(wgLink); err != nil {
		return fmt.Errorf("could not create wg network interface: %v", err)
	}

	sourceIP, sourceIPNet, err := net.ParseCIDR(wgConfig.Address)
	if err != nil {
		return fmt.Errorf("could not parse cidr %q: %v", wgConfig.Address, err)
	}

	addr := &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   sourceIP,
			Mask: sourceIPNet.Mask,
		},
	}

	wgClient, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("could not get wgctrl client: %v", err)
	}
	defer wgClient.Close()

	if err := wgClient.ConfigureDevice(linkName, wgctrlConfig); err != nil {
		return fmt.Errorf("could not configure wireguard link: %v", err)
	}

	if err := netlink.LinkSetNsFd(wgLink, (int)(netnsHandle)); err != nil {
		return fmt.Errorf("could not move network interface into container's net namespace: %v", err)
	}

	netnsNetlinkHandle, err := netlink.NewHandleAt(netnsHandle)
	if err != nil {
		return fmt.Errorf("could not get container net ns netlink handle: %v", err)
	}

	if err := netnsNetlinkHandle.AddrAdd(wgLink, addr); err != nil {
		return fmt.Errorf("could not add address: %v", err)
	}

	if err := netnsNetlinkHandle.LinkSetUp(wgLink); err != nil {
		return fmt.Errorf("could not set link up: %v", err)
	}

	for _, peer := range peers {
		for _, allowedIP := range peer.AllowedIPs {
			// For the source IP CIDR there is a route
			// already from `ip addr add ...` above.
			if allowedIP.Contains(sourceIP) {
				continue
			}

			route := &netlink.Route{
				LinkIndex: wgLink.Attrs().Index,
				Dst:       &allowedIP,
				Scope:     unix.RT_SCOPE_LINK,
			}
			if err := netnsNetlinkHandle.RouteAdd(route); err != nil {
				return fmt.Errorf("could not add route for %v: %v", route, err)
			}
		}
	}

	// Pass through the result for the next plugin
	return types.PrintResult(conf.PrevResult, conf.CNIVersion)
}

// cmdDel is called for DELETE requests
func cmdDel(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}
	_ = conf

	// Do your delete here

	return nil
}

func main() {
	// TODO: implement plugin version
	skel.PluginMain(cmdAdd, cmdGet, cmdDel, version.All, "TODO")
}

func cmdGet(args *skel.CmdArgs) error {
	// TODO: implement
	return fmt.Errorf("not implemented")
}
