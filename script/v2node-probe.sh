#!/bin/bash

set -u

DEFAULT_CONFIG_DIR="/etc/.buncloud-agent"
LEGACY_CONFIG_DIR="/etc/v2node"
RUNTIME_ENV_FILE="${V2NODE_RUNTIME_ENV_FILE:-${DEFAULT_CONFIG_DIR}/runtime.env}"
if [[ -f "$RUNTIME_ENV_FILE" ]]; then
    # shellcheck disable=SC1090
    . "$RUNTIME_ENV_FILE"
fi

CONFIG_DIR="${V2NODE_CONFIG_DIR:-${DEFAULT_CONFIG_DIR}}"
STATE_FILE="${V2NODE_PROBE_STATE_FILE:-${CONFIG_DIR}/probe-state.json}"
CONFIG_FILE="${V2NODE_CONFIG_FILE:-${BUNCLOUD_CONFIG_PATH:-${CONFIG_DIR}/config.enc.json}}"
CONFIG_KEY_FILE="${V2NODE_CONFIG_KEY_FILE:-${CONFIG_DIR}/config.key}"
MANAGED_NODES_STATE_FILE="${V2NODE_PROBE_MANAGED_NODES_STATE_FILE:-${CONFIG_DIR}/probe-managed-nodes.json}"
DDNS_STATE_FILE="${V2NODE_PROBE_DDNS_STATE_FILE:-${CONFIG_DIR}/probe-ddns.json}"
UPDATE_STATE_FILE="${V2NODE_PROBE_UPDATE_STATE_FILE:-${CONFIG_DIR}/probe-update.json}"
GOST_CONFIG_FILE="${V2NODE_PROBE_GOST_CONFIG_FILE:-/etc/gost/config.json}"
GOST_CONFIG_BACKUP_FILE="${V2NODE_PROBE_GOST_CONFIG_BACKUP_FILE:-/etc/gost/config.json.last-good}"
GOST_BIN="${V2NODE_PROBE_GOST_BIN:-/usr/bin/gost}"
GOST_VERSION="${V2NODE_PROBE_GOST_VERSION:-2.11.2}"
SINGBOX_BIN="${V2NODE_PROBE_SINGBOX_BIN:-/usr/local/bin/sing-box-v2node-test}"
SINGBOX_VERSION="${V2NODE_PROBE_SINGBOX_VERSION:-1.12.0}"
AUTO_UPDATE_INTERVAL_DEFAULT=86400
AUTO_UPDATE_REPO_DEFAULT="YeJianbo/v2node"
SYNC_INTERVAL_DEFAULT=30
STATUS_INTERVAL_DEFAULT=5
CONFIG_CHANGED=0
GOST_CONFIG_CHANGED=0

log() {
    echo "[v2node-probe] $*"
}

fail() {
    log "$*" >&2
    return 1
}

ensure_private_dir() {
    mkdir -p "$1" >/dev/null 2>&1
    chmod 700 "$1" >/dev/null 2>&1 || true
}

read_config_key() {
    if [[ -n "${BUNCLOUD_CONFIG_KEY:-}" ]]; then
        printf '%s' "$BUNCLOUD_CONFIG_KEY"
        return 0
    fi
    if [[ -n "${V2NODE_CONFIG_KEY:-}" ]]; then
        printf '%s' "$V2NODE_CONFIG_KEY"
        return 0
    fi
    if [[ -s "$CONFIG_KEY_FILE" ]]; then
        tr -d '\r\n' < "$CONFIG_KEY_FILE"
        return 0
    fi
    return 1
}

decrypt_config_to_file() {
    local output_file="$1"
    local key

    if [[ -f "$CONFIG_FILE" ]]; then
        key=$(read_config_key) || return 1
        /usr/local/v2node/v2node config decrypt --in "$CONFIG_FILE" --out "$output_file" --key "$key" >/dev/null 2>&1
        return $?
    fi

    if [[ -f "${CONFIG_DIR}/config.json" ]]; then
        cp "${CONFIG_DIR}/config.json" "$output_file"
        return 0
    fi

    if [[ -f "${LEGACY_CONFIG_DIR}/config.json" ]]; then
        cp "${LEGACY_CONFIG_DIR}/config.json" "$output_file"
        return 0
    fi

    return 1
}

encrypt_config_from_file() {
    local input_file="$1"
    local key

    key=$(read_config_key) || return 1
    ensure_private_dir "$CONFIG_DIR"
    /usr/local/v2node/v2node config encrypt --in "$input_file" --out "$CONFIG_FILE" --key "$key" >/dev/null 2>&1 || return 1
    chmod 600 "$CONFIG_FILE" >/dev/null 2>&1 || true
    rm -f "${CONFIG_DIR}/config.json" "${LEGACY_CONFIG_DIR}/config.json"
}

seed_plain_config_file() {
    local output_file="$1"
    jq -n '{
        Log: {
            Level: "warning",
            Output: "",
            Access: "none"
        },
        Nodes: []
    }' > "$output_file"
}

load_state() {
    if [[ ! -f "$STATE_FILE" ]]; then
        fail "未找到探针配置文件: $STATE_FILE"
        return 1
    fi

    if jq -e 'type == "object"' "$STATE_FILE" >/dev/null 2>&1; then
        PANEL_URL=$(jq -r '.panel_url // ""' "$STATE_FILE")
        MACHINE_TOKEN=$(jq -r '.machine_token // ""' "$STATE_FILE")
        MACHINE_ID=$(jq -r '.machine_id // ""' "$STATE_FILE")
        SYNC_INTERVAL=$(jq -r '.sync_interval // empty' "$STATE_FILE")
        STATUS_INTERVAL=$(jq -r '.status_interval // empty' "$STATE_FILE")
    else
        # shellcheck disable=SC1090
        source "$STATE_FILE"
        PANEL_URL="${PANEL_URL:-}"
        MACHINE_TOKEN="${MACHINE_TOKEN:-}"
        MACHINE_ID="${MACHINE_ID:-}"
        SYNC_INTERVAL="${SYNC_INTERVAL:-}"
        STATUS_INTERVAL="${STATUS_INTERVAL:-}"
    fi

    SYNC_INTERVAL="${SYNC_INTERVAL:-$SYNC_INTERVAL_DEFAULT}"
    if ! [[ "$SYNC_INTERVAL" =~ ^[0-9]+$ ]] || (( SYNC_INTERVAL < SYNC_INTERVAL_DEFAULT )); then
        SYNC_INTERVAL="$SYNC_INTERVAL_DEFAULT"
    fi
    STATUS_INTERVAL="${STATUS_INTERVAL:-$STATUS_INTERVAL_DEFAULT}"
    if ! [[ "$STATUS_INTERVAL" =~ ^[0-9]+$ ]] || (( STATUS_INTERVAL < STATUS_INTERVAL_DEFAULT )); then
        STATUS_INTERVAL="$STATUS_INTERVAL_DEFAULT"
    fi

    if [[ -z "$PANEL_URL" || -z "$MACHINE_TOKEN" || -z "$MACHINE_ID" ]]; then
        fail "探针配置不完整，请检查 $STATE_FILE"
        return 1
    fi

    PANEL_URL="${PANEL_URL%/}"
    load_ddns_state
    load_update_state
}

ensure_dependencies() {
    if ! command -v curl >/dev/null 2>&1; then
        fail "缺少 curl"
        return 1
    fi
    if ! command -v jq >/dev/null 2>&1; then
        fail "缺少 jq"
        return 1
    fi
    if ! command -v openssl >/dev/null 2>&1; then
        fail "缺少 openssl"
        return 1
    fi
    if ! command -v gzip >/dev/null 2>&1; then
        fail "缺少 gzip"
        return 1
    fi
    if ! command -v tar >/dev/null 2>&1; then
        fail "缺少 tar"
        return 1
    fi
}

detect_firewall_backend() {
    if command -v ufw >/dev/null 2>&1; then
        local ufw_status
        ufw_status=$(ufw status 2>/dev/null | head -n 1 || true)
        if printf '%s' "$ufw_status" | grep -qi '^Status: active'; then
            printf 'ufw'
            return
        fi
    fi

    if command -v firewall-cmd >/dev/null 2>&1; then
        if firewall-cmd --state 2>/dev/null | grep -qi '^running$'; then
            printf 'firewalld'
            return
        fi
    fi

    if command -v iptables >/dev/null 2>&1; then
        printf 'iptables'
        return
    fi

    printf 'none'
}

ensure_ufw_port_open() {
    local port="$1"
    local protocol="$2"

    if ufw status 2>/dev/null | grep -Eq "(^|[[:space:]])${port}/${protocol}([[:space:]]|$)"; then
        return 0
    fi

    ufw allow "${port}/${protocol}" >/dev/null
    log "已放行防火墙端口 ${port}/${protocol} (ufw)"
}

ensure_firewalld_port_open() {
    local port="$1"
    local protocol="$2"
    local changed="$3"

    if firewall-cmd --permanent --query-port="${port}/${protocol}" >/dev/null 2>&1; then
        return 0
    fi

    firewall-cmd --permanent --add-port="${port}/${protocol}" >/dev/null
    printf -v "$changed" '1'
    log "已放行防火墙端口 ${port}/${protocol} (firewalld)"
}

ensure_iptables_port_open() {
    local port="$1"
    local protocol="$2"

    if ! iptables -C INPUT -p "$protocol" --dport "$port" -j ACCEPT >/dev/null 2>&1; then
        iptables -I INPUT -p "$protocol" --dport "$port" -j ACCEPT
        log "已放行防火墙端口 ${port}/${protocol} (iptables)"
    fi

    if command -v ip6tables >/dev/null 2>&1; then
        if ! ip6tables -C INPUT -p "$protocol" --dport "$port" -j ACCEPT >/dev/null 2>&1; then
            ip6tables -I INPUT -p "$protocol" --dport "$port" -j ACCEPT >/dev/null 2>&1 || true
        fi
    fi
}

