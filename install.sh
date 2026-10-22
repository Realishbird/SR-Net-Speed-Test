#!/usr/bin/env bash
# SR-Net-Speed-Test agent installer for Ubuntu 22.04/24.04 (including DGX OS) and Debian 12.
#
#   curl -fsSL https://raw.githubusercontent.com/Realishbird/SR-Net-Speed-Test/main/install.sh \
#     | sudo bash -s -- --controller https://10.0.0.5:8443 --token <token> --ca-sha256 <fingerprint>
#
# The controller prints the full command, with a fresh token, from `srnet token`.
# Re-running upgrades the agent in place and keeps its enrollment.
set -euo pipefail

REPO="Realishbird/SR-Net-Speed-Test"
CONTROLLER=""
TOKEN=""
CA_SHA=""
VERSION="latest"
WITH_MFT=0
UNINSTALL=0
SOURCE="auto"
SKIP_APT=0

usage() {
  cat <<EOF
usage: install.sh --controller URL [--token TOKEN] --ca-sha256 HEX [--version vX.Y.Z] [--source auto|github|controller] [--skip-apt] [--with-mft]
       install.sh --uninstall

  --controller   controller URL, e.g. https://10.0.0.5:8443
  --token        enrollment token from the Hosts page or 'srnet token'; may be left out when
                 this host's subnet is listed under Settings > Enrolment on the controller
  --ca-sha256    sha256 of the controller's CA certificate (pins the CA on first contact)
  --version      agent release to install (default: latest)
  --source       where the agent binary comes from: github (releases), controller (a release
                 mirrored on the controller, for hosts without internet), auto (controller
                 if it has one, else github)
  --skip-apt     do not install packages (an air-gapped host with rdma-core, perftest,
                 iperf3, ethtool, lldpd and chrony already present)
  --with-mft     also note Mellanox MFT (not installed; printed as a hint)
  --uninstall    stop and remove the agent, its user and config
EOF
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --controller) CONTROLLER="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --ca-sha256) CA_SHA="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --source) SOURCE="$2"; shift 2 ;;
    --skip-apt) SKIP_APT=1; shift ;;
    --with-mft) WITH_MFT=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help) usage ;;
    *) echo "unknown option: $1" >&2; usage ;;
  esac
done

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo)"

if [ "$UNINSTALL" -eq 1 ]; then
  log "Removing srnet-agent"
  systemctl disable --now srnet-agent srnet-netplan.path srnet-netplan.timer 2>/dev/null || true
  rm -f /etc/systemd/system/srnet-agent.service /usr/local/bin/srnet-agent /usr/local/bin/srnet-agent-launch /etc/sudoers.d/srnet-ethtool /etc/sudoers.d/srnet-lldp
  rm -f /etc/systemd/system/srnet-netplan.service /etc/systemd/system/srnet-netplan.path /etc/systemd/system/srnet-netplan.timer
  rm -rf /etc/srnet /var/lib/srnet /var/lib/srnet-netplan
  # /etc/netplan/90-srnet.yaml stays: it holds the RDMA ports' addresses.
  userdel srnet 2>/dev/null || true
  systemctl daemon-reload
  log "Done"
  exit 0
fi

# Read os-release in a subshell: sourcing it directly would overwrite $VERSION
# (the agent release to install) with the OS version string.
OS_ID=$(. /etc/os-release && echo "${ID:-}")
OS_VERSION_ID=$(. /etc/os-release && echo "${VERSION_ID:-}")
OS_PRETTY=$(. /etc/os-release && echo "${PRETTY_NAME:-unknown}")
case "$OS_ID:$OS_VERSION_ID" in
  ubuntu:22.04|ubuntu:24.04|debian:12) ;;
  *) die "unsupported OS: $OS_PRETTY. Supported: Ubuntu 22.04, Ubuntu 24.04, Debian 12" ;;
esac

case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

ENROLLED=0
[ -s /etc/srnet/agent.json ] && grep -q '"secret": *"[^"]' /etc/srnet/agent.json && ENROLLED=1
if [ "$ENROLLED" -eq 0 ]; then
  [ -n "$CONTROLLER" ] && [ -n "$CA_SHA" ] || die "--controller and --ca-sha256 are required for a first install (--token too, unless this subnet may enrol without one)"
fi
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# The controller CA is fetched first and pinned by fingerprint, so downloads
# from the controller (--source controller) are verified too.
CACERT=""
if [ -n "$CONTROLLER" ] && [ -n "$CA_SHA" ]; then
  log "Fetching and pinning the controller CA"
  curl -fsSk -o "$TMP/ca.pem" "$CONTROLLER/ca.pem" || die "cannot reach $CONTROLLER/ca.pem"
  GOT=$(sha256sum "$TMP/ca.pem" | cut -d' ' -f1)
  [ "$GOT" = "$CA_SHA" ] || die "CA fingerprint mismatch: expected $CA_SHA, got $GOT. Check the controller URL."
  CACERT="$TMP/ca.pem"
