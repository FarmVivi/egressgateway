// Copyright 2022 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package agent

import (
	"strings"
	"testing"
)

// TestBuildNatStaticRule_PostroutingAcceptUsesByteMask verifies that the
// POSTROUTING ACCEPT rule uses Mark (0xff000000) as mask, not Mask (0xffffffff).
//
// With Mask=0xffffffff the rule only matches when the packet mark equals base
// exactly (e.g. 0x26000000). In cross-node scenarios the agent stamps a
// per-gateway-node mark (e.g. 0x26577c9e), so the rule never fires and the
// packet falls through to the CNI MASQUERADE — breaking SNAT on the gateway
// node.  Using Mark=0xff000000 correctly matches the high byte only.
func TestBuildNatStaticRule_PostroutingAcceptUsesByteMask(t *testing.T) {
	const base uint32 = 0x26000000

	rules := buildNatStaticRule(base)

	postrouting, ok := rules["POSTROUTING"]
	if !ok {
		t.Fatal("POSTROUTING chain missing from buildNatStaticRule result")
	}
	if len(postrouting) < 2 {
		t.Fatalf("expected at least 2 POSTROUTING rules, got %d", len(postrouting))
	}

	// Rule at index 1 is the ACCEPT that bypasses CNI MASQUERADE.
	acceptRule := postrouting[1]
	rendered := acceptRule.Match.Render()

	// Must match with the byte-level mask (Mark), not the full-word mask (Mask).
	wantMask := "0xff000000"
	badMask := "0xffffffff"

	if strings.Contains(rendered, badMask) {
		t.Errorf("POSTROUTING ACCEPT rule uses full mask %s — cross-node marks will not match; got: %s", badMask, rendered)
	}
	if !strings.Contains(rendered, wantMask) {
		t.Errorf("POSTROUTING ACCEPT rule missing byte mask %s; got: %s", wantMask, rendered)
	}
}
