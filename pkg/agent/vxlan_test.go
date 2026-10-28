// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"testing"

	"github.com/go-logr/logr"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/spidernet-io/egressgateway/pkg/agent/vxlan"
	"github.com/spidernet-io/egressgateway/pkg/config"
	egressv1 "github.com/spidernet-io/egressgateway/pkg/k8s/apis/v1beta1"
	"github.com/spidernet-io/egressgateway/pkg/schema"
	"github.com/spidernet-io/egressgateway/pkg/utils"
)

func readyTunnel(name, ip, mac string) *egressv1.EgressTunnel {
	return &egressv1.EgressTunnel{
		ObjectMeta: v1.ObjectMeta{Name: name},
		Status: egressv1.EgressTunnelStatus{
			Phase:  egressv1.EgressTunnelReady,
			Tunnel: egressv1.Tunnel{IPv4: ip, MAC: mac},
		},
	}
}

// The local VTEP must come from the local node's own EgressTunnel, whatever
// the order of the listing. keepVXLAN creates the link from this entry, so a
// peer's address here gives the local device the peer's MAC.
func TestInitTunnelPeerMapKeepsOwnVTEP(t *testing.T) {
	// The local node is listed first, so a peer listed after it would win
	// if every tunnel were stored under the local key.
	objs := []client.Object{
		readyTunnel("node1", "172.31.0.1", "66:00:00:00:00:01"),
		readyTunnel("node2", "172.31.0.2", "66:00:00:00:00:02"),
		readyTunnel("node3", "172.31.0.3", "66:00:00:00:00:03"),
	}
	cli := fake.NewClientBuilder().WithScheme(schema.GetScheme()).
		WithObjects(objs...).WithStatusSubresource(objs...).Build()

	r := &vxlanReconciler{
		client:  cli,
		log:     logr.Discard(),
		peerMap: utils.NewSyncMap[string, vxlan.Peer](),
		cfg: &config.Config{
			EnvConfig:  config.EnvConfig{NodeName: "node1"},
			FileConfig: config.FileConfig{EnableIPv4: true},
		},
	}

	if err := r.initTunnelPeerMap(); err != nil {
		t.Fatal(err)
	}

	vtep, ok := r.peerMap.Load("node1")
	if !ok {
		t.Fatal("local VTEP missing after init")
	}
	if got := vtep.MAC.String(); got != "66:00:00:00:00:01" {
		t.Fatalf("local VTEP MAC = %s, want 66:00:00:00:00:01", got)
	}
	if got := vtep.IPv4.String(); got != "172.31.0.1" {
		t.Fatalf("local VTEP IPv4 = %s, want 172.31.0.1", got)
	}

	// Peers are left to reconcileEgressTunnel, which also sets Parent and Mark.
	for _, peer := range []string{"node2", "node3"} {
		if _, ok := r.peerMap.Load(peer); ok {
			t.Fatalf("peer %s stored by init", peer)
		}
	}
}

// Without a Ready tunnel of its own the agent must wait, not borrow a peer's.
func TestInitTunnelPeerMapWithoutOwnTunnel(t *testing.T) {
	objs := []client.Object{
		readyTunnel("node2", "172.31.0.2", "66:00:00:00:00:02"),
	}
	cli := fake.NewClientBuilder().WithScheme(schema.GetScheme()).
		WithObjects(objs...).WithStatusSubresource(objs...).Build()

	r := &vxlanReconciler{
		client:  cli,
		log:     logr.Discard(),
		peerMap: utils.NewSyncMap[string, vxlan.Peer](),
		cfg: &config.Config{
			EnvConfig:  config.EnvConfig{NodeName: "node1"},
			FileConfig: config.FileConfig{EnableIPv4: true},
		},
	}

	if err := r.initTunnelPeerMap(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.peerMap.Load("node1"); ok {
		t.Fatal("local VTEP taken from a peer's tunnel")
	}
}
