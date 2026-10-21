#!/usr/bin/env bash
# Installs or upgrades the controller stack inside the LXC (Debian 12):
# Postgres, VictoriaMetrics, Grafana, srnet-controller, srnet CLI.
# Run by proxmox/create-lxc.sh via `pct exec`; can also be run by hand as root.
set -euo pipefail

REPO="Realishbird/SR-Net-Speed-Test"
VERSION="${SRNET_VERSION:-latest}"
VM_VERSION="v1.104.0"
GNMIC_VERSION="0.38.2"
PUBLIC_URL="${SRNET_PUBLIC_URL:-}"
ADMIN_PASSWORD="${SRNET_ADMIN_PASSWORD:-}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "run as root"

case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *) die "unsupported architecture" ;;
esac

export DEBIAN_FRONTEND=noninteractive
log "Installing packages"
apt-get update -qq
apt-get install -y -qq curl ca-certificates gnupg apt-transport-https postgresql chrony >/dev/null

log "Installing Grafana (apt repo)"
install -d /etc/apt/keyrings
if [ ! -s /etc/apt/keyrings/grafana.gpg ]; then
  curl -fsSL https://apt.grafana.com/gpg.key | gpg --dearmor -o /etc/apt/keyrings/grafana.gpg
fi
echo "deb [signed-by=/etc/apt/keyrings/grafana.gpg] https://apt.grafana.com stable main" > /etc/apt/sources.list.d/grafana.list
apt-get update -qq
apt-get install -y -qq grafana >/dev/null

log "Installing VictoriaMetrics $VM_VERSION"
if [ ! -x /usr/local/bin/victoria-metrics ] || ! /usr/local/bin/victoria-metrics -version 2>&1 | grep -q "${VM_VERSION#v}"; then
  TMP=$(mktemp -d)
  curl -fsSL -o "$TMP/vm.tar.gz" "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/$VM_VERSION/victoria-metrics-linux-$ARCH-$VM_VERSION.tar.gz"
  tar -xzf "$TMP/vm.tar.gz" -C "$TMP"
  install -m 0755 "$TMP/victoria-metrics-prod" /usr/local/bin/victoria-metrics
  rm -rf "$TMP"
fi
getent passwd victoriametrics >/dev/null || useradd --system --home-dir /var/lib/victoriametrics --shell /usr/sbin/nologin victoriametrics
install -d -o victoriametrics -g victoriametrics /var/lib/victoriametrics
cat > /etc/systemd/system/victoriametrics.service <<'EOF'
[Unit]
Description=VictoriaMetrics
After=network.target

[Service]
User=victoriametrics
ExecStart=/usr/local/bin/victoria-metrics -storageDataPath=/var/lib/victoriametrics -retentionPeriod=90d -httpListenAddr=127.0.0.1:8428
Restart=always

[Install]
WantedBy=multi-user.target
EOF

log "Installing gnmic $GNMIC_VERSION (gNMI collector for SONiC switches)"
if [ ! -x /usr/local/bin/gnmic ] || ! /usr/local/bin/gnmic version 2>/dev/null | grep -q "$GNMIC_VERSION"; then
  TMP=$(mktemp -d)
  GNMIC_ARCH=x86_64
  [ "$ARCH" = arm64 ] && GNMIC_ARCH=arm64
  curl -fsSL -o "$TMP/gnmic.tar.gz" "https://github.com/openconfig/gnmic/releases/download/v$GNMIC_VERSION/gnmic_${GNMIC_VERSION}_linux_$GNMIC_ARCH.tar.gz"
  tar -xzf "$TMP/gnmic.tar.gz" -C "$TMP" gnmic
  install -m 0755 "$TMP/gnmic" /usr/local/bin/gnmic
  rm -rf "$TMP"
fi
cat > /etc/systemd/system/gnmic.service <<'EOF'
[Unit]
Description=gnmic gNMI collector (SR-Net-Speed-Test switches)
After=network-online.target victoriametrics.service
ConditionPathExists=/etc/srnet/gnmic.yaml

