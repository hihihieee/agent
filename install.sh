#!/usr/bin/env bash
set -Eeuo pipefail

readonly APP=palm-agent
readonly BIN=/usr/local/bin/palm-agent
readonly ENV_FILE=/etc/palm-agent.env
readonly UNIT_FILE=/etc/systemd/system/palm-agent.service
readonly STATE_DIR=/var/lib/palm-agent
readonly LIB_DIR=/usr/local/lib/palm-agent
readonly LEGACY_BIN=/usr/local/bin/palm-iplog
readonly LEGACY_AWK=/usr/local/bin/palm-iplog-parse.awk
readonly LEGACY_STATS_BIN=/usr/local/bin/palm-iplog-stats
readonly LEGACY_ENV_FILE=/etc/palm-iplog.env
readonly LEGACY_UNIT_FILE=/etc/systemd/system/palm-iplog.service
REPO="${PALM_AGENT_REPO:-hihihieee/agent}"
VERSION="${PALM_AGENT_VERSION:-latest}"
ACTION="${1:-install}"
[ "$#" -gt 0 ] && shift || true
SECRET=""; ENDPOINT=""; NODE=""; LOG_PATH=""; NETWORK_INTERFACE=""; WINDOW=""; INTERVAL=""; STATS_INTERVAL=""
BUILD_FROM_SOURCE=0; PURGE=0; NON_INTERACTIVE=0

green='\033[32m'; red='\033[31m'; yellow='\033[33m'; bold='\033[1m'; reset='\033[0m'
ok(){ printf "${green}[ok]${reset} %s\n" "$*"; }
info(){ printf "${bold}->${reset} %s\n" "$*"; }
warn(){ printf "${yellow}!${reset} %s\n" "$*"; }
die(){ printf "${red}[error]${reset} %s\n" "$*" >&2; exit 1; }
usage(){ cat <<'EOF'
Palm Agent installer

  install.sh install --secret SECRET --endpoint URL --node NODE [options]
  install.sh update [--version TAG]
  install.sh doctor | status | logs
  install.sh uninstall [--purge]

Options:
  --secret VALUE          Permanent HMAC secret (stored only in /etc/palm-agent.env)
  --secret-file PATH      Read the secret without placing it in shell history
  --endpoint URL          Example: https://connect.example.com/limiter/ingest
  --node NAME             Node key used by the control plane
  --log PATH              Xray access log; auto-detected by default
  --interface NAME        Network interface; default route by default
  --window SEC            Active-IP window (default 90)
  --interval SEC          Ingest interval (default 20)
  --stats-interval SEC    Metrics interval; 0 disables metrics (default 15)
  --repo OWNER/REPO       Public GitHub repository (or PALM_AGENT_REPO)
  --version TAG           Release tag; default latest
  --build-from-source     Download Go toolchain, build, then remove it
  --non-interactive       Never prompt
  --purge                 Remove configuration during uninstall

The repository contains no endpoint, node name, or secret.
EOF
}

case "$ACTION" in -h|--help) usage; exit 0;; esac

need_arg(){ [ "$#" -ge 2 ] || die "$1 requires a value"; }
load_config_file(){
  local path="$1" only_missing="${2:-0}" line key value current
  [ -f "$path" ] || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    [ -n "$line" ] || continue
    key="${line%%=*}"; value="${line#*=}"
    case "$key" in ENDPOINT|SECRET|NODE|LOG_PATH|NETWORK_INTERFACE|WINDOW|INTERVAL|STATS_INTERVAL) ;; *) continue;; esac
    if [ "$only_missing" = 1 ]; then
      current="${!key-}"
      [ -z "$current" ] || continue
    fi
    if [[ "$value" == \"*\" ]]; then value="${value:1:${#value}-2}"; fi
    value="${value//\\\"/\"}"; value="${value//\\\\/\\}"
    printf -v "$key" '%s' "$value"
  done < "$path"
}
load_config(){ load_config_file "$ENV_FILE" || die "$ENV_FILE not found"; }
load_legacy_config(){ load_config_file "$LEGACY_ENV_FILE" 1 || return 0; }
export_config(){ export ENDPOINT SECRET NODE LOG_PATH NETWORK_INTERFACE STATE_DIR WINDOW INTERVAL STATS_INTERVAL; }
download(){
  local url="$1" target="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fL --retry 3 --proto '=https' --tlsv1.2 "$url" -o "$target"
  elif command -v wget >/dev/null 2>&1; then
    wget --https-only --tries=3 -qO "$target" "$url"
  else
    die "curl or wget is required"
  fi
}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --secret) need_arg "$@"; SECRET="$2"; shift 2;;
    --secret-file) need_arg "$@"; SECRET="$(<"$2")"; shift 2;;
    --endpoint) need_arg "$@"; ENDPOINT="$2"; shift 2;;
    --node) need_arg "$@"; NODE="$2"; shift 2;;
    --log) need_arg "$@"; LOG_PATH="$2"; shift 2;;
    --interface) need_arg "$@"; NETWORK_INTERFACE="$2"; shift 2;;
    --window) need_arg "$@"; WINDOW="$2"; shift 2;;
    --interval) need_arg "$@"; INTERVAL="$2"; shift 2;;
    --stats-interval) need_arg "$@"; STATS_INTERVAL="$2"; shift 2;;
    --repo) need_arg "$@"; REPO="$2"; shift 2;;
    --version) need_arg "$@"; VERSION="$2"; shift 2;;
    --build-from-source) BUILD_FROM_SOURCE=1; shift;;
    --non-interactive) NON_INTERACTIVE=1; shift;;
    --purge) PURGE=1; shift;;
    -h|--help) usage; exit 0;;
    *) die "unknown argument: $1";;
  esac
