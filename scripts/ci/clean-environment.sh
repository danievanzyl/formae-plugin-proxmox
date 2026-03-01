#!/bin/bash
set -euo pipefail

TEST_PREFIX="${TEST_PREFIX:-formae-plugin-sdk-test-}"
PROXMOX_URL="${PROXMOX_URL:-https://localhost:8006}"
PROXMOX_NODE="${PROXMOX_NODE:-pve}"
PROXMOX_API_TOKEN="${PROXMOX_API_TOKEN:-}"

echo "clean-environment.sh: Cleaning test VMs/containers with prefix '${TEST_PREFIX}'"

if [ -z "$PROXMOX_API_TOKEN" ]; then
    echo "PROXMOX_API_TOKEN not set, skipping cleanup"
    exit 0
fi

AUTH="Authorization: PVEAPIToken=${PROXMOX_API_TOKEN}"
API="${PROXMOX_URL}/api2/json"

# Clean VMs
echo "Listing VMs on node ${PROXMOX_NODE}..."
VMS=$(curl -sk -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/qemu" 2>/dev/null | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin).get('data', [])
    for vm in data:
        if vm.get('name', '').startswith('${TEST_PREFIX}'):
            print(vm['vmid'])
except: pass
" 2>/dev/null || true)

for VMID in $VMS; do
    echo "  Stopping and deleting VM ${VMID}..."
    curl -sk -X POST -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/qemu/${VMID}/status/stop" 2>/dev/null || true
    sleep 2
    curl -sk -X DELETE -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/qemu/${VMID}?purge=1&destroy-unreferenced-disks=1" 2>/dev/null || true
done

# Clean containers
echo "Listing containers on node ${PROXMOX_NODE}..."
CTS=$(curl -sk -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/lxc" 2>/dev/null | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin).get('data', [])
    for ct in data:
        if ct.get('name', '').startswith('${TEST_PREFIX}'):
            print(ct['vmid'])
except: pass
" 2>/dev/null || true)

for VMID in $CTS; do
    echo "  Stopping and deleting container ${VMID}..."
    curl -sk -X POST -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/lxc/${VMID}/status/stop" 2>/dev/null || true
    sleep 2
    curl -sk -X DELETE -H "$AUTH" "${API}/nodes/${PROXMOX_NODE}/lxc/${VMID}?purge=1&force=1" 2>/dev/null || true
done

echo "clean-environment.sh: Cleanup complete"
