#!/bin/bash

set -u

STATE_FILE="${V2NODE_PROBE_STATE_FILE:-/etc/v2node/probe.env}"
CONFIG_FILE="${V2NODE_CONFIG_FILE:-/etc/v2node/config.json}"
DDNS_STATE_FILE="${V2NODE_PROBE_DDNS_STATE_FILE:-/etc/v2node/probe-ddns.state}"
SYNC_INTERVAL_DEFAULT=15
CONFIG_CHANGED=0

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

read_disk_percent() {
    df -P / 2>/dev/null | awk 'NR==2 { gsub(/%/, "", $5); print int($5) }'
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

    local cpu mem disk uptime version net_rx net_tx primary_ip body
    cpu=$(read_cpu_percent)
    mem=$(read_mem_percent)
    disk=$(read_disk_percent)
    uptime=$(cut -d' ' -f1 /proc/uptime 2>/dev/null | cut -d'.' -f1)
    version="v2node-probe $(uname -s 2>/dev/null) $(uname -m 2>/dev/null)"
    primary_ip=$(read_primary_ip)
    read -r net_rx net_tx <<< "$(read_net_bytes)"

    body=$(jq -nc \
        --argjson cpu "${cpu:-0}" \
        --argjson mem "${mem:-0}" \
        --argjson disk "${disk:-0}" \
        --argjson net_rx "${net_rx:-0}" \
        --argjson net_tx "${net_tx:-0}" \
        --argjson uptime "${uptime:-0}" \
        --arg ip "$primary_ip" \
        --arg version "$version" \
        --arg ddns_host "${DDNS_LAST_HOST:-}" \
        --arg ddns_synced_ip "${DDNS_LAST_IP:-}" \
        --argjson ddns_synced_at "${DDNS_LAST_SYNCED_AT:-0}" \
        --arg ddns_error "${DDNS_LAST_ERROR:-}" \
        '{cpu:$cpu, mem:$mem, disk:$disk, net_rx:$net_rx, net_tx:$net_tx, uptime:$uptime, ip:$ip, version:$version, ddns_host:$ddns_host, ddns_synced_ip:$ddns_synced_ip, ddns_synced_at:$ddns_synced_at, ddns_error:$ddns_error}')

    signed_post_json "/api/v1/server/machine/push" "$body" >/dev/null
}

write_config() {
    local nodes_json="$1"
    local tmp_file
    CONFIG_CHANGED=0
    tmp_file=$(mktemp)

    if ! jq -n \
        --argjson nodes "$nodes_json" \
        '{
            Log: {
                Level: "warning",
                Output: "",
                Access: "none"
            },
            Nodes: $nodes
        }' > "$tmp_file"; then
        rm -f "$tmp_file"
        fail "生成配置文件失败"
        return 1
    fi

    mkdir -p "$(dirname "$CONFIG_FILE")"

    if [[ -f "$CONFIG_FILE" ]] && cmp -s "$tmp_file" "$CONFIG_FILE"; then
        rm -f "$tmp_file"
        return 0
    fi

    mv "$tmp_file" "$CONFIG_FILE"
    CONFIG_CHANGED=1
    log "已更新 $CONFIG_FILE"
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