[Service]
User=srnet
ExecStart=/usr/local/bin/gnmic --config /etc/srnet/gnmic.yaml subscribe
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

# The controller runs as the srnet user and cannot restart services, so a path
# unit restarts gnmic whenever the controller rewrites its config (switches
# added or removed from the UI).
cat > /etc/systemd/system/gnmic-restart.service <<'EOF'
[Unit]
Description=Restart gnmic after its config changed

[Service]
Type=oneshot
ExecStart=/bin/systemctl restart gnmic.service
EOF
cat > /etc/systemd/system/gnmic-restart.path <<'EOF'
[Unit]
Description=Watch the gnmic config written by srnet-controller

[Path]
PathChanged=/etc/srnet/gnmic.yaml
Unit=gnmic-restart.service

[Install]
WantedBy=multi-user.target
EOF

log "Installing srnet-controller and srnet CLI ($VERSION)"
if [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$VERSION"
fi
TMP=$(mktemp -d)
for bin in srnet-controller srnet; do
  curl -fsSL -o "$TMP/$bin" "$BASE/$bin-linux-$ARCH" || die "download failed: $BASE/$bin-linux-$ARCH (is there a release yet?)"
done
curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"
for bin in srnet-controller srnet; do
  (cd "$TMP" && grep " $bin-linux-$ARCH\$" SHA256SUMS | sed "s/$bin-linux-$ARCH/$bin/" | sha256sum -c --quiet -) || die "checksum mismatch for $bin"
  install -m 0755 "$TMP/$bin" "/usr/local/bin/$bin"
done
# Mirror the agent binaries so hosts can also install from this LXC later.
install -d /var/lib/srnet/releases
for a in amd64 arm64; do
  curl -fsSL -o "/var/lib/srnet/releases/srnet-agent-linux-$a" "$BASE/srnet-agent-linux-$a" || true
done
cp "$TMP/SHA256SUMS" /var/lib/srnet/releases/SHA256SUMS
rm -rf "$TMP"

getent group srnet >/dev/null || groupadd --system srnet
getent passwd srnet >/dev/null || useradd --system --gid srnet --home-dir /var/lib/srnet --shell /usr/sbin/nologin srnet
install -d -m 0750 -o srnet -g srnet /etc/srnet /var/lib/srnet
chown -R srnet:srnet /var/lib/srnet

log "Preparing Postgres"
systemctl enable --now postgresql >/dev/null
runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='srnet'" | grep -q 1 || runuser -u postgres -- createuser srnet
runuser -u postgres -- psql -tAc "SELECT 1 FROM pg_database WHERE datname='srnet'" | grep -q 1 || runuser -u postgres -- createdb -O srnet srnet

cat > /etc/systemd/system/srnet-controller.service <<'EOF'
[Unit]
Description=SR-Net-Speed-Test controller
After=network-online.target postgresql.service victoriametrics.service
Wants=network-online.target
Requires=postgresql.service

[Service]
Type=simple
User=srnet
Group=srnet
ExecStart=/usr/local/bin/srnet-controller serve
Restart=always
RestartSec=5
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
StateDirectory=srnet
ConfigurationDirectory=srnet

[Install]
WantedBy=multi-user.target
EOF

if [ ! -s /etc/srnet/controller.json ]; then
  [ -n "$PUBLIC_URL" ] || PUBLIC_URL="https://$(hostname -I | awk '{print $1}'):8443"
  if [ -z "$ADMIN_PASSWORD" ]; then
    ADMIN_PASSWORD=$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 16)
  fi
  log "Initialising controller config and CA for $PUBLIC_URL"
  umask 077
  printf '%s\n' "$ADMIN_PASSWORD" > /etc/srnet/admin-password
  chown srnet:srnet /etc/srnet/admin-password
  HOSTS="$(hostname),$(hostname -I | tr ' ' ',' | sed 's/,$//')"
  runuser -u srnet -- /usr/local/bin/srnet-controller init --public-url "$PUBLIC_URL" --cert-hosts "$HOSTS" --admin-password-file /etc/srnet/admin-password
  umask 022
