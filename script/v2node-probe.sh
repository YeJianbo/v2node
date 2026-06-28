#!/bin/bash

set -u

STATE_FILE="${V2NODE_PROBE_STATE_FILE:-/etc/v2node/probe.env}"
CONFIG_FILE="${V2NODE_CONFIG_FILE:-/etc/v2node/config.json}"
MANAGED_NODES_STATE_FILE="${V2NODE_PROBE_MANAGED_NODES_STATE_FILE:-/etc/v2node/probe-managed-nodes.json}"
DDNS_STATE_FILE="${V2NODE_PROBE_DDNS_STATE_FILE:-/etc/v2node/probe-ddns.state}"
GOST_CONFIG_FILE="${V2NODE_PROBE_GOST_CONFIG_FILE:-/etc/gost/config.json}"
GOST_BIN="${V2NODE_PROBE_GOST_BIN:-/usr/bin/gost}"
GOST_VERSION="${V2NODE_PROBE_GOST_VERSION:-2.11.2}"
SYNC_INTERVAL_DEFAULT=15
CONFIG_CHANGED=0
GOST_CONFIG_CHANGED=0

log() {
    echo "[v2node-probe] $*"
}

fail() {
    log "$*" >&2
    return 1
}

load_state() {
    if [[ ! -f "$STATE_FILE" ]]; then
        fail "未找到探针配置文件: $STATE_FILE"
        return 1
    fi

    # shellcheck disable=SC1090
    source "$STATE_FILE"

    PANEL_URL="${PANEL_URL:-}"
    MACHINE_TOKEN="${MACHINE_TOKEN:-}"
    MACHINE_ID="${MACHINE_ID:-}"
    SYNC_INTERVAL="${SYNC_INTERVAL:-$SYNC_INTERVAL_DEFAULT}"

    if [[ -z "$PANEL_URL" || -z "$MACHINE_TOKEN" || -z "$MACHINE_ID" ]]; then
        fail "探针配置不完整，请检查 $STATE_FILE"
        return 1
    fi

    PANEL_URL="${PANEL_URL%/}"
    load_ddns_state
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

    relay_rules_json=$(printf '%s' "$relay_json" | jq -c '
        (.rules // [])
        | map({
            listen_host: (.listen_host // "0.0.0.0"),
            listen_port: ((.listen_port // 0) | tonumber),
            target_host: (.target_host // ""),
            target_port: ((.target_port // 0) | tonumber),
            protocols: (
                (.protocols // [])
                | map(ascii_downcase)
                | map(select(. == "tcp" or . == "udp"))
                | unique
            )
        })
        | map(select(.listen_port >= 1 and .listen_port <= 65535 and .target_port >= 1 and .target_port <= 65535))
    ')

    if [[ "$(printf '%s' "$relay_rules_json" | jq 'length')" -eq 0 ]]; then
        cleanup_gost_config
        return 0
    fi

    ensure_gost_binary || return 1
    ensure_gost_service || return 1
    write_gost_config "$relay_rules_json" || return 1

    if [[ "${GOST_CONFIG_CHANGED:-0}" == "1" ]]; then
        restart_gost_service || {
            fail "重启 gost 服务失败"
            return 1
        }
    fi
}

load_ddns_state() {
    DDNS_ZONE_ID="${DDNS_ZONE_ID:-}"
    DDNS_RECORD_ID="${DDNS_RECORD_ID:-}"
    DDNS_LAST_IP="${DDNS_LAST_IP:-}"
    DDNS_LAST_HOST="${DDNS_LAST_HOST:-}"
    DDNS_LAST_SYNCED_AT="${DDNS_LAST_SYNCED_AT:-0}"
    DDNS_LAST_ERROR="${DDNS_LAST_ERROR:-}"

    if [[ -f "$DDNS_STATE_FILE" ]]; then
        # shellcheck disable=SC1090
        source "$DDNS_STATE_FILE"
    fi
}

save_ddns_state() {
    mkdir -p "$(dirname "$DDNS_STATE_FILE")"
    cat > "$DDNS_STATE_FILE" <<EOF
DDNS_ZONE_ID='${DDNS_ZONE_ID//\'/\'\"\'\"\'}'
DDNS_RECORD_ID='${DDNS_RECORD_ID//\'/\'\"\'\"\'}'
DDNS_LAST_IP='${DDNS_LAST_IP//\'/\'\"\'\"\'}'
DDNS_LAST_HOST='${DDNS_LAST_HOST//\'/\'\"\'\"\'}'
DDNS_LAST_SYNCED_AT='${DDNS_LAST_SYNCED_AT//\'/\'\"\'\"\'}'
DDNS_LAST_ERROR='${DDNS_LAST_ERROR//\'/\'\"\'\"\'}'
EOF
}

update_ddns_state() {
    DDNS_LAST_HOST="${1:-$DDNS_LAST_HOST}"
    DDNS_LAST_IP="${2:-$DDNS_LAST_IP}"
    DDNS_LAST_SYNCED_AT="${3:-$DDNS_LAST_SYNCED_AT}"
    DDNS_LAST_ERROR="${4:-$DDNS_LAST_ERROR}"
    save_ddns_state
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
        printf '{"docker_total":0,"docker_running":0}'
        return
    fi

    local total running
    total=$(docker ps -a -q 2>/dev/null | wc -l | awk '{print int($1)}')
    running=$(docker ps -q 2>/dev/null | wc -l | awk '{print int($1)}')
    printf '{"docker_total":%d,"docker_running":%d}' "${total:-0}" "${running:-0}"
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

read_primary_ip() {
    local ip
    ip=$(hostname -I 2>/dev/null | tr ' ' '\n' | awk '
        /^127\./ { next }
        /^169\.254\./ { next }
        /^172\.(1[6-9]|2[0-9]|3[0-1])\./ { next }
        /^198\.18\./ { next }
        /^198\.19\./ { next }
        /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }
    ')
    if [[ -n "$ip" ]]; then
        printf '%s' "$ip"
        return
    fi

    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1; i<=NF; i++) if ($i == "src") {print $(i+1); exit}}')
    if [[ -n "$ip" ]]; then
        printf '%s' "$ip"
        return
    fi

    hostname -I 2>/dev/null | awk '{print $1}'
}

push_status() {
    load_state || return 1
    ensure_dependencies || return 1

    local cpu mem disk uptime version net_rx net_tx primary_ip body gost_version gost_rule_count
    local mem_total mem_used disk_total disk_used cpu_cores cpu_model os_name kernel arch
    local load_json docker_json v2node_status gost_status listen_ports
    cpu=$(read_cpu_percent)
    mem=$(read_mem_percent)
    disk=$(read_disk_percent)
    mem_total=$(read_mem_total_bytes)
    mem_used=$(read_mem_used_bytes)
    disk_total=$(read_disk_total_bytes)
    disk_used=$(read_disk_used_bytes)
    cpu_cores=$(read_cpu_cores)
    cpu_model=$(read_cpu_model)
    os_name=$(read_os_name)
    kernel=$(uname -r 2>/dev/null | cut -c 1-80)
    arch=$(uname -m 2>/dev/null | cut -c 1-40)
    load_json=$(read_load_json)
    if [[ -z "$load_json" ]]; then
        load_json='{"load1":0,"load5":0,"load15":0}'
    fi
    docker_json=$(read_docker_status_json)
    if [[ -z "$docker_json" ]]; then
        docker_json='{"docker_total":0,"docker_running":0}'
    fi
    v2node_status=$(read_service_status v2node)
    gost_status=$(read_service_status gost)
    listen_ports=$(read_listen_ports)
    uptime=$(cut -d' ' -f1 /proc/uptime 2>/dev/null | cut -d'.' -f1)
    version="v2node-probe $(uname -s 2>/dev/null) $(uname -m 2>/dev/null)"
    primary_ip=$(read_primary_ip)
    read -r net_rx net_tx <<< "$(read_net_bytes)"
    gost_version=$(get_gost_version)
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
        --argjson disk_total "${disk_total:-0}" \
        --argjson disk_used "${disk_used:-0}" \
        --argjson cpu_cores "${cpu_cores:-0}" \
        --arg cpu_model "${cpu_model:-}" \
        --arg os "${os_name:-}" \
        --arg kernel "${kernel:-}" \
        --arg arch "${arch:-}" \
        --argjson load "$load_json" \
        --argjson docker "$docker_json" \
        --argjson net_rx "${net_rx:-0}" \
        --argjson net_tx "${net_tx:-0}" \
        --argjson uptime "${uptime:-0}" \
        --arg ip "$primary_ip" \
        --arg version "$version" \
        --arg ddns_host "${DDNS_LAST_HOST:-}" \
        --arg ddns_synced_ip "${DDNS_LAST_IP:-}" \
        --argjson ddns_synced_at "${DDNS_LAST_SYNCED_AT:-0}" \
        --arg ddns_error "${DDNS_LAST_ERROR:-}" \
        --arg gost_version "${gost_version:-}" \
        --argjson gost_rule_count "${gost_rule_count:-0}" \
        --arg v2node_status "${v2node_status:-unknown}" \
        --arg gost_status "${gost_status:-unknown}" \
        --arg listen_ports "${listen_ports:-}" \
        '{
            cpu:$cpu,
            mem:$mem,
            disk:$disk,
            mem_total:$mem_total,
            mem_used:$mem_used,
            disk_total:$disk_total,
            disk_used:$disk_used,
            cpu_cores:$cpu_cores,
            cpu_model:$cpu_model,
            os:$os,
            kernel:$kernel,
            arch:$arch,
            load1:($load.load1 // 0),
            load5:($load.load5 // 0),
            load15:($load.load15 // 0),
            docker_total:($docker.docker_total // 0),
            docker_running:($docker.docker_running // 0),
            net_rx:$net_rx,
            net_tx:$net_tx,
            uptime:$uptime,
            ip:$ip,
            version:$version,
            ddns_host:$ddns_host,
            ddns_synced_ip:$ddns_synced_ip,
            ddns_synced_at:$ddns_synced_at,
            ddns_error:$ddns_error,
            gost_version:$gost_version,
            gost_rule_count:$gost_rule_count,
            v2node_status:$v2node_status,
            gost_status:$gost_status,
            listen_ports:$listen_ports
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

    if [[ -f "$CONFIG_FILE" ]] && jq -e 'type == "object"' "$CONFIG_FILE" >/dev/null 2>&1; then
        cp "$CONFIG_FILE" "$existing_config_file"
    else
        jq -n '{
            Log: {
                Level: "warning",
                Output: "",
                Access: "none"
            },
            Nodes: []
        }' > "$existing_config_file"
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
        def node_key:
            [
                (.ApiHost // .api_host // "" | tostring),
                ((.NodeID // .node_id // 0) | tostring)
            ] | join("#");

        ($existing[0] // {}) as $old
        | (($previous_managed[0] // []) | map(tostring)) as $previousKeys
        | ($desired_nodes // []) as $desired
        | (($old.Nodes // []) | map(select((node_key as $key | $previousKeys | index($key)) | not))) as $manualNodes
        | ($desired | map(select(node_key as $key | ($manualNodes | map(node_key) | index($key)) | not))) as $newManagedNodes
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
        def node_key:
            [
                (.ApiHost // .api_host // "" | tostring),
                ((.NodeID // .node_id // 0) | tostring)
            ] | join("#");
        map(node_key) | unique
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

    mkdir -p "$(dirname "$CONFIG_FILE")"
    mkdir -p "$(dirname "$MANAGED_NODES_STATE_FILE")"

    if [[ -f "$CONFIG_FILE" ]] && cmp -s "$tmp_file" "$CONFIG_FILE"; then
        rm -f "$tmp_file"
    else
        mv "$tmp_file" "$CONFIG_FILE"
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

sync_once() {
    load_state || return 1
    ensure_dependencies || return 1

    push_status || true

    local api_path="/api/v1/server/machine/v2nodeConfig"
    local response

    if ! response=$(signed_get "$api_path" "t=$(date +%s)"); then
        fail "拉取探针配置失败: ${PANEL_URL}${api_path}"
        return 1
    fi

    local restart_token
    restart_token=$(printf '%s' "$response" | jq -r '.restart_v2node_token // ""')
    local ddns_json
    ddns_json=$(printf '%s' "$response" | jq -c '.probe.ddns // {}')
    local firewall_json
    firewall_json=$(printf '%s' "$response" | jq -c '.probe.firewall_rules // []')
    local relay_json
    relay_json=$(printf '%s' "$response" | jq -c '.probe.relay // {}')
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

    sync_ddns "$ddns_json" || true
    sync_relay_config "$relay_json" || true
    sync_firewall "$combined_firewall_json" || true
    write_config "$nodes_json"

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
            fail "重启 v2node 节点服务失败"
            return 1
        fi
    fi

    push_status || true
}

daemon_loop() {
    load_state || return 1
    ensure_dependencies || return 1

    trap 'exit 0' TERM INT

    while true; do
        sync_once || true
        sleep "${SYNC_INTERVAL:-$SYNC_INTERVAL_DEFAULT}"
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
