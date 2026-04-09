package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDrainSafety_RefusesStopWhenVMsUnaccounted simulates the VM safety check
// that runs before node stop. When the target node does not have all VMs that
// were on the source, the drain must refuse to stop the node.
func TestDrainSafety_RefusesStopWhenVMsUnaccounted(t *testing.T) {
	// Target node only has vm-1, but source had vm-1 and vm-2
	targetNode := newSimNode(true, true)
	targetNode.addVM("vm-1", "running")
	// vm-2 is NOT on the target — simulates failed migration
	targetSrv := httptest.NewServer(targetNode.handler())
	defer targetSrv.Close()

	// Original VMs that were on the source node
	sourceVMs := []map[string]interface{}{
		{"name": "vm-1", "status": "running", "ram_mib": float64(2048)},
		{"name": "vm-2", "status": "running", "ram_mib": float64(2048)},
	}

	// Simulate the safety check logic from drainNode
	safetyClient := &http.Client{Timeout: 5 * time.Second}
	var unaccountedVMs []string
	for _, vm := range sourceVMs {
		vmName, _ := vm["name"].(string)
		found := false
		checkResp, err := safetyClient.Get(targetSrv.URL + "/api/vms/" + vmName)
		if err == nil {
			var detail map[string]interface{}
			json.NewDecoder(checkResp.Body).Decode(&detail)
			checkResp.Body.Close()
			if status, _ := detail["status"].(string); status == "running" || status == "stopped" {
				found = true
			}
		}
		if !found {
			unaccountedVMs = append(unaccountedVMs, vmName)
		}
	}

	if len(unaccountedVMs) == 0 {
		t.Fatal("safety check should have found unaccounted VMs")
	}
	if len(unaccountedVMs) != 1 {
		t.Errorf("expected 1 unaccounted VM, got %d: %v", len(unaccountedVMs), unaccountedVMs)
	}
	if unaccountedVMs[0] != "vm-2" {
		t.Errorf("unaccounted VM = %q, want vm-2", unaccountedVMs[0])
	}
}

// TestDrainSafety_AllVMsAccountedFor verifies the safety check passes when
// every VM from the source is found on the target.
func TestDrainSafety_AllVMsAccountedFor(t *testing.T) {
	targetNode := newSimNode(true, true)
	targetNode.addVM("vm-1", "running")
	targetNode.addVM("vm-2", "running")
	targetNode.addVM("vm-3", "stopped") // stopped VMs also count as accounted for
	targetSrv := httptest.NewServer(targetNode.handler())
	defer targetSrv.Close()

	sourceVMs := []map[string]interface{}{
		{"name": "vm-1", "status": "running", "ram_mib": float64(2048)},
		{"name": "vm-2", "status": "running", "ram_mib": float64(1024)},
		{"name": "vm-3", "status": "stopped", "ram_mib": float64(512)},
	}

	safetyClient := &http.Client{Timeout: 5 * time.Second}
	var unaccountedVMs []string
	for _, vm := range sourceVMs {
		vmName, _ := vm["name"].(string)
		found := false
		checkResp, err := safetyClient.Get(targetSrv.URL + "/api/vms/" + vmName)
		if err == nil {
			var detail map[string]interface{}
			json.NewDecoder(checkResp.Body).Decode(&detail)
			checkResp.Body.Close()
			if status, _ := detail["status"].(string); status == "running" || status == "stopped" {
				found = true
			}
		}
		if !found {
			unaccountedVMs = append(unaccountedVMs, vmName)
		}
	}

	if len(unaccountedVMs) != 0 {
		t.Errorf("all VMs should be accounted for, but got unaccounted: %v", unaccountedVMs)
	}
}

// TestDrainSafety_TargetUnreachableRefusesStop verifies that if the target
// node is unreachable during safety check, all VMs are treated as unaccounted.
func TestDrainSafety_TargetUnreachableRefusesStop(t *testing.T) {
	// Create and immediately close server to simulate unreachable target
	targetSrv := httptest.NewServer(http.NotFoundHandler())
	targetSrv.Close()

	sourceVMs := []map[string]interface{}{
		{"name": "vm-1", "status": "running", "ram_mib": float64(2048)},
	}

	safetyClient := &http.Client{Timeout: 1 * time.Second}
	var unaccountedVMs []string
	for _, vm := range sourceVMs {
		vmName, _ := vm["name"].(string)
		found := false
		checkResp, err := safetyClient.Get(targetSrv.URL + "/api/vms/" + vmName)
		if err == nil {
			var detail map[string]interface{}
			json.NewDecoder(checkResp.Body).Decode(&detail)
			checkResp.Body.Close()
			if status, _ := detail["status"].(string); status == "running" || status == "stopped" {
				found = true
			}
		}
		if !found {
			unaccountedVMs = append(unaccountedVMs, vmName)
		}
	}

	if len(unaccountedVMs) != 1 {
		t.Errorf("unreachable target should mean all VMs unaccounted, got %d unaccounted", len(unaccountedVMs))
	}
}

// TestDrainSafety_MigratingStatusNotAccountedFor verifies that a VM with
// "migrating" status on the target is NOT considered accounted for — only
// "running" or "stopped" count as successfully landed.
func TestDrainSafety_MigratingStatusNotAccountedFor(t *testing.T) {
	targetNode := newSimNode(true, true)
	targetNode.addVM("vm-1", "running")
	targetNode.addVM("vm-2", "migrating") // still in-flight, not safely landed
	targetSrv := httptest.NewServer(targetNode.handler())
	defer targetSrv.Close()

	sourceVMs := []map[string]interface{}{
		{"name": "vm-1", "status": "running", "ram_mib": float64(2048)},
		{"name": "vm-2", "status": "running", "ram_mib": float64(2048)},
	}

	safetyClient := &http.Client{Timeout: 5 * time.Second}
	var unaccountedVMs []string
	for _, vm := range sourceVMs {
		vmName, _ := vm["name"].(string)
		found := false
		checkResp, err := safetyClient.Get(targetSrv.URL + "/api/vms/" + vmName)
		if err == nil {
			var detail map[string]interface{}
			json.NewDecoder(checkResp.Body).Decode(&detail)
			checkResp.Body.Close()
			if status, _ := detail["status"].(string); status == "running" || status == "stopped" {
				found = true
			}
		}
		if !found {
			unaccountedVMs = append(unaccountedVMs, vmName)
		}
	}

	if len(unaccountedVMs) != 1 {
		t.Errorf("expected 1 unaccounted VM (migrating), got %d: %v", len(unaccountedVMs), unaccountedVMs)
	}
	if len(unaccountedVMs) > 0 && unaccountedVMs[0] != "vm-2" {
		t.Errorf("unaccounted VM = %q, want vm-2", unaccountedVMs[0])
	}
}