fi

# Grafana's unit enables systemd sandboxing (ProtectSystem, PrivateTmp,
# ProtectProc ...) which needs a private mount namespace. An unprivileged LXC
# cannot create one, so the service dies with status=226/NAMESPACE. Clear
# those options; the container itself is the sandbox.
log "Grafana systemd override for unprivileged containers"
install -d /etc/systemd/system/grafana-server.service.d
cat > /etc/systemd/system/grafana-server.service.d/lxc.conf <<'EOF'
[Service]
ProtectSystem=false
ProtectHome=false
PrivateTmp=false
PrivateDevices=false
ProtectProc=default
ProcSubset=all
ProtectKernelTunables=false
ProtectKernelModules=false
ProtectKernelLogs=false
ProtectControlGroups=false
ProtectClock=false
ProtectHostname=false
RestrictNamespaces=false
LockPersonality=false
RestrictRealtime=false
RestrictSUIDSGID=false
RemoveIPC=false
NoNewPrivileges=false
EOF
systemctl daemon-reload
systemctl reset-failed grafana-server 2>/dev/null || true

log "Grafana data source and dashboards"
install -d /etc/grafana/provisioning/datasources /etc/grafana/provisioning/dashboards
cat > /etc/grafana/provisioning/datasources/srnet.yaml <<'EOF'
apiVersion: 1
datasources:
  - name: VictoriaMetrics
    uid: srnet-vm
    type: prometheus
    access: proxy
    url: http://127.0.0.1:8428
    isDefault: true
EOF
cat > /etc/grafana/provisioning/dashboards/srnet.yaml <<'EOF'
apiVersion: 1
providers:
  - name: srnet
    folder: SR-Net
    type: file
    allowUiUpdates: true
    options:
      path: /var/lib/srnet/grafana
EOF
install -d -m 0755 -o srnet -g srnet /var/lib/srnet/grafana
# Re-running init refreshes the bundled dashboards without touching other settings.
if [ -s /etc/srnet/controller.json ]; then
  PUB=$(python3 -c 'import json;print(json.load(open("/etc/srnet/controller.json"))["public_url"])' 2>/dev/null || true)
  [ -n "$PUB" ] && runuser -u srnet -- /usr/local/bin/srnet-controller init --public-url "$PUB" --admin-password-file /etc/srnet/admin-password >/dev/null 2>&1 || true
fi

systemctl daemon-reload
systemctl enable --now victoriametrics grafana-server srnet-controller >/dev/null
systemctl enable gnmic >/dev/null 2>&1 || true
systemctl enable --now gnmic-restart.path >/dev/null 2>&1 || true
systemctl restart srnet-controller
systemctl restart gnmic 2>/dev/null || true

sleep 2
log "Controller status"
echo "  srnet-controller: $(systemctl is-active srnet-controller)"
echo "  victoriametrics:  $(systemctl is-active victoriametrics)"
echo "  grafana:          $(systemctl is-active grafana-server) (port 3000, login admin/admin on first visit)"
echo "  postgresql:       $(systemctl is-active postgresql)"
echo "  gnmic:            $(systemctl is-active gnmic) (idle until a switch with gNMI is added on the Switches page)"
echo "  web UI:           https://<this host>:8443 (admin login)"
for svc in srnet-controller victoriametrics grafana-server postgresql; do
  if [ "$(systemctl is-active "$svc")" != "active" ]; then
    echo
    echo "==> $svc is not running; last log lines:"
    journalctl -u "$svc" -n 25 --no-pager 2>/dev/null | sed 's/^/    /'
    echo "    (fix the cause, then: systemctl restart $svc)"
  fi
done