fi

if [ "$SKIP_APT" -eq 0 ]; then
  log "Installing packages (rdma-core, perftest, iperf3, ethtool, chrony)"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq rdma-core ibverbs-utils perftest iperf3 ethtool pciutils chrony curl ca-certificates lldpd >/dev/null
else
  log "Skipping packages (--skip-apt)"
  for t in ib_write_bw iperf3 ethtool lldpd; do command -v "$t" >/dev/null 2>&1 || echo "  warning: $t is not installed" ; done
fi
systemctl enable --now chrony >/dev/null 2>&1 || true
systemctl enable --now lldpd >/dev/null 2>&1 || true

# Where the binary comes from. The controller serves releases it has
# mirrored (Fleet page) at /dl/<version>/<file>, taking the install token;
# "auto" prefers it when it has a release and falls back to GitHub.
fetch_from_controller() {
  [ -n "$CONTROLLER" ] && [ -n "$CACERT" ] || return 1
  curl -fsS --cacert "$CACERT" -H "Authorization: Bearer $TOKEN" -o "$TMP/SHA256SUMS" "$CONTROLLER/dl/$VERSION/SHA256SUMS" 2>/dev/null || return 1
  curl -fsS --cacert "$CACERT" -H "Authorization: Bearer $TOKEN" -o "$TMP/srnet-agent" "$CONTROLLER/dl/$VERSION/srnet-agent-linux-$ARCH" || return 1
  echo "$CONTROLLER/dl/$VERSION"
}
fetch_from_github() {
  if [ "$VERSION" = "latest" ]; then
    BASE="https://github.com/$REPO/releases/latest/download"
  else
    BASE="https://github.com/$REPO/releases/download/$VERSION"
  fi
  curl -fsSL -o "$TMP/srnet-agent" "$BASE/srnet-agent-linux-$ARCH" || die "download failed: $BASE/srnet-agent-linux-$ARCH (is there a release yet?)"
  curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS" || die "download failed: $BASE/SHA256SUMS"
  echo "$BASE"
}
log "Downloading srnet-agent ($VERSION, $ARCH, source $SOURCE)"
case "$SOURCE" in
  controller) FROM=$(fetch_from_controller) || die "the controller has no mirrored release $VERSION for $ARCH (Fleet page: Mirror a release or Upload release)" ;;
  github) FROM=$(fetch_from_github) ;;
  auto) FROM=$(fetch_from_controller) || FROM=$(fetch_from_github) ;;
  *) die "--source must be auto, github or controller" ;;
esac
log "Got srnet-agent from $FROM"
(cd "$TMP" && grep " srnet-agent-linux-$ARCH\$" SHA256SUMS | sed "s/srnet-agent-linux-$ARCH/srnet-agent/" | sha256sum -c --quiet -) || die "checksum mismatch for srnet-agent"
install -m 0755 "$TMP/srnet-agent" /usr/local/bin/srnet-agent

# The launcher lets the controller push updates: the agent stages a verified
# binary as srnet-agent.next, exits, and the launcher swaps it in on restart.
# If the new binary never reports healthy, the next start restores .prev.
cat > /usr/local/bin/srnet-agent-launch <<'EOF'
#!/usr/bin/env bash
set -u
BIN=/var/lib/srnet/bin
mkdir -p "$BIN"
if [ -x "$BIN/srnet-agent.next" ]; then
  [ -x "$BIN/srnet-agent" ] && mv -f "$BIN/srnet-agent" "$BIN/srnet-agent.prev"
  mv -f "$BIN/srnet-agent.next" "$BIN/srnet-agent"
  touch "$BIN/trial"
elif [ -e "$BIN/trial" ] && [ -x "$BIN/srnet-agent.prev" ]; then
  echo "srnet-agent-launch: previous update did not become healthy; rolling back" >&2
  mv -f "$BIN/srnet-agent.prev" "$BIN/srnet-agent"
  rm -f "$BIN/trial"
elif [ -e "$BIN/trial" ]; then
  rm -f "$BIN/trial" "$BIN/srnet-agent"  # fall back to the package-installed binary
fi
if [ -x "$BIN/srnet-agent" ]; then
  exec "$BIN/srnet-agent" run
fi
exec /usr/local/bin/srnet-agent run
EOF
chmod 0755 /usr/local/bin/srnet-agent-launch

log "Creating srnet user and directories"
getent group srnet >/dev/null || groupadd --system srnet
getent passwd srnet >/dev/null || useradd --system --gid srnet --home-dir /var/lib/srnet --shell /usr/sbin/nologin srnet
install -d -m 0750 -o srnet -g srnet /etc/srnet /var/lib/srnet /var/lib/srnet/bin
rm -f /var/lib/srnet/bin/srnet-agent /var/lib/srnet/bin/srnet-agent.next /var/lib/srnet/bin/srnet-agent.prev /var/lib/srnet/bin/trial

