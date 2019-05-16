/*
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements.  See the NOTICE file
distributed with this work for additional information
regarding copyright ownership.  The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License.  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
*/

package openflow

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/k-vswitch/k-vswitch/apis/kvswitch/v1alpha1"
	"github.com/k-vswitch/k-vswitch/flows"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog"
)

const (
	tableClassification         = 0
	tableLocalARP               = 10
	tableOverlay                = 20
	tableL3Rewrites             = 30
	tableL3Forwarding           = 40
	tableL2Rewrites             = 50
	tableL2Forwarding           = 60
	tableNetworkPoliciesIngress = 70
	tableNetworkPoliciesEgress  = 80
	tableProxy                  = 90
	tableNAT                    = 100
	tableAudit                  = 110
)

func (c *controller) syncFlows() error {
	c.flows.Reset()

	err := c.defaultFlows()
	if err != nil {
		return err
	}

	vswitchCfgs, err := c.vswitchLister.List(labels.Everything())
	if err != nil {
		return err
	}

	for _, vswitchCfg := range vswitchCfgs {
		err = c.flowsForVSwitch(vswitchCfg)
		if err != nil {
			return err
		}
	}

	pods, err := c.podLister.List(labels.Everything())
	for _, pod := range pods {
		err = c.flowsForPod(pod)
		if err != nil {
			return err
		}
	}

	return c.flows.SyncFlows(c.bridgeName)
}

func (c *controller) defaultFlows() error {
	flow := flows.NewFlow().WithTable(tableClassification).
		WithPriority(0).
		WithOutputPort(c.hostLocalOFPort)
	c.flows.AddFlow(flow)

	flow = flows.NewFlow().WithTable(tableClassification).
		WithPriority(500).WithProtocol("ip").
		WithIPDest(c.gatewayIP).WithModDlDest(c.gatewayMAC).
		WithOutputPort(c.hostLocalOFPort)
	c.flows.AddFlow(flow)

	flow = flows.NewFlow().WithTable(tableClassification).
		WithPriority(200).WithProtocol("arp").WithResubmit(tableLocalARP)
	c.flows.AddFlow(flow)

	flow = flows.NewFlow().WithTable(tableLocalARP).
		WithPriority(500).WithProtocol("arp").
		WithArpDest(c.gatewayIP).WithOutputPort(c.hostLocalOFPort)
	c.flows.AddFlow(flow)

	flow = flows.NewFlow().WithTable(tableLocalARP).
		WithPriority(100).WithProtocol("arp").
		WithArpDest(c.podCIDR).WithResubmit(tableL2Rewrites)
	c.flows.AddFlow(flow)

	flow = flows.NewFlow().WithTable(tableLocalARP).
		WithPriority(50).WithProtocol("arp").
		WithArpDest(c.clusterCIDR).WithResubmit(tableOverlay)
	c.flows.AddFlow(flow)

	return nil
}

func (c *controller) flowsForVSwitch(vswitch *v1alpha1.VSwitchConfig) error {
	isCurrentNode := false
	if vswitch.Name == c.nodeName {
		isCurrentNode = true
	}

	podCIDR := vswitch.Spec.PodCIDR

	//
	// flows for table 0 - classification
	//

	// traffic in the local pod CIDR should go to tableL2Rewrites
	// TODO: put this in a separate function for "local" flows
	flow := flows.NewFlow().WithTable(tableClassification).
		WithPriority(300).WithProtocol("ip").WithIPDest(c.podCIDR).
		WithResubmit(tableL2Rewrites)
	c.flows.AddFlow(flow)

	// traffic in the cluster CIDR without tunnel ID should go to tableOverlay
	flow = flows.NewFlow().WithTable(tableClassification).
		WithPriority(100).WithProtocol("ip").
		WithIPDest(c.clusterCIDR).WithResubmit(tableOverlay)
	c.flows.AddFlow(flow)

	//
	// flow for table 10 - overlay
	//

	if !isCurrentNode {
		// IPv4
		flow = flows.NewFlow().WithTable(tableOverlay).
			WithPriority(100).WithProtocol("ip").WithIPDest(podCIDR).
			WithTunnelIDField(vswitch.Spec.OverlayTunnelID).
			WithResubmit(tableL3Forwarding)
		c.flows.AddFlow(flow)

		// ARP
		flow = flows.NewFlow().WithTable(tableOverlay).
			WithPriority(100).WithProtocol("arp").WithArpDest(podCIDR).
			WithTunnelIDField(vswitch.Spec.OverlayTunnelID).
			WithResubmit(tableL3Forwarding)
		c.flows.AddFlow(flow)
	}

	//
	// flow for table 30 - L3 Forwarding
	//

	// If pod cidr is not for current node, output to vxlan overlay port
	// If pod cidr is for current node, go straight to L2 rewrites
	if !isCurrentNode {
		flow = flows.NewFlow().WithTable(tableL3Forwarding).
			WithPriority(150).WithProtocol("ip").WithTunnelID(vswitch.Spec.OverlayTunnelID).
			WithIPDest(podCIDR).WithTunnelDest(vswitch.Spec.OverlayIP).
			WithOutputPort(c.vxlanOFPort)
		c.flows.AddFlow(flow)

		flow = flows.NewFlow().WithTable(tableL3Forwarding).
			WithPriority(150).WithProtocol("arp").WithTunnelID(vswitch.Spec.OverlayTunnelID).
			WithArpDest(podCIDR).WithTunnelDest(vswitch.Spec.OverlayIP).
			WithOutputPort(c.vxlanOFPort)
		c.flows.AddFlow(flow)

	} else {
		flow = flows.NewFlow().WithTable(tableL3Forwarding).
			WithPriority(100).WithProtocol("ip").WithIPDest(podCIDR).
			WithTunnelID(vswitch.Spec.OverlayTunnelID).
			WithResubmit(tableL2Rewrites)
		c.flows.AddFlow(flow)

		flow = flows.NewFlow().WithTable(tableL3Forwarding).
			WithPriority(100).WithProtocol("arp").WithArpDest(podCIDR).
			WithTunnelID(vswitch.Spec.OverlayTunnelID).
			WithResubmit(tableL2Rewrites)
		c.flows.AddFlow(flow)
	}

	return nil
}