done

[ "$(id -u)" = 0 ] || die "run as root (sudo)"
case "$ACTION" in
  status) exec systemctl --no-pager --lines=20 status palm-agent.service;;
  logs) exec journalctl -u palm-agent.service -f;;
  doctor) [ -x "$BIN" ] || die "$BIN is not installed"; load_config; export_config; exec "$BIN" doctor;;
  uninstall)
    systemctl disable --now palm-agent.service >/dev/null 2>&1 || true
    rm -f "$UNIT_FILE" "$BIN"; systemctl daemon-reload
    if [ "$PURGE" = 1 ]; then rm -f "$ENV_FILE"; rm -rf "$STATE_DIR"; fi
    ok "Palm Agent removed$([ "$PURGE" = 1 ] && printf ' with configuration')"; exit 0;;
  install|update) ;;
  *) usage; die "unknown action: $ACTION";;
esac

# Re-running `install` is an in-place upgrade. Explicit CLI options win, while
# omitted values are inherited from the currently installed (or legacy) agent.
load_config_file "$ENV_FILE" 1 || true
load_legacy_config
NODE="${NODE:-$(hostname -s)}"
WINDOW="${WINDOW:-90}"
INTERVAL="${INTERVAL:-20}"
STATS_INTERVAL="${STATS_INTERVAL:-15}"
[ -n "$SECRET" ] || { if [ "$NON_INTERACTIVE" = 0 ]; then read -rsp "HMAC secret: " SECRET; echo; fi; }
[ -n "$SECRET" ] || die "--secret or --secret-file is required for the first installation"
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || die "curl or wget is required"
case "$REPO" in */*) ;; *) die "--repo must be OWNER/REPO";; esac
case "$NODE" in ''|*[!A-Za-z0-9._-]*) die "node may contain A-Z, a-z, 0-9, dot, underscore and dash";; esac
for n in "$WINDOW" "$INTERVAL" "$STATS_INTERVAL"; do [[ "$n" =~ ^[0-9]+$ ]] || die "intervals must be integer seconds"; done
[ -n "$SECRET" ] || die "secret is empty"
[[ "$ENDPOINT" =~ ^https://[^[:space:]]+/limiter/ingest/?$ ]] || die "endpoint must be HTTPS and end with /limiter/ingest"
for value in "$SECRET" "$ENDPOINT" "$LOG_PATH" "$NETWORK_INTERFACE"; do
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "configuration values may not contain newlines"
done

if [ -z "$LOG_PATH" ]; then
  for candidate in /var/log/xray/access.log /usr/local/x-ui/bin/access.log /usr/local/x-ui/access.log /var/log/x-ui/access.log; do
    if [ -f "$candidate" ]; then LOG_PATH="$candidate"; break; fi
  done
fi
[ -n "$LOG_PATH" ] || die "Xray access log not found; enable it and pass --log PATH"

machine="$(uname -m)"; case "$machine" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) die "unsupported architecture: $machine";; esac
tmp="$(mktemp -d /tmp/palm-agent.XXXXXX)"; trap 'rm -rf "$tmp"' EXIT

install_release(){
  local tag="$VERSION" api="https://api.github.com/repos/$REPO/releases/latest"
  if [ "$tag" = latest ]; then download "$api" "$tmp/release.json"; tag="$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/release.json" | head -n1)"; fi
  [ -n "$tag" ] || die "cannot determine latest release"
  local base="https://github.com/$REPO/releases/download/$tag" asset="palm-agent-linux-$arch"
  info "Downloading $asset ($tag)"
  download "$base/$asset" "$tmp/$APP"
  download "$base/checksums.txt" "$tmp/checksums.txt"
  expected="$(awk -v f="$asset" '$2==f || $2=="*"f {print $1}' "$tmp/checksums.txt")"; [ -n "$expected" ] || die "checksum missing for $asset"
  actual="$(sha256sum "$tmp/$APP" | awk '{print $1}')"; [ "$actual" = "$expected" ] || die "binary checksum mismatch"
}
install_source(){
  local gov="${PALM_GO_VERSION:-}" goarch="$arch" tar expected
  download "https://go.dev/dl/?mode=json&include=all" "$tmp/go.json"
  if [ -n "$gov" ]; then
    tar="go${gov}.linux-${goarch}.tar.gz"
  else
    tar="$(sed -n 's/.*"filename": "\([^"]*\.linux-'"$goarch"'\.tar\.gz\)".*/\1/p' "$tmp/go.json" | head -n1)"
    [ -n "$tar" ] || die "cannot determine current Go toolchain"
    gov="${tar#go}"; gov="${gov%.linux-*}"
  fi
  info "Downloading temporary Go $gov toolchain"
  download "https://go.dev/dl/$tar" "$tmp/go.tgz"
  expected="$(awk -v name="$tar" '
    index($0, "\"filename\"") && index($0, name) { found=1 }
    found && /"sha256"/ { gsub(/^.*"sha256": "/, ""); gsub(/".*$/, ""); print; exit }
    found && /}/ { exit }
  ' "$tmp/go.json")"
  [ -n "$expected" ] && [ "$(sha256sum "$tmp/go.tgz"|awk '{print $1}')" = "$expected" ] || die "Go toolchain checksum mismatch"
  mkdir "$tmp/toolchain"; tar -xzf "$tmp/go.tgz" -C "$tmp/toolchain"
  download "https://github.com/$REPO/archive/refs/heads/main.tar.gz" "$tmp/source.tgz"
  mkdir "$tmp/source"; tar -xzf "$tmp/source.tgz" -C "$tmp/source" --strip-components=1
  (cd "$tmp/source" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" "$tmp/toolchain/go/bin/go" test ./... && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" "$tmp/toolchain/go/bin/go" build -trimpath -ldflags '-s -w' -o "$tmp/$APP" .)
}