# Earlier versions wrote sudo rules for ethtool -m and lldpctl. They never
# worked (sudo cannot change user under the unit's capability bounding set)
# and only filled auth.log; CAP_NET_ADMIN covers ethtool -m and group adm
# covers lldpctl, so the rules go.
rm -f /etc/sudoers.d/srnet-ethtool /etc/sudoers.d/srnet-lldp

cat > /etc/systemd/system/srnet-agent.service <<'EOF'
[Unit]
Description=SR-Net-Speed-Test agent
Documentation=https://github.com/Realishbird/SR-Net-Speed-Test
After=network-online.target chrony.service
Wants=network-online.target

[Service]
Type=simple
User=srnet
Group=srnet
# lldpcli is executable by group adm only; sudo cannot be used from here
# because the capability bounding set below leaves it unable to change group.
SupplementaryGroups=adm
ExecStart=/usr/local/bin/srnet-agent-launch
Restart=always
RestartSec=5
LimitMEMLOCK=infinity
AmbientCapabilities=CAP_NET_ADMIN CAP_IPC_LOCK CAP_SYS_NICE
CapabilityBoundingSet=CAP_NET_ADMIN CAP_IPC_LOCK CAP_SYS_NICE
NoNewPrivileges=false
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/srnet
StateDirectory=srnet
ConfigurationDirectory=srnet

[Install]
WantedBy=multi-user.target
EOF

# The netplan helper lets the controller's Hosts page change the addresses
# and MTU of RDMA ports. The agent never gets root: it leaves a request in
# /var/lib/srnet/netplan, and this root service, running the root-owned
# binary above (never the one fleet updates stage under /var/lib/srnet),
# checks it, refuses the management port, applies it with netplan and puts
# the old files back unless the port comes up, a fabric peer answers and
# the agent still reaches the controller. The timer refreshes what the
# agent reports about each port's netplan setup.
log "Installing the netplan helper"
install -d -m 0750 -o srnet -g srnet /var/lib/srnet/netplan
install -d -m 0755 -o root -g root /var/lib/srnet-netplan
cat > /etc/systemd/system/srnet-netplan.service <<'EOF'
[Unit]
Description=SR-Net-Speed-Test netplan helper (RDMA port network changes, with rollback)
Documentation=https://github.com/Realishbird/SR-Net-Speed-Test

[Service]
Type=oneshot
ExecStart=/usr/local/bin/srnet-agent netplan-helper
TimeoutStartSec=15min
EOF
cat > /etc/systemd/system/srnet-netplan.path <<'EOF'
[Unit]
Description=Watch for RDMA port network changes from the srnet agent

[Path]
PathExists=/var/lib/srnet/netplan/request.json
Unit=srnet-netplan.service

[Install]
WantedBy=multi-user.target
EOF
cat > /etc/systemd/system/srnet-netplan.timer <<'EOF'
[Unit]
Description=Refresh the netplan setup the srnet agent reports

[Timer]
OnBootSec=1min
OnUnitActiveSec=5min
Unit=srnet-netplan.service

[Install]
WantedBy=timers.target
EOF
command -v netplan >/dev/null 2>&1 || log "netplan is not installed here: the Hosts page shows each port's setup and offers the netplan to copy"

if [ "$ENROLLED" -eq 0 ]; then
  install -m 0644 -o srnet -g srnet "$CACERT" /etc/srnet/ca.pem
  if [ -n "$TOKEN" ]; then
    log "Enrolling with $CONTROLLER"
  else
    log "Enrolling with $CONTROLLER without a token (this host's subnet must be allowed under Settings > Enrolment)"
  fi
  runuser -u srnet -- /usr/local/bin/srnet-agent enroll --controller "$CONTROLLER" --token "$TOKEN" --ca /etc/srnet/ca.pem --config /etc/srnet/agent.json
fi

systemctl daemon-reload
systemctl enable --now srnet-agent >/dev/null
systemctl enable --now srnet-netplan.path srnet-netplan.timer >/dev/null
systemctl start --no-block srnet-netplan.service
systemctl restart srnet-agent

log "Self-check"
sleep 2
PORTS=$(runuser -u srnet -- /usr/local/bin/srnet-agent inventory 2>/dev/null | grep -c '"rdma_dev"' || true)
echo "  agent version: $(/usr/local/bin/srnet-agent version)"
echo "  service:       $(systemctl is-active srnet-agent)"
echo "  RDMA ports:    $PORTS"
echo "  clock:         $(chronyc tracking 2>/dev/null | awk -F': ' '/System time/{print $2}' || echo unknown)"
if [ "$PORTS" -eq 0 ]; then
  echo "  warning: no RDMA devices found. Check 'rdma link show'."
fi
if [ "$WITH_MFT" -eq 1 ]; then
  echo "  note: Mellanox MFT is not packaged in apt; install it from NVIDIA for mlxlink temperature data."
fi
log "Done. The host should now appear in 'srnet hosts' on the controller."