func (c *controller) isLocalIP(ip string) (bool, error) {
	_, ipnet, err := net.ParseCIDR(c.podCIDR)
	if err != nil {
		return false, err
	}

	return ipnet.Contains(net.ParseIP(ip)), nil
}

func (c *controller) flowsForPod(pod *corev1.Pod) error {
	podIP := pod.Status.PodIP
	if podIP == "" {
		return nil
	}

	local, err := c.isLocalIP(podIP)
	if err != nil {
		return fmt.Errorf("error checking if IP %q is local: %v", err)
	}

	if !local {
		return nil
	}

	// TODO: cache port/mac info for each pod namespace/name combination
	portName, err := findPort(pod.Namespace, pod.Name)
	if err != nil {
		return fmt.Errorf("error finding port for pod %q, err: %v", pod.Name, err)
	}

	podMacAddr, err := macAddrFromPort(portName)
	if err != nil {
		return fmt.Errorf("error getting mac address for pod %q, err: %v", pod.Name, err)
	}

	ofport, err := ofPortFromName(portName)
	if err != nil {
		return fmt.Errorf("error getting ofport for port %q, err: %v", portName, err)
	}

	flow := flows.NewFlow().WithTable(tableL2Rewrites).WithPriority(100).
		WithProtocol("ip").WithIPDest(podIP).WithModDlDest(podMacAddr).
		WithOutputPort(ofport)
	c.flows.AddFlow(flow)

	flow = flows.NewFlow().WithTable(tableL2Rewrites).WithPriority(100).
		WithProtocol("arp").WithArpDest(podIP).WithOutputPort(ofport)
	c.flows.AddFlow(flow)

	return nil
}

func findPort(podNamespace, podName string) (string, error) {
	commands := []string{
		"--format=json", "--column=name", "find", "port",
		fmt.Sprintf("external-ids:k8s_pod_namespace=%s", podNamespace),
		fmt.Sprintf("external-ids:k8s_pod_name=%s", podName),
	}

	out, err := exec.Command("ovs-vsctl", commands...).Output()
	if err != nil {
		return "", fmt.Errorf("failed to get OVS port for %s/%s, err: %v",
			podNamespace, podName, err)
	}

	dbData := struct {
		Data [][]string
	}{}
	if err = json.Unmarshal(out, &dbData); err != nil {
		return "", err
	}

	if len(dbData.Data) == 0 {
		// TODO: might make more sense to not return an error here since
		// CNI delete can be called multiple times.
		return "", fmt.Errorf("OVS port for %s/%s was not found, OVS DB data: %v, output: %q",
			podNamespace, podName, dbData.Data, string(out))
	}

	portName := dbData.Data[0][0]
	return portName, nil
}

func macAddrFromPort(portName string) (string, error) {
	commands := []string{
		"get", "port", portName, "mac",
	}

	out, err := exec.Command("ovs-vsctl", commands...).Output()
	if err != nil {
		return "", fmt.Errorf("failed to get MAC address from OVS port for %q, err: %v, out: %q",
			portName, err, string(out))
	}

	// TODO: validate mac address
	macAddr := strings.TrimSpace(string(out))
	if len(macAddr) > 0 && macAddr[0] == '"' {
		macAddr = macAddr[1:]
	}
	if len(macAddr) > 0 && macAddr[len(macAddr)-1] == '"' {
		macAddr = macAddr[:len(macAddr)-1]
	}

	return macAddr, nil
}

func (c *controller) OnAddVSwitch(obj interface{}) {
	err := c.syncFlows()
	if err != nil {
		klog.Errorf("error syncing flows: %v", err)
		return
	}
}

func (c *controller) OnUpdateVSwitch(oldObj, newObj interface{}) {
	err := c.syncFlows()
	if err != nil {
		klog.Errorf("error syncing flows: %v", err)
		return
	}
}

func (c *controller) OnDeleteVSwitch(obj interface{}) {
}

func (c *controller) OnAddPod(obj interface{}) {
	if err := c.syncFlows(); err != nil {
		klog.Errorf("error syncing pod: %v", err)
		return
	}
}

func (c *controller) OnUpdatePod(oldObj, newObj interface{}) {
	if err := c.syncFlows(); err != nil {
		klog.Errorf("error syncing pod : %v", err)
		return
	}
}

func (c *controller) OnDeletePod(obj interface{}) {
	_, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
}