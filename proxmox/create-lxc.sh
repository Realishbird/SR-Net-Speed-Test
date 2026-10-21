#!/usr/bin/env bash
# Creates the SR-Net-Speed-Test controller LXC on a Proxmox VE node.
#
#   bash -c "$(curl -fsSL https://raw.githubusercontent.com/Realishbird/SR-Net-Speed-Test/main/proxmox/create-lxc.sh)"
#
# Non-interactive example:
#   create-lxc.sh --ctid 200 --storage local-lvm --bridge vmbr0 --password 'ChangeMe123'
# Upgrade the stack inside an existing container:
#   create-lxc.sh --upgrade 200
set -euo pipefail

REPO="Realishbird/SR-Net-Speed-Test"
CTID=""
HOSTNAME_="sr-speedtest"
STORAGE=""
BRIDGE="vmbr0"
VLAN=""
IP="dhcp"
GATEWAY=""
CORES=4
MEMORY=8192
SWAP=1024
DISK=100
PASSWORD=""
VERSION="latest"
UPGRADE=""
TEMPLATE_STORAGE="local"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
usage: create-lxc.sh [options]
  --ctid N           container id (default: next free)
  --hostname NAME    (default: $HOSTNAME_)
  --storage NAME     rootfs storage (default: first storage that holds containers)
  --bridge NAME      (default: $BRIDGE)
  --vlan TAG         VLAN tag for the management network
  --ip CIDR|dhcp     (default: dhcp)
  --gateway IP       required with a static --ip
  --cores N          (default: $CORES)
  --memory MB        (default: $MEMORY)
  --disk GB          (default: $DISK)
  --password PW      admin password for the web/API login (default: prompt, or generated)
  --version vX.Y.Z   release to install (default: latest)
  --upgrade CTID     upgrade the stack in an existing container and exit
EOF
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --ctid) CTID="$2"; shift 2 ;;
    --hostname) HOSTNAME_="$2"; shift 2 ;;
    --storage) STORAGE="$2"; shift 2 ;;
    --bridge) BRIDGE="$2"; shift 2 ;;
    --vlan) VLAN="$2"; shift 2 ;;
    --ip) IP="$2"; shift 2 ;;
    --gateway) GATEWAY="$2"; shift 2 ;;
    --cores) CORES="$2"; shift 2 ;;
    --memory) MEMORY="$2"; shift 2 ;;
    --disk) DISK="$2"; shift 2 ;;
    --password) PASSWORD="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --upgrade) UPGRADE="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "unknown option: $1" >&2; usage ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run as root on the Proxmox node"
command -v pct >/dev/null || die "pct not found; run this on a Proxmox VE node"
PVE_MAJOR=$(pveversion | sed -E 's|pve-manager/([0-9]+).*|\1|')
[ "$PVE_MAJOR" -ge 8 ] || die "Proxmox VE 8 or newer is required (found $(pveversion))"

INSTALLER_URL="https://raw.githubusercontent.com/$REPO/main/deploy/install-controller.sh"

run_installer() {
  local ctid="$1"
  log "Installing the controller stack inside CT $ctid (this takes a few minutes)"
  pct exec "$ctid" -- bash -c "curl -fsSL '$INSTALLER_URL' -o /root/install-controller.sh && chmod +x /root/install-controller.sh"
  pct exec "$ctid" -- env SRNET_VERSION="$VERSION" SRNET_ADMIN_PASSWORD="$PASSWORD" /root/install-controller.sh
}

if [ -n "$UPGRADE" ]; then
  pct status "$UPGRADE" >/dev/null 2>&1 || die "no container $UPGRADE"
  run_installer "$UPGRADE"
  log "Upgrade of CT $UPGRADE complete"
  exit 0
fi

if [ -z "$CTID" ]; then
  CTID=$(pvesh get /cluster/nextid)
fi
pct status "$CTID" >/dev/null 2>&1 && die "container $CTID already exists"

if [ -z "$STORAGE" ]; then
  STORAGE=$(pvesm status --content rootdir 2>/dev/null | awk 'NR>1 && $3=="active"{print $1; exit}')
  [ -n "$STORAGE" ] || die "no storage holds containers; pass --storage"
fi

if [ "$IP" != "dhcp" ] && [ -z "$GATEWAY" ]; then
  die "--gateway is required with a static --ip"
fi

if [ -z "$PASSWORD" ] && [ -t 0 ]; then
  read -r -s -p "Admin password for the speed test web/API (blank = generate): " PASSWORD
  echo
fi
if [ -z "$PASSWORD" ]; then
  PASSWORD=$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16)
  GENERATED=1
else
  GENERATED=0
fi
[ ${#PASSWORD} -ge 8 ] || die "password must be at least 8 characters"

log "Finding the Debian 12 template"
pveam update >/dev/null
TEMPLATE=$(pveam available --section system | awk '/debian-12-standard/{print $2}' | sort -V | tail -1)
[ -n "$TEMPLATE" ] || die "no debian-12-standard template available from pveam"
if ! pveam list "$TEMPLATE_STORAGE" | grep -q "$TEMPLATE"; then
  log "Downloading $TEMPLATE"
  pveam download "$TEMPLATE_STORAGE" "$TEMPLATE"
fi

NET="name=eth0,bridge=$BRIDGE,ip=$IP"
[ "$IP" != "dhcp" ] && NET="$NET,gw=$GATEWAY"
[ -n "$VLAN" ] && NET="$NET,tag=$VLAN"

log "Creating CT $CTID ($HOSTNAME_): $CORES cores, ${MEMORY} MB RAM, ${DISK} GB on $STORAGE, $NET"
pct create "$CTID" "$TEMPLATE_STORAGE:vztmpl/$TEMPLATE" \
  --hostname "$HOSTNAME_" \
  --cores "$CORES" --memory "$MEMORY" --swap "$SWAP" \
  --rootfs "$STORAGE:$DISK" \
  --net0 "$NET" \
  --unprivileged 1 --features nesting=0 \
  --onboot 1 --start 1 \
  --description "SR-Net-Speed-Test controller. Repo: https://github.com/$REPO" \
  --tags srnet >/dev/null

log "Waiting for the container network"
for _ in $(seq 1 30); do
  CT_IP=$(pct exec "$CTID" -- hostname -I 2>/dev/null | awk '{print $1}' || true)
  [ -n "$CT_IP" ] && break
  sleep 2
done
[ -n "$CT_IP" ] || die "container has no IP after 60 s; check the bridge/VLAN/DHCP"
pct exec "$CTID" -- bash -c "apt-get update -qq && apt-get install -y -qq curl ca-certificates >/dev/null"

run_installer "$CTID"

FP=$(pct exec "$CTID" -- sha256sum /etc/srnet/tls/ca.pem | cut -d' ' -f1)
cat <<EOF

================================================================================
 SR-Net-Speed-Test controller is running in CT $CTID ($HOSTNAME_)

   API / agents:  https://$CT_IP:8443
   Grafana:       http://$CT_IP:3000   (admin / admin on first login)
   Admin login:   admin / $PASSWORD$( [ "$GENERATED" -eq 1 ] && echo "   (generated; saved in CT at /etc/srnet/admin-password)")
   CA sha256:     $FP

 Install the agent on each host with a fresh token:
   pct exec $CTID -- srnet token

 Or from inside the container: pct enter $CTID, then 'srnet hosts', 'srnet run --help'.
================================================================================
EOF