sync_firewall() {
    local firewall_json="$1"
    local backend
    local normalized_rules

    if ! normalized_rules=$(printf '%s' "$firewall_json" | jq -c '
        [
            (. // [])[]?
            | {
                port: ((.port // 0) | tonumber),
                protocols: (
                    (.protocols // [])
                    | map(ascii_downcase)
                    | map(select(. == "tcp" or . == "udp"))
                    | unique
                )
            }
            | select(.port >= 1 and .port <= 65535)
            | select((.protocols | length) > 0)
        ]
    '); then
        fail "解析防火墙规则失败"
        return 1
    fi

    if [[ "$(printf '%s' "$normalized_rules" | jq 'length')" -eq 0 ]]; then
        return 0
    fi

    backend=$(detect_firewall_backend)
    if [[ "$backend" == "none" ]]; then
        log "未检测到可管理的防火墙，跳过端口放行"
        return 0
    fi

    case "$backend" in
        ufw)
            while read -r port protocol; do
                ensure_ufw_port_open "$port" "$protocol" || return 1
            done < <(printf '%s' "$normalized_rules" | jq -r '.[] | .port as $port | .protocols[] | "\($port) \(.)"')
            ;;
        firewalld)
            local firewalld_changed=0
            while read -r port protocol; do
                ensure_firewalld_port_open "$port" "$protocol" firewalld_changed || return 1
            done < <(printf '%s' "$normalized_rules" | jq -r '.[] | .port as $port | .protocols[] | "\($port) \(.)"')
            if [[ "$firewalld_changed" == "1" ]]; then
                firewall-cmd --reload >/dev/null
            fi
            ;;
        iptables)
            while read -r port protocol; do
                ensure_iptables_port_open "$port" "$protocol" || return 1
            done < <(printf '%s' "$normalized_rules" | jq -r '.[] | .port as $port | .protocols[] | "\($port) \(.)"')
            ;;
    esac
}

detect_service_manager() {
    if command -v systemctl >/dev/null 2>&1 && timeout 3 systemctl list-units >/dev/null 2>&1; then
        printf 'systemd'
        return
    fi

    if command -v rc-service >/dev/null 2>&1; then
        printf 'openrc'
        return
    fi

    printf 'none'
}

detect_gost_arch() {
    case "$(uname -m 2>/dev/null)" in
        x86_64|amd64)
            printf 'amd64'
            ;;
        aarch64|arm64)
            printf 'arm64'
            ;;
        armv7l|armv7)
            printf 'armv7'
            ;;
        armv6l|armv6)
            printf 'armv6'
            ;;
        i386|i686)
            printf '386'
            ;;
        *)
            return 1
            ;;
    esac
}

ensure_gost_binary() {
    if [[ -x "$GOST_BIN" ]]; then
        return 0
    fi

    local arch
    local download_url
    local tmp_file

    arch=$(detect_gost_arch) || {
        fail "不支持当前架构，无法自动安装 gost"
        return 1
    }

    download_url="https://github.com/ginuerzh/gost/releases/download/v${GOST_VERSION}/gost-linux-${arch}-${GOST_VERSION}.gz"
    tmp_file=$(mktemp)
    if ! curl -fsSL --connect-timeout 8 --max-time 60 "$download_url" -o "$tmp_file"; then
        rm -f "$tmp_file"
        fail "下载 gost 失败: ${download_url}"
        return 1
    fi

    mkdir -p "$(dirname "$GOST_BIN")"
    if ! gzip -dc "$tmp_file" > "$GOST_BIN"; then
        rm -f "$tmp_file"
        rm -f "$GOST_BIN"
        fail "解压 gost 失败"
        return 1
    fi
    rm -f "$tmp_file"
    chmod +x "$GOST_BIN"
    log "已安装 gost ${GOST_VERSION}"
}

ensure_gost_service() {
    local service_manager
    service_manager=$(detect_service_manager)

    case "$service_manager" in
        systemd)
            if [[ ! -f /etc/systemd/system/gost.service ]]; then
                cat > /etc/systemd/system/gost.service <<EOF
[Unit]
Description=gost relay service
After=network.target
Wants=network.target

[Service]
Type=simple
User=root
Group=root
ExecStart=${GOST_BIN} -C ${GOST_CONFIG_FILE}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
                systemctl daemon-reload >/dev/null 2>&1 || true
            fi
            systemctl enable gost >/dev/null 2>&1 || true
            ;;
        openrc)
            if [[ ! -f /etc/init.d/gost ]]; then
                cat > /etc/init.d/gost <<EOF
#!/sbin/openrc-run

name="gost"
description="gost relay service"
command="${GOST_BIN}"
command_args="-C ${GOST_CONFIG_FILE}"
command_background="yes"
pidfile="/run/gost.pid"

depend() {
    need net
}
EOF
                chmod +x /etc/init.d/gost
            fi
            rc-update add gost default >/dev/null 2>&1 || true
            ;;
        *)
            fail "未检测到 systemd/openrc，无法托管 gost 服务"
            return 1
            ;;
    esac
}

stop_gost_service() {
    local service_manager
    service_manager=$(detect_service_manager)

    case "$service_manager" in
        systemd)
            systemctl stop gost >/dev/null 2>&1 || true
            ;;
        openrc)
            rc-service gost stop >/dev/null 2>&1 || true
            ;;
        *)
            pkill -f "${GOST_BIN} -C ${GOST_CONFIG_FILE}" >/dev/null 2>&1 || true
            ;;
    esac
}

restart_gost_service() {
    local service_manager
    service_manager=$(detect_service_manager)

    case "$service_manager" in
        systemd)
            systemctl restart gost
            ;;
        openrc)
            rc-service gost restart >/dev/null 2>&1 || rc-service gost start >/dev/null 2>&1
            ;;
        *)
            pkill -f "${GOST_BIN} -C ${GOST_CONFIG_FILE}" >/dev/null 2>&1 || true
            setsid "$GOST_BIN" -C "$GOST_CONFIG_FILE" >/var/log/gost.log 2>&1 < /dev/null &
            ;;
    esac
}

cleanup_gost_config() {
    GOST_CONFIG_CHANGED=0

    if [[ -f "$GOST_CONFIG_FILE" ]]; then
        rm -f "$GOST_CONFIG_FILE"
        GOST_CONFIG_CHANGED=1
        log "已清理 ${GOST_CONFIG_FILE}"
    fi

    stop_gost_service
}

restore_last_good_gost_config() {
    if [[ ! -f "$GOST_CONFIG_BACKUP_FILE" ]]; then
        return 1
    fi

    mkdir -p "$(dirname "$GOST_CONFIG_FILE")"
    cp "$GOST_CONFIG_BACKUP_FILE" "$GOST_CONFIG_FILE"
    GOST_CONFIG_CHANGED=1
    log "已恢复上一份可用 gost 配置"
}