if [ "$BUILD_FROM_SOURCE" = 1 ]; then install_source; else install_release; fi
install -m 0755 "$tmp/$APP" "$BIN"
install -d -m 0700 "$STATE_DIR"
install -d -m 0755 "$LIB_DIR"
script_source="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd 2>/dev/null || true)/xui_config.py"
if [ -f "$script_source" ]; then
  install -m 0755 "$script_source" "$LIB_DIR/xui_config.py"
else
  download "https://raw.githubusercontent.com/$REPO/main/xui_config.py" "$tmp/xui_config.py"
  install -m 0755 "$tmp/xui_config.py" "$LIB_DIR/xui_config.py"
fi
umask 077
escape_env(){ local value="$1"; value="${value//\\/\\\\}"; value="${value//\"/\\\"}"; printf '%s' "$value"; }
cat > "$ENV_FILE" <<EOF
ENDPOINT="$(escape_env "$ENDPOINT")"
SECRET="$(escape_env "$SECRET")"
NODE="$(escape_env "$NODE")"
LOG_PATH="$(escape_env "$LOG_PATH")"
NETWORK_INTERFACE="$(escape_env "$NETWORK_INTERFACE")"
STATE_DIR="$STATE_DIR"
WINDOW="$WINDOW"
INTERVAL="$INTERVAL"
STATS_INTERVAL="$STATS_INTERVAL"
EOF
chmod 0600 "$ENV_FILE"
cat > "$UNIT_FILE" <<'EOF'
[Unit]
Description=Palm node telemetry and live-IP agent
Wants=network-online.target
After=network-online.target x-ui.service

[Service]
Type=simple
EnvironmentFile=/etc/palm-agent.env
ExecStart=/usr/local/bin/palm-agent run
Restart=always
RestartSec=5
StateDirectory=palm-agent
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=/var/lib/palm-agent /etc/x-ui /usr/local/x-ui/bin

[Install]
WantedBy=multi-user.target
EOF
if [ -e "$LEGACY_UNIT_FILE" ] || [ -e "$LEGACY_BIN" ] || [ -e "$LEGACY_ENV_FILE" ]; then
  info "Migrating legacy palm-iplog agent"
  systemctl disable --now palm-iplog.service >/dev/null 2>&1 || true
  rm -f "$LEGACY_UNIT_FILE" "$LEGACY_BIN" "$LEGACY_AWK" "$LEGACY_STATS_BIN" "$LEGACY_ENV_FILE"
  systemctl reset-failed palm-iplog.service >/dev/null 2>&1 || true
  ok "Legacy palm-iplog agent removed"
fi
systemctl daemon-reload; systemctl enable --now palm-agent.service >/dev/null
ok "Binary installed: $BIN"
ok "Configuration: $ENV_FILE (0600)"
ok "Access log: $LOG_PATH"
ok "Service started: palm-agent.service"
if systemctl is-active --quiet palm-agent.service; then load_config; export_config; "$BIN" doctor || warn "doctor found a problem; run: journalctl -u palm-agent -n 50"; else systemctl --no-pager --lines=20 status palm-agent.service || true; exit 1; fi