write_gost_config() {
    local relay_rules_json="$1"
    local tmp_file
    local serve_nodes

    GOST_CONFIG_CHANGED=0
    tmp_file=$(mktemp)
    if ! serve_nodes=$(printf '%s' "$relay_rules_json" | jq -c '
        def normhost:
            tostring as $h
            | if $h == "" then ""
              elif ($h | startswith("[")) then $h
              elif ($h | contains(":")) then "[\($h)]"
              else $h
              end;
        [
            .[]?
            | . as $rule
            | select((.listen_port // 0 | tonumber) >= 1 and (.target_port // 0 | tonumber) >= 1)
            | (.listen_host // "0.0.0.0" | tostring) as $listenHost
            | (.target_host // "" | normhost) as $targetHost
            | select($targetHost != "")
            | $rule.protocols[]?
            | ascii_downcase
            | select(. == "tcp" or . == "udp")
            | "\(.)://\((if $listenHost == "0.0.0.0" or $listenHost == "::" then ":" else ((($listenHost | normhost)) + ":") end))\(($rule.listen_port | tonumber))/\($targetHost):\(($rule.target_port | tonumber))"
        ]
    '); then
        rm -f "$tmp_file"
        fail "生成 gost ServeNodes 失败"
        return 1
    fi

    if [[ "$(printf '%s' "$serve_nodes" | jq 'length')" -eq 0 ]]; then
        rm -f "$tmp_file"
        cleanup_gost_config
        log "本次转发规则为空，已清理 gost 配置并停止服务"
        return 0
    fi

    if ! jq -n \
        --argjson serve_nodes "$serve_nodes" \
        '{
            Debug: false,
            Retries: 0,
            ServeNodes: $serve_nodes
        }' > "$tmp_file"; then
        rm -f "$tmp_file"
        fail "生成 gost 配置文件失败"
        return 1
    fi

    mkdir -p "$(dirname "$GOST_CONFIG_FILE")"
    if [[ -f "$GOST_CONFIG_FILE" ]] && cmp -s "$tmp_file" "$GOST_CONFIG_FILE"; then
        rm -f "$tmp_file"
        return 0
    fi

    mv "$tmp_file" "$GOST_CONFIG_FILE"
    cp "$GOST_CONFIG_FILE" "$GOST_CONFIG_BACKUP_FILE" 2>/dev/null || true
    GOST_CONFIG_CHANGED=1
    log "已更新 ${GOST_CONFIG_FILE}"
}

get_gost_version() {
    if [[ ! -x "$GOST_BIN" ]]; then
        printf ''
        return
    fi

    "$GOST_BIN" -V 2>/dev/null | head -n 1 | tr -d '\r'
}

sync_relay_config() {
    local relay_json="$1"
    local relay_rules_json
    local previous_gost_config
    local had_previous_gost_config=0

    if ! relay_rules_json=$(printf '%s' "$relay_json" | jq -c '
        def normprotos:
            if type == "array" then .
            elif type == "string" then (gsub("[,;/|+]+"; " ") | split(" "))
            else []
            end
            | map(ascii_downcase)
            | map(if . == "all" or . == "both" or . == "tcpudp" then ["tcp", "udp"] else [.] end)
            | flatten
            | map(select(. == "tcp" or . == "udp"))
            | unique;
        (.rules // [])
        | map({
            listen_host: (.listen_host // .listenHost // .local_host // .localHost // "0.0.0.0"),
            listen_port: ((.listen_port // .listenPort // .local_port // .localPort // 0) | tonumber),
            target_host: (.target_host // .targetHost // .remote_host // .remoteHost // .host // ""),
            target_port: ((.target_port // .targetPort // .remote_port // .remotePort // .port // 0) | tonumber),
            protocols: (
                (if ((.protocols // null) | type) == "array" and ((.protocols // []) | length) > 0 then .protocols
                 elif ((.protocols // null) | type) == "string" and ((.protocols // "") | length) > 0 then .protocols
                 elif (.protocol // null) then .protocol
                 elif (.type // null) then .type
                 else []
                 end) | normprotos
            )
        })
        | map(select(.listen_port >= 1 and .listen_port <= 65535 and .target_port >= 1 and .target_port <= 65535))
    '); then
        fail "解析 gost 转发规则失败，保留当前转发配置"
        return 1
    fi

    previous_gost_config=$(mktemp)
    if [[ -f "$GOST_CONFIG_FILE" ]]; then
        cp "$GOST_CONFIG_FILE" "$previous_gost_config"
        had_previous_gost_config=1
    fi

    if [[ "$(printf '%s' "$relay_rules_json" | jq 'length')" -eq 0 ]]; then
        cleanup_gost_config
        log "本次下发转发规则为空，已清理 gost 配置并停止服务"
        rm -f "$previous_gost_config"
        return 0
    fi

    ensure_gost_binary || {
        rm -f "$previous_gost_config"
        return 1
    }
    ensure_gost_service || {
        rm -f "$previous_gost_config"
        return 1
    }
    write_gost_config "$relay_rules_json" || {
        rm -f "$previous_gost_config"
        return 1
    }

    if [[ "${GOST_CONFIG_CHANGED:-0}" == "1" ]]; then
        restart_gost_service || {
            fail "重启 gost 服务失败，恢复上一份转发配置"
            if [[ "$had_previous_gost_config" == "1" ]]; then
                mkdir -p "$(dirname "$GOST_CONFIG_FILE")"
                mv "$previous_gost_config" "$GOST_CONFIG_FILE"
                restart_gost_service || true
            else
                rm -f "$GOST_CONFIG_FILE" "$previous_gost_config"
                stop_gost_service || true
            fi
            return 1
        }
    fi

    rm -f "$previous_gost_config"
}

load_ddns_state() {
    DDNS_ZONE_ID=""
    DDNS_RECORD_ID=""
    DDNS_LAST_IP=""
    DDNS_LAST_HOST=""
    DDNS_LAST_SYNCED_AT="0"
    DDNS_LAST_ERROR=""

    if [[ ! -f "$DDNS_STATE_FILE" ]]; then
        return
    fi

    if jq -e 'type == "object"' "$DDNS_STATE_FILE" >/dev/null 2>&1; then
        DDNS_ZONE_ID=$(jq -r '.zone_id // ""' "$DDNS_STATE_FILE")
        DDNS_RECORD_ID=$(jq -r '.record_id // ""' "$DDNS_STATE_FILE")
        DDNS_LAST_IP=$(jq -r '.last_ip // ""' "$DDNS_STATE_FILE")
        DDNS_LAST_HOST=$(jq -r '.last_host // ""' "$DDNS_STATE_FILE")
        DDNS_LAST_SYNCED_AT=$(jq -r '.last_synced_at // 0' "$DDNS_STATE_FILE")
        DDNS_LAST_ERROR=$(jq -r '.last_error // ""' "$DDNS_STATE_FILE")
        return
    fi

    # shellcheck disable=SC1090
    source "$DDNS_STATE_FILE"
    DDNS_ZONE_ID="${DDNS_ZONE_ID:-}"
    DDNS_RECORD_ID="${DDNS_RECORD_ID:-}"
    DDNS_LAST_IP="${DDNS_LAST_IP:-}"
    DDNS_LAST_HOST="${DDNS_LAST_HOST:-}"
    DDNS_LAST_SYNCED_AT="${DDNS_LAST_SYNCED_AT:-0}"
    DDNS_LAST_ERROR="${DDNS_LAST_ERROR:-}"
}

save_ddns_state() {
    ensure_private_dir "$(dirname "$DDNS_STATE_FILE")"
    jq -n \
        --arg zone_id "${DDNS_ZONE_ID:-}" \
        --arg record_id "${DDNS_RECORD_ID:-}" \
        --arg last_ip "${DDNS_LAST_IP:-}" \
        --arg last_host "${DDNS_LAST_HOST:-}" \
        --argjson last_synced_at "${DDNS_LAST_SYNCED_AT:-0}" \
        --arg last_error "${DDNS_LAST_ERROR:-}" \
        '{
            zone_id: $zone_id,
            record_id: $record_id,
            last_ip: $last_ip,
            last_host: $last_host,
            last_synced_at: $last_synced_at,
            last_error: $last_error
        }' > "$DDNS_STATE_FILE"
    chmod 600 "$DDNS_STATE_FILE" >/dev/null 2>&1 || true
}

update_ddns_state() {
    DDNS_LAST_HOST="${1:-$DDNS_LAST_HOST}"
    DDNS_LAST_IP="${2:-$DDNS_LAST_IP}"
    DDNS_LAST_SYNCED_AT="${3:-$DDNS_LAST_SYNCED_AT}"
    DDNS_LAST_ERROR="${4:-$DDNS_LAST_ERROR}"
    save_ddns_state
}

detect_singbox_arch() {
    case "$(uname -m 2>/dev/null)" in
        x86_64|amd64)
            printf 'amd64'
            ;;
        aarch64|arm64)
            printf 'arm64'
            ;;
        armv7l|armv7)
            printf 'armv7'
            ;;
        armv6l|armv6)
            printf 'armv6'
            ;;
        i386|i686)
            printf '386'
            ;;
        s390x)
            printf 's390x'
            ;;
        *)
            return 1
            ;;
    esac
}

ensure_singbox_binary() {
    if [[ -x "$SINGBOX_BIN" ]]; then
        return 0
    fi

    local arch
    local tmp_dir
    local archive
    local url
    local binary_path

    arch=$(detect_singbox_arch) || {
        fail "当前架构不支持自动安装 sing-box"
        return 1
    }

    tmp_dir=$(mktemp -d)
    archive="${tmp_dir}/sing-box.tar.gz"
    url="https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/sing-box-${SINGBOX_VERSION}-linux-${arch}.tar.gz"

    if ! curl -fsSL --connect-timeout 8 --max-time 120 "$url" -o "$archive"; then
        rm -rf "$tmp_dir"
        fail "下载 sing-box 失败: ${url}"
        return 1
    fi

    if ! tar -xzf "$archive" -C "$tmp_dir"; then
        rm -rf "$tmp_dir"
        fail "解压 sing-box 失败"
        return 1
    fi

    binary_path=$(find "$tmp_dir" -type f -name sing-box | head -n 1)
    if [[ -z "$binary_path" || ! -f "$binary_path" ]]; then
        rm -rf "$tmp_dir"
        fail "未找到 sing-box 可执行文件"
        return 1
    fi

    mkdir -p "$(dirname "$SINGBOX_BIN")"
    cp "$binary_path" "$SINGBOX_BIN"
    chmod +x "$SINGBOX_BIN"
    rm -rf "$tmp_dir"
    log "已安装 sing-box ${SINGBOX_VERSION}"
}

load_update_state() {
    PROBE_AUTO_UPDATE_ENABLED="false"
    PROBE_AUTO_UPDATE_INTERVAL="$AUTO_UPDATE_INTERVAL_DEFAULT"
    PROBE_AUTO_UPDATE_REPO="$AUTO_UPDATE_REPO_DEFAULT"
    PROBE_UPDATE_LAST_CHECKED_AT="0"
    PROBE_UPDATE_LAST_VERSION=""
    PROBE_UPDATE_LAST_ERROR=""

    if [[ ! -f "$UPDATE_STATE_FILE" ]]; then
        return
    fi

    if jq -e 'type == "object"' "$UPDATE_STATE_FILE" >/dev/null 2>&1; then
        PROBE_AUTO_UPDATE_ENABLED=$(jq -r '.enabled // false' "$UPDATE_STATE_FILE")
        PROBE_AUTO_UPDATE_INTERVAL=$(jq -r '.interval // 86400' "$UPDATE_STATE_FILE")
        PROBE_AUTO_UPDATE_REPO=$(jq -r '.repo // "YeJianbo/v2node"' "$UPDATE_STATE_FILE")
        PROBE_UPDATE_LAST_CHECKED_AT=$(jq -r '.last_checked_at // 0' "$UPDATE_STATE_FILE")
        PROBE_UPDATE_LAST_VERSION=$(jq -r '.last_version // ""' "$UPDATE_STATE_FILE")
        PROBE_UPDATE_LAST_ERROR=$(jq -r '.last_error // ""' "$UPDATE_STATE_FILE")
        return
    fi

    # shellcheck disable=SC1090
    source "$UPDATE_STATE_FILE"
    PROBE_AUTO_UPDATE_ENABLED="${PROBE_AUTO_UPDATE_ENABLED:-false}"
    PROBE_AUTO_UPDATE_INTERVAL="${PROBE_AUTO_UPDATE_INTERVAL:-$AUTO_UPDATE_INTERVAL_DEFAULT}"
    PROBE_AUTO_UPDATE_REPO="${PROBE_AUTO_UPDATE_REPO:-$AUTO_UPDATE_REPO_DEFAULT}"
    PROBE_UPDATE_LAST_CHECKED_AT="${PROBE_UPDATE_LAST_CHECKED_AT:-0}"
    PROBE_UPDATE_LAST_VERSION="${PROBE_UPDATE_LAST_VERSION:-}"
    PROBE_UPDATE_LAST_ERROR="${PROBE_UPDATE_LAST_ERROR:-}"
}

save_update_state() {
    ensure_private_dir "$(dirname "$UPDATE_STATE_FILE")"
    jq -n \
        --arg enabled "${PROBE_AUTO_UPDATE_ENABLED:-false}" \
        --argjson interval "${PROBE_AUTO_UPDATE_INTERVAL:-$AUTO_UPDATE_INTERVAL_DEFAULT}" \
        --arg repo "${PROBE_AUTO_UPDATE_REPO:-$AUTO_UPDATE_REPO_DEFAULT}" \
        --argjson last_checked_at "${PROBE_UPDATE_LAST_CHECKED_AT:-0}" \
        --arg last_version "${PROBE_UPDATE_LAST_VERSION:-}" \
        --arg last_error "${PROBE_UPDATE_LAST_ERROR:-}" \
        '{
            enabled: ($enabled == "true"),
            interval: $interval,
            repo: $repo,
            last_checked_at: $last_checked_at,
            last_version: $last_version,
            last_error: $last_error
        }' > "$UPDATE_STATE_FILE"
    chmod 600 "$UPDATE_STATE_FILE" >/dev/null 2>&1 || true
}

get_local_v2node_version() {
    if [[ -x /usr/local/v2node/v2node ]]; then
        /usr/local/v2node/v2node version 2>/dev/null | awk 'NR==1 {print $2}' | cut -c 1-64
        return 0
    fi

    printf ''
}

get_latest_release_version() {
    local repo="${1:-$AUTO_UPDATE_REPO_DEFAULT}"
    curl -fsSL \
        -H 'Accept: application/vnd.github+json' \
        -H 'User-Agent: v2node-probe' \
        "https://api.github.com/repos/${repo}/releases/latest" \
        | jq -r '(.tag_name // .name // "") | ltrimstr("v")' \
        | tr -d '\r' \
        | cut -c 1-64
}

version_is_newer() {
    local current_version="$1"
    local latest_version="$2"

    if [[ -z "$current_version" || -z "$latest_version" || "$current_version" == "$latest_version" ]]; then
        return 1
    fi

    [[ "$(printf '%s\n%s\n' "$current_version" "$latest_version" | sort -V | tail -n 1)" == "$latest_version" ]]
}

maybe_auto_update() {
    local auto_update_json="$1"
    local enabled interval repo now current_version latest_version update_log_file update_error
    local probe_script_updated=0

    enabled=$(printf '%s' "$auto_update_json" | jq -r '.enabled // false')
    interval=$(printf '%s' "$auto_update_json" | jq -r '.interval_seconds // 86400')
    repo=$(printf '%s' "$auto_update_json" | jq -r '.repo // "YeJianbo/v2node"')
    now=$(date +%s)

    PROBE_AUTO_UPDATE_ENABLED="$enabled"
    PROBE_AUTO_UPDATE_INTERVAL="$interval"
    PROBE_AUTO_UPDATE_REPO="$repo"

    if [[ "$enabled" != "true" ]]; then
        save_update_state
        return 1
    fi

    if ! [[ "$interval" =~ ^[0-9]+$ ]] || (( interval < 300 )); then
        interval=$AUTO_UPDATE_INTERVAL_DEFAULT
        PROBE_AUTO_UPDATE_INTERVAL="$interval"
    fi

    if (( now - ${PROBE_UPDATE_LAST_CHECKED_AT:-0} < interval )); then
        save_update_state
        return 1
    fi

    current_version=$(get_local_v2node_version)
    PROBE_UPDATE_LAST_CHECKED_AT="$now"
    PROBE_UPDATE_LAST_ERROR=""

    if update_probe_script "$repo"; then
        probe_script_updated=1
    fi

    if ! latest_version=$(get_latest_release_version "$repo"); then
        PROBE_UPDATE_LAST_ERROR="检查最新版本失败"
        save_update_state
        if [[ "$probe_script_updated" == "1" ]]; then
            restart_probe_service || true
        fi
        return 1
    fi

    PROBE_UPDATE_LAST_VERSION="$latest_version"
    save_update_state

    if ! version_is_newer "$current_version" "$latest_version"; then
        if [[ "$probe_script_updated" == "1" ]]; then
            restart_probe_service || true
        fi
        return 1
    fi

    log "检测到 v2node 新版本 ${latest_version}，当前 ${current_version:-unknown}，开始自动更新"
    update_log_file=$(mktemp)

    if /usr/bin/v2node update >"$update_log_file" 2>&1; then
        log "v2node 自动更新命令已执行"
        rm -f "$update_log_file"
        if [[ "$probe_script_updated" == "1" ]]; then
            restart_probe_service || true
        fi
        return 0
    fi

    update_error=$(tail -n 1 "$update_log_file" 2>/dev/null | cut -c 1-220)
    rm -f "$update_log_file"
    PROBE_UPDATE_LAST_ERROR="${update_error:-自动更新失败}"
    save_update_state
    fail "${PROBE_UPDATE_LAST_ERROR}"
    if [[ "$probe_script_updated" == "1" ]]; then
        restart_probe_service || true
    fi
    return 1
}

update_probe_script() {
    local repo="$1"
    local url tmp_file

    repo="${repo:-$AUTO_UPDATE_REPO_DEFAULT}"
    url="https://raw.githubusercontent.com/${repo}/main/script/v2node-probe.sh"
    tmp_file=$(mktemp)

    if ! curl -fsSL --connect-timeout 8 --max-time 30 "$url" -o "$tmp_file"; then
        rm -f "$tmp_file"
        return 1
    fi

    if ! bash -n "$tmp_file"; then
        rm -f "$tmp_file"
        fail "下载到的探针脚本语法校验失败"
        return 1
    fi

    if cmp -s "$tmp_file" "$0"; then
        rm -f "$tmp_file"
        return 1
    fi

    install -m 0755 "$tmp_file" "$0"
    rm -f "$tmp_file"
    log "已更新探针脚本 $0"
    return 0
}

restart_probe_service() {
    log "探针脚本已更新，准备重启探针服务"

    if command -v systemctl >/dev/null 2>&1 && timeout 3 systemctl list-units >/dev/null 2>&1; then
        systemctl restart v2node-probe >/dev/null 2>&1 || true
        exit 0
    fi

    if command -v rc-service >/dev/null 2>&1; then
        rc-service v2node-probe restart >/dev/null 2>&1 || true
        exit 0
    fi

    return 0
}

sha256_hex() {
    openssl dgst -sha256 -binary | od -An -tx1 | tr -d ' \n'
}

hmac_sha256_hex() {
    local secret="$1"
    openssl dgst -sha256 -hmac "$secret" -binary | od -An -tx1 | tr -d ' \n'
}

signed_get() {
    local path="$1"
    local query="${2:-}"
    local timestamp
    local nonce
    local body_hash
    local payload
    local signature
    local url

    timestamp=$(date +%s)
    nonce="${timestamp}-$$-${RANDOM}-${RANDOM}"
    body_hash=$(printf '' | sha256_hex)
    payload=$(printf 'GET\n%s\n%s\n%s\n%s' "$path" "$timestamp" "$nonce" "$body_hash")
    signature=$(printf '%s' "$payload" | hmac_sha256_hex "$MACHINE_TOKEN")
    url="${PANEL_URL}${path}"
    if [[ -n "$query" ]]; then
        url="${url}?${query}"
    fi

    local output
    local curl_status=0
    local curl_error_file
    curl_error_file=$(mktemp)
    output=$(curl -fsSL --connect-timeout 5 --max-time 8 \
        -H "X-V2Node-Machine-Id: ${MACHINE_ID}" \
        -H "X-V2Node-Timestamp: ${timestamp}" \
        -H "X-V2Node-Nonce: ${nonce}" \
        -H "X-V2Node-Signature: ${signature}" \
        -H "Connection: close" \
        "$url" 2>"$curl_error_file") || curl_status=$?

    if [[ "$curl_status" -ne 0 && -z "$output" ]]; then
        cat "$curl_error_file" >&2
        rm -f "$curl_error_file"
        return "$curl_status"
    fi

    rm -f "$curl_error_file"
    printf '%s' "$output"
}

signed_post_json() {
    local path="$1"
    local body="$2"
    local timestamp
    local nonce
    local body_hash
    local payload
    local signature

    timestamp=$(date +%s)
    nonce="${timestamp}-$$-${RANDOM}-${RANDOM}"
    body_hash=$(printf '%s' "$body" | sha256_hex)
    payload=$(printf 'POST\n%s\n%s\n%s\n%s' "$path" "$timestamp" "$nonce" "$body_hash")
    signature=$(printf '%s' "$payload" | hmac_sha256_hex "$MACHINE_TOKEN")

    local output
    local curl_status=0
    local curl_error_file
    curl_error_file=$(mktemp)
    output=$(curl -fsSL --connect-timeout 5 --max-time 8 \
        -X POST \
        -H "Content-Type: application/json" \
        -H "X-V2Node-Machine-Id: ${MACHINE_ID}" \
        -H "X-V2Node-Timestamp: ${timestamp}" \
        -H "X-V2Node-Nonce: ${nonce}" \
        -H "X-V2Node-Signature: ${signature}" \
        -H "Connection: close" \
        --data "$body" \
        "${PANEL_URL}${path}" 2>"$curl_error_file") || curl_status=$?

    if [[ "$curl_status" -ne 0 && -z "$output" ]]; then
        cat "$curl_error_file" >&2
        rm -f "$curl_error_file"
        return "$curl_status"
    fi

    rm -f "$curl_error_file"
    printf '%s' "$output"
}

cloudflare_api() {
    local method="$1"
    local path="$2"
    local token="$3"
    local body="${4:-}"
    local url="https://api.cloudflare.com/client/v4${path}"

    if [[ -n "$body" ]]; then
        curl -fsSL --connect-timeout 5 --max-time 15 \
            -X "$method" \
            -H "Authorization: Bearer ${token}" \
            -H "Content-Type: application/json" \
            "$url" \
            --data "$body"
        return
    fi

    curl -fsSL --connect-timeout 5 --max-time 15 \
        -X "$method" \
        -H "Authorization: Bearer ${token}" \
        -H "Content-Type: application/json" \
        "$url"
}

sync_ddns() {
    local ddns_json="$1"
    local enabled provider zone_name record_name host record_type ttl proxied api_token current_ip

    enabled=$(printf '%s' "$ddns_json" | jq -r '.enabled // false')
    if [[ "$enabled" != "true" ]]; then
        DDNS_LAST_ERROR=""
        save_ddns_state
        return 0
    fi

    provider=$(printf '%s' "$ddns_json" | jq -r '.provider // "cloudflare"')
    zone_name=$(printf '%s' "$ddns_json" | jq -r '.zone_name // ""')
    record_name=$(printf '%s' "$ddns_json" | jq -r '.record_name // ""')
    host=$(printf '%s' "$ddns_json" | jq -r '.host // ""')
    record_type=$(printf '%s' "$ddns_json" | jq -r '.record_type // "A"')
    ttl=$(printf '%s' "$ddns_json" | jq -r '.ttl // 120')
    proxied=$(printf '%s' "$ddns_json" | jq -r '.proxied // false')
    api_token=$(printf '%s' "$ddns_json" | jq -r '.api_token // ""')
    current_ip=$(printf '%s' "$ddns_json" | jq -r '.current_ip // ""')

    if [[ "$provider" != "cloudflare" ]]; then
        DDNS_LAST_ERROR="暂不支持 ${provider} DDNS"
        save_ddns_state
        return 1
    fi

    if [[ -z "$zone_name" || -z "$record_name" || -z "$host" || -z "$api_token" || -z "$current_ip" ]]; then
        DDNS_LAST_ERROR="DDNS 配置不完整"
        save_ddns_state
        return 1
    fi

    local zone_response record_response zone_id record_id record_body now_ts
    now_ts=$(date +%s)

    if [[ "$DDNS_LAST_IP" == "$current_ip" && "$DDNS_LAST_HOST" == "$host" && -n "$DDNS_RECORD_ID" && -n "$DDNS_ZONE_ID" ]]; then
        DDNS_LAST_SYNCED_AT="$now_ts"
        DDNS_LAST_ERROR=""
        save_ddns_state
        return 0
    fi

    zone_id="$DDNS_ZONE_ID"
    if [[ -z "$zone_id" || "$DDNS_LAST_HOST" != "$host" ]]; then
        zone_response=$(cloudflare_api GET "/zones?name=${zone_name}" "$api_token") || {
            DDNS_LAST_ERROR="获取 Cloudflare Zone 失败"
            save_ddns_state
            return 1
        }
        zone_id=$(printf '%s' "$zone_response" | jq -r '.result[0].id // ""')
        if [[ -z "$zone_id" ]]; then
            DDNS_LAST_ERROR="未找到 Cloudflare Zone"
            save_ddns_state
            return 1
        fi
    fi

    record_id="$DDNS_RECORD_ID"
    if [[ -z "$record_id" || "$DDNS_LAST_HOST" != "$host" || "$DDNS_ZONE_ID" != "$zone_id" ]]; then
        record_response=$(cloudflare_api GET "/zones/${zone_id}/dns_records?type=${record_type}&name=${host}" "$api_token") || {
            DDNS_LAST_ERROR="获取 Cloudflare 记录失败"
            DDNS_ZONE_ID="$zone_id"
            save_ddns_state
            return 1
        }
        record_id=$(printf '%s' "$record_response" | jq -r '.result[0].id // ""')
        if [[ -z "$record_id" ]]; then
            DDNS_LAST_ERROR="未找到 Cloudflare 记录"
            DDNS_ZONE_ID="$zone_id"
            save_ddns_state
            return 1
        fi
    fi

    record_body=$(jq -nc \
        --arg type "$record_type" \
        --arg name "$host" \
        --arg content "$current_ip" \
        --argjson ttl "${ttl:-120}" \
        --argjson proxied "$([[ "$proxied" == "true" ]] && echo true || echo false)" \
        '{type:$type, name:$name, content:$content, ttl:$ttl, proxied:$proxied}')

    cloudflare_api PUT "/zones/${zone_id}/dns_records/${record_id}" "$api_token" "$record_body" >/dev/null || {
        DDNS_ZONE_ID="$zone_id"
        DDNS_RECORD_ID="$record_id"
        DDNS_LAST_ERROR="更新 Cloudflare 记录失败"
        save_ddns_state
        return 1
    }

    DDNS_ZONE_ID="$zone_id"
    DDNS_RECORD_ID="$record_id"
    DDNS_LAST_HOST="$host"
    DDNS_LAST_IP="$current_ip"
    DDNS_LAST_SYNCED_AT="$now_ts"
    DDNS_LAST_ERROR=""
    save_ddns_state
    return 0
}

read_cpu_percent() {
    local cpu user nice system idle iowait irq softirq steal total idle_all
    local prev_total prev_idle next_total next_idle diff_total diff_idle

    read -r cpu user nice system idle iowait irq softirq steal _ < /proc/stat || {
        echo 0
        return
    }
    prev_idle=$((idle + iowait))
    prev_total=$((user + nice + system + idle + iowait + irq + softirq + steal))
    sleep 1
    read -r cpu user nice system idle iowait irq softirq steal _ < /proc/stat || {
        echo 0
        return
    }
    next_idle=$((idle + iowait))
    next_total=$((user + nice + system + idle + iowait + irq + softirq + steal))
    diff_total=$((next_total - prev_total))
    diff_idle=$((next_idle - prev_idle))

    if [[ "$diff_total" -le 0 ]]; then
        echo 0
        return
    fi

    echo $(( (100 * (diff_total - diff_idle)) / diff_total ))
}

read_mem_percent() {
    awk '
        /^MemTotal:/ { total=$2 }
        /^MemAvailable:/ { available=$2 }
        END {
            if (total > 0) {
                printf "%d", ((total - available) * 100 / total)
            } else {
                printf "0"
            }
        }
    ' /proc/meminfo 2>/dev/null
}

read_mem_total_bytes() {
    awk '/^MemTotal:/ { printf "%d", $2 * 1024 }' /proc/meminfo 2>/dev/null
}

read_mem_used_bytes() {
    awk '
        /^MemTotal:/ { total=$2 }
        /^MemAvailable:/ { available=$2 }
        END {
            if (total > 0 && available >= 0) {
                printf "%d", (total - available) * 1024
            } else {
                printf "0"
            }
        }
    ' /proc/meminfo 2>/dev/null
}

read_swap_total_bytes() {
    awk '/^SwapTotal:/ { printf "%d", $2 * 1024 }' /proc/meminfo 2>/dev/null
}

read_swap_used_bytes() {
    awk '
        /^SwapTotal:/ { total=$2 }
        /^SwapFree:/ { free=$2 }
        END {
            if (total > 0 && free >= 0) {
                printf "%d", (total - free) * 1024
            } else {
                printf "0"
            }
        }
    ' /proc/meminfo 2>/dev/null
}

read_swap_percent() {
    awk '
        /^SwapTotal:/ { total=$2 }
        /^SwapFree:/ { free=$2 }
        END {
            if (total > 0 && free >= 0) {
                printf "%d", ((total - free) * 100 / total)
            } else {
                printf "0"
            }
        }
    ' /proc/meminfo 2>/dev/null
}

read_disk_percent() {
    df -P / 2>/dev/null | awk 'NR==2 { gsub(/%/, "", $5); print int($5) }'
}

read_disk_total_bytes() {
    df -P / 2>/dev/null | awk 'NR==2 { printf "%d", $2 * 1024 }'
}

read_disk_used_bytes() {
    df -P / 2>/dev/null | awk 'NR==2 { printf "%d", $3 * 1024 }'
}

read_net_bytes() {
    awk -F'[: ]+' '
        $1 !~ /lo$/ && NF >= 17 {
            rx += $3
            tx += $11
        }
        END {
            printf "%d %d", rx, tx
        }
    ' /proc/net/dev 2>/dev/null
}

read_cpu_cores() {
    if command -v nproc >/dev/null 2>&1; then
        nproc 2>/dev/null
        return
    fi

    awk '/^processor[[:space:]]*:/ { count++ } END { print count > 0 ? count : 0 }' /proc/cpuinfo 2>/dev/null
}

read_cpu_model() {
    awk -F': ' '
        /^model name[[:space:]]*:/ { print $2; exit }
        /^Hardware[[:space:]]*:/ { print $2; exit }
        /^Processor[[:space:]]*:/ { print $2; exit }
    ' /proc/cpuinfo 2>/dev/null | cut -c 1-120
}

read_os_name() {
    if [[ -r /etc/os-release ]]; then
        awk -F= '
            /^PRETTY_NAME=/ {
                gsub(/^"/, "", $2)
                gsub(/"$/, "", $2)
                print $2
                exit
            }
        ' /etc/os-release 2>/dev/null | cut -c 1-120
        return
    fi

    uname -s 2>/dev/null
}

read_process_count() {
    if command -v ps >/dev/null 2>&1; then
        ps -e --no-headers 2>/dev/null | wc -l | awk '{print int($1)}'
        return
    fi

    find /proc -maxdepth 1 -type d -regex '.*/[0-9]+' 2>/dev/null | wc -l | awk '{print int($1)}'
}

read_virtualization() {
    if [[ -f /.dockerenv ]]; then
        printf 'docker'
        return
    fi

    if command -v systemd-detect-virt >/dev/null 2>&1; then
        systemd-detect-virt 2>/dev/null | head -n 1 | cut -c 1-40
        return
    fi

    if grep -qaE 'docker|lxc|kubepods|containerd' /proc/1/cgroup 2>/dev/null; then
        printf 'container'
        return
    fi

    printf 'none'
}

read_tcp_congestion_control() {
    if command -v sysctl >/dev/null 2>&1; then
        sysctl -n net.ipv4.tcp_congestion_control 2>/dev/null | head -n 1 | cut -c 1-40
        return
    fi

    if [[ -r /proc/sys/net/ipv4/tcp_congestion_control ]]; then
        head -n 1 /proc/sys/net/ipv4/tcp_congestion_control 2>/dev/null | cut -c 1-40
        return
    fi

    printf ''
}

read_connection_json() {
    if ! command -v ss >/dev/null 2>&1; then
        printf '{"tcp_conn":0,"tcp_established":0,"udp_conn":0}'
        return
    fi

    local tcp_conn tcp_established udp_conn
    tcp_conn=$(ss -H -tan 2>/dev/null | wc -l | awk '{print int($1)}')
    tcp_established=$(ss -H -tan state established 2>/dev/null | wc -l | awk '{print int($1)}')
    udp_conn=$(ss -H -uan 2>/dev/null | wc -l | awk '{print int($1)}')
    printf '{"tcp_conn":%d,"tcp_established":%d,"udp_conn":%d}' "${tcp_conn:-0}" "${tcp_established:-0}" "${udp_conn:-0}"
}

read_load_json() {
    awk '{ printf "{\"load1\":%s,\"load5\":%s,\"load15\":%s}", $1, $2, $3 }' /proc/loadavg 2>/dev/null
}

read_service_status() {
    local name="$1"

    if command -v systemctl >/dev/null 2>&1 && timeout 3 systemctl list-units >/dev/null 2>&1; then
        local status
        status=$(systemctl is-active "$name" 2>/dev/null || true)
        printf '%s' "${status:-unknown}"
        return
    fi

    if command -v service >/dev/null 2>&1; then
        if service "$name" status >/dev/null 2>&1; then
            printf 'active'
        else
            printf 'inactive'
        fi
        return
    fi

    if pgrep -x "$name" >/dev/null 2>&1; then
        printf 'active'
        return
    fi

    printf 'unknown'
}

read_docker_status_json() {
    if ! command -v docker >/dev/null 2>&1; then
        printf '{"docker_total":0,"docker_running":0,"docker_images":0}'
        return
    fi

    local total running images
    total=$(docker ps -a -q 2>/dev/null | wc -l | awk '{print int($1)}')
    running=$(docker ps -q 2>/dev/null | wc -l | awk '{print int($1)}')
    images=$(docker images -q 2>/dev/null | sort -u | wc -l | awk '{print int($1)}')
    printf '{"docker_total":%d,"docker_running":%d,"docker_images":%d}' "${total:-0}" "${running:-0}" "${images:-0}"
}

read_docker_summary() {
    if ! command -v docker >/dev/null 2>&1; then
        printf ''
        return
    fi

    docker ps --format '{{.Names}} {{.Status}}' 2>/dev/null \
        | head -n 5 \
        | paste -sd '; ' - \
        | cut -c 1-180
}

read_listen_ports() {
    if command -v ss >/dev/null 2>&1; then
        ss -H -lntu 2>/dev/null | awk '
            {
                proto=$1
                addr=$5
                gsub(/^\[/, "", addr)
                gsub(/\]$/, "", addr)
                n=split(addr, parts, ":")
                port=parts[n]
                if (port ~ /^[0-9]+$/) {
                    key=tolower(proto) ":" port
                    if (!seen[key]++) values[++count]=key
                }
            }
            END {
                for (i=1; i<=count && i<=40; i++) {
                    printf "%s%s", i == 1 ? "" : ",", values[i]
                }
            }
        '
        return
    fi

    printf ''
}

read_listen_processes() {
    if command -v ss >/dev/null 2>&1; then
        ss -H -lntup 2>/dev/null | awk '
            {
                proto=tolower($1)
                addr=$5
                gsub(/^\[/, "", addr)
                gsub(/\]$/, "", addr)
                n=split(addr, parts, ":")
                port=parts[n]
                if (port !~ /^[0-9]+$/) next

                raw=$0
                proc=""
                if (raw ~ /users:\(\("[^"]+"/) {
                    sub(/^.*users:\(\("/, "", raw)
                    sub(/".*$/, "", raw)
                    proc=raw
                }

                rawPid=$0
                pid=""
                if (rawPid ~ /pid=[0-9]+/) {
                    sub(/^.*pid=/, "", rawPid)
                    sub(/[^0-9].*$/, "", rawPid)
                    pid=rawPid
                }

                if (proc == "") proc="unknown"
                key=proto ":" port ":" proc ":" pid
                if (!seen[key]++) values[++count]=key
            }
            END {
                for (i=1; i<=count && i<=80; i++) {
                    printf "%s%s", i == 1 ? "" : ",", values[i]
                }
            }
        '
        return
    fi

    printf ''
}

is_global_ipv4() {
    local ip="$1"

    [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
    awk -v ip="$ip" '
        BEGIN {
            split(ip, p, ".")
            for (i = 1; i <= 4; i++) {
                if (p[i] !~ /^[0-9]+$/ || p[i] < 0 || p[i] > 255) exit 1
            }
            if (p[1] == 0 || p[1] == 10 || p[1] == 127) exit 1
            if (p[1] == 169 && p[2] == 254) exit 1
            if (p[1] == 172 && p[2] >= 16 && p[2] <= 31) exit 1
            if (p[1] == 192 && p[2] == 168) exit 1
            if (p[1] == 100 && p[2] >= 64 && p[2] <= 127) exit 1
            if (p[1] == 198 && (p[2] == 18 || p[2] == 19)) exit 1
            if (p[1] >= 224) exit 1
            exit 0
        }
    '
}

is_global_ipv6() {
    local ip="$1"
    local lower

    [[ "$ip" == *:* ]] || return 1
    lower=$(printf '%s' "$ip" | tr 'A-Z' 'a-z')
    [[ "$lower" == "::1" ]] && return 1
    [[ "$lower" == fe80:* ]] && return 1
    [[ "$lower" == fc* || "$lower" == fd* ]] && return 1
    [[ "$lower" == ff* ]] && return 1
    return 0
}

read_local_ipv4() {
    local ip
    ip=$(hostname -I 2>/dev/null | tr ' ' '\n' | awk '
        /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }
    ')
    if is_global_ipv4 "$ip"; then
        printf '%s' "$ip"
        return
    fi

    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1; i<=NF; i++) if ($i == "src") {print $(i+1); exit}}')
    if is_global_ipv4 "$ip"; then
        printf '%s' "$ip"
        return
    fi

    printf ''
}

read_local_ipv6() {
    local ip
    ip=$(hostname -I 2>/dev/null | tr ' ' '\n' | awk '/:/ { print; exit }')
    if is_global_ipv6 "$ip"; then
        printf '%s' "$ip"
        return
    fi

    ip=$(ip -6 route get 2606:4700:4700::1111 2>/dev/null | awk '{for (i=1; i<=NF; i++) if ($i == "src") {print $(i+1); exit}}')
    if is_global_ipv6 "$ip"; then
        printf '%s' "$ip"
        return
    fi

    printf ''
}

read_public_ip() {
    local family="$1"
    local ip

    if ! command -v curl >/dev/null 2>&1; then
        printf ''
        return
    fi

    if [[ "$family" == "4" ]]; then
        ip=$(curl -4fsS --max-time 3 https://api.ipify.org 2>/dev/null | tr -d '[:space:]' | cut -c 1-64 || true)
        if is_global_ipv4 "$ip"; then
            printf '%s' "$ip"
            return
        fi
    elif [[ "$family" == "6" ]]; then
        ip=$(curl -6fsS --max-time 3 https://api6.ipify.org 2>/dev/null | tr -d '[:space:]' | cut -c 1-128 || true)
        if is_global_ipv6 "$ip"; then
            printf '%s' "$ip"
            return
        fi
    fi

    printf ''
}

read_primary_ip() {
    local public_ipv4="$1"
    local public_ipv6="$2"
    local local_ipv4="$3"
    local local_ipv6="$4"

    if [[ -n "$public_ipv4" ]]; then
        printf '%s' "$public_ipv4"
    elif [[ -n "$public_ipv6" ]]; then
        printf '%s' "$public_ipv6"
    elif [[ -n "$local_ipv4" ]]; then
        printf '%s' "$local_ipv4"
    else
        printf '%s' "$local_ipv6"
    fi
}

push_status() {
    load_state || return 1
    ensure_dependencies || return 1

    local cpu mem disk uptime version net_rx net_tx primary_ip body gost_version gost_rule_count
    local v2node_version probe_update_checked_at probe_update_latest_version probe_update_error
    local local_ipv4 local_ipv6 public_ipv4 public_ipv6
    local mem_total mem_used swap_total swap_used swap_percent disk_total disk_used cpu_cores cpu_model os_name kernel arch
    local load_json docker_json connection_json v2node_status gost_status listen_ports listen_processes process_count virtualization tcp_cc docker_summary
    cpu=$(read_cpu_percent)
    mem=$(read_mem_percent)
    disk=$(read_disk_percent)
    mem_total=$(read_mem_total_bytes)
    mem_used=$(read_mem_used_bytes)
    swap_total=$(read_swap_total_bytes)
    swap_used=$(read_swap_used_bytes)
    swap_percent=$(read_swap_percent)
    disk_total=$(read_disk_total_bytes)
    disk_used=$(read_disk_used_bytes)
    cpu_cores=$(read_cpu_cores)
    cpu_model=$(read_cpu_model)
    os_name=$(read_os_name)
    kernel=$(uname -r 2>/dev/null | cut -c 1-80)
    arch=$(uname -m 2>/dev/null | cut -c 1-40)
    process_count=$(read_process_count)
    virtualization=$(read_virtualization)
    tcp_cc=$(read_tcp_congestion_control)
    load_json=$(read_load_json)
    if [[ -z "$load_json" ]]; then
        load_json='{"load1":0,"load5":0,"load15":0}'
    fi
    connection_json=$(read_connection_json)
    if [[ -z "$connection_json" ]]; then
        connection_json='{"tcp_conn":0,"tcp_established":0,"udp_conn":0}'
    fi
    docker_json=$(read_docker_status_json)
    if [[ -z "$docker_json" ]]; then
        docker_json='{"docker_total":0,"docker_running":0,"docker_images":0}'
    fi
    docker_summary=$(read_docker_summary)
    v2node_status=$(read_service_status v2node)
    gost_status=$(read_service_status gost)
    listen_ports=$(read_listen_ports)
    listen_processes=$(read_listen_processes)
    uptime=$(cut -d' ' -f1 /proc/uptime 2>/dev/null | cut -d'.' -f1)
    version="v2node-probe $(uname -s 2>/dev/null) $(uname -m 2>/dev/null)"
    v2node_version=$(get_local_v2node_version)
    local_ipv4=$(read_local_ipv4)
    local_ipv6=$(read_local_ipv6)
    public_ipv4=$(read_public_ip 4)
    public_ipv6=$(read_public_ip 6)
    primary_ip=$(read_primary_ip "$public_ipv4" "$public_ipv6" "$local_ipv4" "$local_ipv6")
    read -r net_rx net_tx <<< "$(read_net_bytes)"
    gost_version=$(get_gost_version)
    probe_update_checked_at="${PROBE_UPDATE_LAST_CHECKED_AT:-0}"
    probe_update_latest_version="${PROBE_UPDATE_LAST_VERSION:-}"
    probe_update_error="${PROBE_UPDATE_LAST_ERROR:-}"
    gost_rule_count=0
    if [[ -f "$GOST_CONFIG_FILE" ]]; then
        gost_rule_count=$(jq -r '(.ServeNodes // []) | length' "$GOST_CONFIG_FILE" 2>/dev/null || echo 0)
    fi

    body=$(jq -nc \
        --argjson cpu "${cpu:-0}" \
        --argjson mem "${mem:-0}" \
        --argjson disk "${disk:-0}" \
        --argjson mem_total "${mem_total:-0}" \
        --argjson mem_used "${mem_used:-0}" \
        --argjson swap_total "${swap_total:-0}" \
        --argjson swap_used "${swap_used:-0}" \
        --argjson swap_percent "${swap_percent:-0}" \
        --argjson disk_total "${disk_total:-0}" \
        --argjson disk_used "${disk_used:-0}" \
        --argjson cpu_cores "${cpu_cores:-0}" \
        --arg cpu_model "${cpu_model:-}" \
        --arg os "${os_name:-}" \
        --arg kernel "${kernel:-}" \
        --arg arch "${arch:-}" \
        --argjson process_count "${process_count:-0}" \
        --arg virtualization "${virtualization:-}" \
        --arg tcp_cc "${tcp_cc:-}" \
        --argjson load "$load_json" \
        --argjson conn "$connection_json" \
        --argjson docker "$docker_json" \
        --arg docker_summary "${docker_summary:-}" \
        --argjson net_rx "${net_rx:-0}" \
        --argjson net_tx "${net_tx:-0}" \
        --argjson uptime "${uptime:-0}" \
        --arg ip "$primary_ip" \
        --arg ipv4 "${local_ipv4:-}" \
        --arg ipv6 "${local_ipv6:-}" \
        --arg public_ipv4 "${public_ipv4:-}" \
        --arg public_ipv6 "${public_ipv6:-}" \
        --arg primary_ipv4 "${public_ipv4:-$local_ipv4}" \
        --arg primary_ipv6 "${public_ipv6:-$local_ipv6}" \
        --arg version "$version" \
        --arg v2node_version "${v2node_version:-}" \
        --arg ddns_host "${DDNS_LAST_HOST:-}" \
        --arg ddns_synced_ip "${DDNS_LAST_IP:-}" \
        --argjson ddns_synced_at "${DDNS_LAST_SYNCED_AT:-0}" \
        --arg ddns_error "${DDNS_LAST_ERROR:-}" \
        --arg gost_version "${gost_version:-}" \
        --argjson gost_rule_count "${gost_rule_count:-0}" \
        --arg probe_auto_update "${PROBE_AUTO_UPDATE_ENABLED:-false}" \
        --argjson probe_update_checked_at "${probe_update_checked_at:-0}" \
        --arg probe_update_latest_version "${probe_update_latest_version:-}" \
        --arg probe_update_error "${probe_update_error:-}" \
        --arg v2node_status "${v2node_status:-unknown}" \
        --arg gost_status "${gost_status:-unknown}" \
        --arg listen_ports "${listen_ports:-}" \
        --arg listen_processes "${listen_processes:-}" \
        '{
            cpu:$cpu,
            mem:$mem,
            disk:$disk,
            mem_total:$mem_total,
            mem_used:$mem_used,
            swap_total:$swap_total,
            swap_used:$swap_used,
            swap_percent:$swap_percent,
            disk_total:$disk_total,
            disk_used:$disk_used,
            cpu_cores:$cpu_cores,
            cpu_model:$cpu_model,
            os:$os,
            kernel:$kernel,
            arch:$arch,
            process_count:$process_count,
            virtualization:$virtualization,
            tcp_congestion_control:$tcp_cc,
            bbr_status:(if $tcp_cc == "bbr" then "enabled" elif $tcp_cc == "" then "unknown" else "disabled" end),
            load1:($load.load1 // 0),
            load5:($load.load5 // 0),
            load15:($load.load15 // 0),
            tcp_conn:($conn.tcp_conn // 0),
            tcp_established:($conn.tcp_established // 0),
            udp_conn:($conn.udp_conn // 0),
            docker_total:($docker.docker_total // 0),
            docker_running:($docker.docker_running // 0),
            docker_images:($docker.docker_images // 0),
            docker_summary:$docker_summary,
            net_rx:$net_rx,
            net_tx:$net_tx,
            uptime:$uptime,
            ip:$ip,
            ipv4:$ipv4,
            ipv6:$ipv6,
            public_ipv4:$public_ipv4,
            public_ipv6:$public_ipv6,
            primary_ipv4:$primary_ipv4,
            primary_ipv6:$primary_ipv6,
            version:$version,
            v2node_version:$v2node_version,
            ddns_host:$ddns_host,
            ddns_synced_ip:$ddns_synced_ip,
            ddns_synced_at:$ddns_synced_at,
            ddns_error:$ddns_error,
            gost_version:$gost_version,
            gost_rule_count:$gost_rule_count,
            probe_auto_update:($probe_auto_update == "true"),
            probe_update_checked_at:$probe_update_checked_at,
            probe_update_latest_version:$probe_update_latest_version,
            probe_update_error:$probe_update_error,
            v2node_status:$v2node_status,
            gost_status:$gost_status,
            listen_ports:$listen_ports,
            listen_processes:$listen_processes
        }')

    signed_post_json "/api/v1/server/machine/push" "$body" >/dev/null
}

write_config() {
    local nodes_json="$1"
    local tmp_file
    local existing_config_file
    local managed_state_file
    local next_state_file
    CONFIG_CHANGED=0
    tmp_file=$(mktemp)
    existing_config_file=$(mktemp)
    managed_state_file=$(mktemp)
    next_state_file=$(mktemp)

    if decrypt_config_to_file "$existing_config_file" && jq -e 'type == "object"' "$existing_config_file" >/dev/null 2>&1; then
        :
    else
        seed_plain_config_file "$existing_config_file"
    fi

    if [[ -f "$MANAGED_NODES_STATE_FILE" ]] && jq -e 'type == "array"' "$MANAGED_NODES_STATE_FILE" >/dev/null 2>&1; then
        cp "$MANAGED_NODES_STATE_FILE" "$managed_state_file"
    else
        printf '[]\n' > "$managed_state_file"
    fi

    if ! jq -n \
        --slurpfile existing "$existing_config_file" \
        --slurpfile previous_managed "$managed_state_file" \
        --argjson desired_nodes "$nodes_json" \
        '
        def normalized_api_host:
            (.ApiHost // .api_host // "" | tostring | ascii_downcase | sub("/+$"; ""));
        def normalized_node_id:
            ((.NodeID // .node_id // 0) | tonumber? // 0);
        def node_key:
            [
                normalized_api_host,
                (normalized_node_id | tostring)
            ] | join("#");
        def node_identity:
            normalized_node_id as $id
            | if $id > 0 then ($id | tostring) else node_key end;
        def unique_by_identity:
            reduce .[] as $node ({seen:{}, out:[]};
                ($node | node_identity) as $key
                | if $key == "" or (.seen[$key] // false) then .
                  else (.seen[$key] = true) | (.out += [$node])
                  end
            ) | .out;

        ($existing[0] // {}) as $old
        | (($previous_managed[0] // []) | map(tostring)) as $previousKeys
        | ($previousKeys | map(split("#") | .[-1])) as $previousNodeIds
        | (($desired_nodes // []) | unique_by_identity) as $desired
        | (($old.Nodes // [])
            | map(select(
                (node_key as $key | $previousKeys | index($key)) as $matchedKey
                | (node_identity as $identity | $previousNodeIds | index($identity)) as $matchedId
                | ($matchedKey or $matchedId) | not
            ))
            | unique_by_identity) as $manualNodes
        | ($desired | map(select(node_identity as $identity | ($manualNodes | map(node_identity) | index($identity)) | not))) as $newManagedNodes
        | $old
        | .Log = (.Log // {
            Level: "warning",
            Output: "",
            Access: "none"
        })
        | .Nodes = ($manualNodes + $newManagedNodes)
        ' > "$tmp_file"; then
        rm -f "$tmp_file" "$existing_config_file" "$managed_state_file" "$next_state_file"
        fail "生成配置文件失败"
        return 1
    fi

    if ! printf '%s' "$nodes_json" | jq -c '
        def normalized_api_host:
            (.ApiHost // .api_host // "" | tostring | ascii_downcase | sub("/+$"; ""));
        def normalized_node_id:
            ((.NodeID // .node_id // 0) | tonumber? // 0);
        def node_key:
            [
                normalized_api_host,
                (normalized_node_id | tostring)
            ] | join("#");
        def node_identity:
            normalized_node_id as $id
            | if $id > 0 then ($id | tostring) else node_key end;
        map(node_identity) | unique
    ' > "$next_state_file"; then
        rm -f "$tmp_file" "$existing_config_file" "$managed_state_file" "$next_state_file"
        fail "生成探针节点状态失败"
        return 1
    fi

    if ! jq -e '
        type == "object"
        and ((.Nodes // []) | type == "array")
    ' "$tmp_file" >/dev/null; then
        rm -f "$tmp_file" "$existing_config_file" "$managed_state_file" "$next_state_file"
        fail "生成配置文件校验失败"
        return 1
    fi

    ensure_private_dir "$(dirname "$CONFIG_FILE")"
    ensure_private_dir "$(dirname "$MANAGED_NODES_STATE_FILE")"

    if cmp -s "$tmp_file" "$existing_config_file"; then
        rm -f "$tmp_file"
    else
        if ! encrypt_config_from_file "$tmp_file"; then
            rm -f "$tmp_file" "$existing_config_file" "$managed_state_file" "$next_state_file"
            fail "写入加密配置失败"
            return 1
        fi
        rm -f "$tmp_file"
        CONFIG_CHANGED=1
        log "已更新 $CONFIG_FILE"
    fi

    if [[ ! -f "$MANAGED_NODES_STATE_FILE" ]] || ! cmp -s "$next_state_file" "$MANAGED_NODES_STATE_FILE"; then
        mv "$next_state_file" "$MANAGED_NODES_STATE_FILE"
    else
        rm -f "$next_state_file"
    fi

    rm -f "$existing_config_file" "$managed_state_file"
}

restart_v2node_service() {
    log "收到重启 v2node 节点服务指令"

    if command -v systemctl >/dev/null 2>&1 && timeout 3 systemctl list-units >/dev/null 2>&1; then
        systemctl restart v2node
        return $?
    fi

    if command -v service >/dev/null 2>&1 && timeout 3 service v2node status >/dev/null 2>&1; then
        service v2node restart
        return $?
    fi

    local pids
    pids=$(ps -eo pid=,args= | awk '/\/usr\/local\/v2node\/v2node server/ && !/awk/ {print $1}')
    if [[ -n "$pids" ]]; then
        kill $pids 2>/dev/null || true
        sleep 1
    fi

    mkdir -p /var/log
    setsid /usr/local/v2node/v2node server >/var/log/v2node.log 2>&1 < /dev/null &
}

ack_restart_v2node() {
    local restart_token="$1"
    local body
    body=$(jq -nc --arg restart_token "$restart_token" '{restart_token:$restart_token}')
    signed_post_json "/api/v1/server/machine/restartAck" "$body" >/dev/null
}

ack_enable_bbr() {
    local enable_bbr_token="$1"
    local status="${2:-success}"
    local error="${3:-}"
    local body
    body=$(jq -nc \
        --arg enable_bbr_token "$enable_bbr_token" \
        --arg status "$status" \
        --arg error "$error" \
        '{enable_bbr_token:$enable_bbr_token,status:$status,error:$error}')
    signed_post_json "/api/v1/server/machine/bbrAck" "$body" >/dev/null
}

ack_connectivity_test() {
    local task_id="$1"
    local status="$2"
    local message="$3"
    local latency_ms="${4:-null}"
    local http_code="${5:-null}"
    local logs="${6:-}"
    local body

    body=$(jq -nc \
        --arg task_id "$task_id" \
        --arg status "$status" \
        --arg message "$message" \
        --argjson latency_ms "${latency_ms:-null}" \
        --argjson http_code "${http_code:-null}" \
        --arg logs "$logs" \
        '{
            task_id:$task_id,
            status:$status,
            message:$message,
            latency_ms:$latency_ms,
            http_code:$http_code,
            logs:$logs
        }')
    signed_post_json "/api/v1/server/machine/connectivityTestAck" "$body" >/dev/null
}

pick_free_tcp_port() {
    local port
    local try
    for try in $(seq 1 30); do
        port=$((38000 + (RANDOM % 20000)))
        if command -v ss >/dev/null 2>&1; then
            if ! ss -ltn "( sport = :${port} )" 2>/dev/null | grep -q ":${port}"; then
                printf '%s' "$port"
                return 0
            fi
        else
            printf '%s' "$port"
            return 0
        fi
    done

    printf '39080'
}

run_connectivity_test_task() {
    local task_json="$1"
    local task_id runner target_url timeout_seconds config_format config_base64 expected_codes_json
    local tmp_dir config_file runtime_file log_file port pid curl_status curl_error output http_code latency_ms
    local start_ms end_ms message status logs_tailed

    task_id=$(printf '%s' "$task_json" | jq -r '.task_id // ""')
    runner=$(printf '%s' "$task_json" | jq -r '.runner // "sing-box"')
    target_url=$(printf '%s' "$task_json" | jq -r '.target_url // "https://cp.cloudflare.com/generate_204"')
    timeout_seconds=$(printf '%s' "$task_json" | jq -r '(.timeout_seconds // 18) | tonumber')
    config_format=$(printf '%s' "$task_json" | jq -r '.config_format // ""')
    config_base64=$(printf '%s' "$task_json" | jq -r '.config_base64 // ""')
    expected_codes_json=$(printf '%s' "$task_json" | jq -c '.expected_http_codes // [200,204]')

    if [[ -z "$task_id" || -z "$config_base64" || "$config_format" != "sing-box" || "$runner" != "sing-box" ]]; then
        ack_connectivity_test "$task_id" "failed" "测试任务参数不完整" null null "" || true
        return 1
    fi

    ensure_singbox_binary || {
        ack_connectivity_test "$task_id" "failed" "安装 sing-box 失败" null null "" || true
        return 1
    }

    tmp_dir=$(mktemp -d)
    config_file="${tmp_dir}/base.json"
    runtime_file="${tmp_dir}/runtime.json"
    log_file="${tmp_dir}/sing-box.log"
    port=$(pick_free_tcp_port)

    if ! printf '%s' "$config_base64" | base64 -d > "$config_file" 2>/dev/null; then
        rm -rf "$tmp_dir"
        ack_connectivity_test "$task_id" "failed" "解码测试配置失败" null null "" || true
        return 1
    fi

    if ! jq --argjson port "$port" '
        .inbounds = [{
            tag: "mixed-in",
            type: "mixed",
            listen: "127.0.0.1",
            listen_port: $port
        }]
    ' "$config_file" > "$runtime_file"; then
        rm -rf "$tmp_dir"
        ack_connectivity_test "$task_id" "failed" "生成运行配置失败" null null "" || true
        return 1
    fi

    "$SINGBOX_BIN" run -D "$tmp_dir" -c "$runtime_file" >"$log_file" 2>&1 &
    pid=$!

    for _ in $(seq 1 24); do
        if ! kill -0 "$pid" >/dev/null 2>&1; then
            break
        fi
        if command -v ss >/dev/null 2>&1 && ss -ltn "( sport = :${port} )" 2>/dev/null | grep -q ":${port}"; then
            break
        fi
        sleep 0.5
    done

    if ! kill -0 "$pid" >/dev/null 2>&1; then
        logs_tailed=$(tail -n 20 "$log_file" 2>/dev/null | tr '\n' ' ' | cut -c 1-3500)
        rm -rf "$tmp_dir"
        ack_connectivity_test "$task_id" "failed" "sing-box 启动失败" null null "$logs_tailed" || true
        return 1
    fi

    start_ms=$(date +%s%3N)
    curl_error=$(mktemp)
    output=$(curl -sS -o /dev/null -w "%{http_code}" \
        --connect-timeout 6 \
        --max-time "$timeout_seconds" \
        --proxy "http://127.0.0.1:${port}" \
        "$target_url" 2>"$curl_error")
    curl_status=$?
    end_ms=$(date +%s%3N)
    latency_ms=$((end_ms - start_ms))
    http_code=0
    if [[ "$output" =~ ^[0-9]+$ ]]; then
        http_code="$output"
    fi

    logs_tailed=$(tail -n 20 "$log_file" 2>/dev/null | tr '\n' ' ' | cut -c 1-3500)

    kill "$pid" >/dev/null 2>&1 || true
    wait "$pid" >/dev/null 2>&1 || true
    rm -f "$curl_error"
    rm -rf "$tmp_dir"

    status="failed"
    message="真实协议测试失败"
    if [[ "$curl_status" -eq 0 ]] && printf '%s' "$expected_codes_json" | jq -e --argjson code "$http_code" 'index($code) != null' >/dev/null 2>&1; then
        status="success"
        message="节点机器本机真实协议测试成功"
    else
        if [[ "$http_code" -gt 0 ]]; then
            message="真实协议测试失败，HTTP ${http_code}"
        fi
    fi

    ack_connectivity_test "$task_id" "$status" "$message" "$latency_ms" "$http_code" "$logs_tailed" || true
    [[ "$status" == "success" ]]
}

enable_bbr_tuning() {
    local available current conf_file virtualization

    if [[ "$(id -u 2>/dev/null || echo 1)" != "0" ]]; then
        fail "启用 BBR 需要 root 权限"
        return 1
    fi

    virtualization=$(read_virtualization)
    if [[ "$virtualization" != "kvm" ]]; then
        fail "BBR 仅对 KVM 虚拟化机器开放，当前为 ${virtualization:-unknown}"
        return 1
    fi

    if [[ -r /proc/sys/net/ipv4/tcp_available_congestion_control ]]; then
        available=$(cat /proc/sys/net/ipv4/tcp_available_congestion_control 2>/dev/null || true)
        if ! printf '%s' "$available" | grep -qw 'bbr'; then
            modprobe tcp_bbr >/dev/null 2>&1 || true
            available=$(cat /proc/sys/net/ipv4/tcp_available_congestion_control 2>/dev/null || true)
        fi
        if ! printf '%s' "$available" | grep -qw 'bbr'; then
            fail "当前内核未提供 BBR 拥塞控制"
            return 1
        fi
    fi

    mkdir -p /etc/sysctl.d
    conf_file="/etc/sysctl.d/99-v2node-bbr.conf"
    cat > "$conf_file" <<'EOF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
EOF

    sysctl -w net.core.default_qdisc=fq >/dev/null 2>&1 || true
    if ! sysctl -w net.ipv4.tcp_congestion_control=bbr >/dev/null 2>&1; then
        fail "写入 net.ipv4.tcp_congestion_control=bbr 失败"
        return 1
    fi

    current=$(read_tcp_congestion_control)
    if [[ "$current" != "bbr" ]]; then
        fail "BBR 写入后未生效，当前为 ${current:-unknown}"
        return 1
    fi

    log "已启用 BBR 拥塞控制"
    return 0
}

sync_once() {
    load_state || return 1
    ensure_dependencies || return 1

    local api_path="/api/v1/server/machine/v2nodeConfig"
    local response

    if ! response=$(signed_get "$api_path" "t=$(date +%s)"); then
        push_status || true
        fail "拉取探针配置失败: ${PANEL_URL}${api_path}"
        return 1
    fi

    local restart_token
    restart_token=$(printf '%s' "$response" | jq -r '.restart_v2node_token // ""')
    local enable_bbr_token
    enable_bbr_token=$(printf '%s' "$response" | jq -r '.enable_bbr_token // ""')
    local ddns_json
    ddns_json=$(printf '%s' "$response" | jq -c '.probe.ddns // {}')
    local auto_update_json
    auto_update_json=$(printf '%s' "$response" | jq -c '.probe.auto_update // {}')
    local firewall_json
    firewall_json=$(printf '%s' "$response" | jq -c '.probe.firewall_rules // []')
    local relay_json
    relay_json=$(printf '%s' "$response" | jq -c '.probe.relay // {}')
    local connectivity_test_json
    connectivity_test_json=$(printf '%s' "$response" | jq -c '.probe.connectivity_test_task // null')
    local combined_firewall_json
    combined_firewall_json=$(jq -cn \
        --argjson firewall "$firewall_json" \
        --argjson relay "$relay_json" '
        ($firewall // []) + (
            ($relay.rules // [])
            | map({
                port: ((.listen_port // 0) | tonumber),
                protocol: (.protocol // "relay"),
                protocols: (
                    (.protocols // [])
                    | map(ascii_downcase)
                    | map(select(. == "tcp" or . == "udp"))
                    | unique
                )
            })
        )')

    local nodes_json
    if ! nodes_json=$(printf '%s' "$response" | jq -c '
        (.data // []) | map({
            ApiHost: (.ApiHost // .api_host // ""),
            NodeID: ((.NodeID // .node_id // 0) | tonumber),
            ApiKey: (.ApiKey // .api_key // ""),
            Timeout: ((.Timeout // .timeout // 15) | tonumber)
        })
    '); then
        fail "解析探针配置失败"
        return 1
    fi

    local restart_required=0
    local previous_v2node_config
    local previous_v2node_plain_config
    local previous_managed_nodes_state
    local had_previous_v2node_config=0
    local had_previous_v2node_plain_config=0
    local had_previous_managed_nodes_state=0
    previous_v2node_config=$(mktemp)
    previous_v2node_plain_config=$(mktemp)
    previous_managed_nodes_state=$(mktemp)

    if [[ -n "$enable_bbr_token" && "$enable_bbr_token" != "null" ]]; then
        local bbr_error_file
        bbr_error_file=$(mktemp)
        if enable_bbr_tuning 2>"$bbr_error_file"; then
            ack_enable_bbr "$enable_bbr_token" "success" "" || true
        else
            local bbr_error
            bbr_error=$(tail -n 1 "$bbr_error_file" 2>/dev/null | cut -c 1-220)
            ack_enable_bbr "$enable_bbr_token" "failed" "${bbr_error:-启用 BBR 失败}" || true
        fi
        rm -f "$bbr_error_file"
    fi

    sync_ddns "$ddns_json" || true
    sync_relay_config "$relay_json" || true
    sync_firewall "$combined_firewall_json" || true
    if [[ -f "$CONFIG_FILE" ]]; then
        cp "$CONFIG_FILE" "$previous_v2node_config"
        had_previous_v2node_config=1
    fi
    if decrypt_config_to_file "$previous_v2node_plain_config"; then
        had_previous_v2node_plain_config=1
    fi
    if [[ -f "$MANAGED_NODES_STATE_FILE" ]]; then
        cp "$MANAGED_NODES_STATE_FILE" "$previous_managed_nodes_state"
        had_previous_managed_nodes_state=1
    fi
    if ! write_config "$nodes_json"; then
        rm -f "$previous_v2node_config" "$previous_v2node_plain_config" "$previous_managed_nodes_state"
        return 1
    fi
    if [[ -n "$connectivity_test_json" && "$connectivity_test_json" != "null" ]]; then
        run_connectivity_test_task "$connectivity_test_json" || true
    fi

    if [[ "${CONFIG_CHANGED:-0}" == "1" ]]; then
        restart_required=1
    fi

    if [[ -n "$restart_token" && "$restart_token" != "null" ]]; then
        restart_required=1
    fi

    if [[ "$restart_required" == "1" ]]; then
        if restart_v2node_service; then
            if [[ -n "$restart_token" && "$restart_token" != "null" ]]; then
                ack_restart_v2node "$restart_token" || true
            fi
        else
            fail "重启 v2node 节点服务失败，恢复上一份节点配置"
            if [[ "${CONFIG_CHANGED:-0}" == "1" ]]; then
                if [[ "$had_previous_v2node_config" == "1" ]]; then
                    mkdir -p "$(dirname "$CONFIG_FILE")"
                    mv "$previous_v2node_config" "$CONFIG_FILE"
                elif [[ "$had_previous_v2node_plain_config" == "1" ]]; then
                    encrypt_config_from_file "$previous_v2node_plain_config" || true
                else
                    rm -f "$CONFIG_FILE" "$previous_v2node_config" "$previous_v2node_plain_config"
                fi

                if [[ "$had_previous_managed_nodes_state" == "1" ]]; then
                    mkdir -p "$(dirname "$MANAGED_NODES_STATE_FILE")"
                    mv "$previous_managed_nodes_state" "$MANAGED_NODES_STATE_FILE"
                else
                    rm -f "$MANAGED_NODES_STATE_FILE" "$previous_managed_nodes_state"
                fi
                restart_v2node_service || true
            fi
            rm -f "$previous_v2node_config" "$previous_v2node_plain_config" "$previous_managed_nodes_state"
            return 1
        fi
    fi

    rm -f "$previous_v2node_config" "$previous_v2node_plain_config" "$previous_managed_nodes_state"

    maybe_auto_update "$auto_update_json" || true

    push_status || true
}

daemon_loop() {
    load_state || return 1
    ensure_dependencies || return 1
    local last_sync_at=0
    local now

    trap 'exit 0' TERM INT

    while true; do
        load_state || true
        now=$(date +%s)

        if (( now - last_sync_at >= ${SYNC_INTERVAL:-$SYNC_INTERVAL_DEFAULT} )); then
            sync_once || true
            last_sync_at="$now"
        else
            push_status || true
        fi

        sleep "${STATUS_INTERVAL:-$STATUS_INTERVAL_DEFAULT}"
    done
}

case "${1:-sync}" in
    sync)
        sync_once
        ;;
    daemon)
        daemon_loop
        ;;
    *)
        echo "用法: $0 [sync|daemon]"
        exit 1
        ;;
esac
