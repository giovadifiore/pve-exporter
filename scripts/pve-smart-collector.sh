#!/bin/bash
# pve-smart-collector.sh
# Collects SMART data from remote smartctl-exporter HTTP endpoints and writes to JSON file
# Run as root via cron every 5 minutes (SMART data doesn't change frequently)
#
# Usage: /usr/local/bin/pve-smart-collector.sh
# Cron:  */5 * * * * root /usr/local/bin/pve-smart-collector.sh

OUTPUT_DIR="/var/lib/pve-exporter"
OUTPUT_FILE="${OUTPUT_DIR}/smart.json"
TEMP_FILE="${OUTPUT_DIR}/smart.json.tmp"
LOCK_FILE="${OUTPUT_DIR}/.smart-collector.lock"
CONFIG_FILE="${CONFIG_FILE:-/etc/pve-exporter/smart-collector-urls.conf}"
HTTP_TIMEOUT="${HTTP_TIMEOUT:-15}"
HOSTNAME=$(hostname -f 2>/dev/null || hostname)

# Ensure output directory exists before anything else
mkdir -p "$OUTPUT_DIR" || exit 1

# Use lock file to prevent parallel execution
exec 200>"$LOCK_FILE"
if ! flock -n 200; then
    exit 0
fi

if [ ! -f "$CONFIG_FILE" ]; then
    echo "Config file not found: $CONFIG_FILE" >&2
    exit 1
fi

echo "{\"hostname\":\"$HOSTNAME\",\"timestamp\":$(date +%s),\"disks\":[" > "$TEMP_FILE"

first=true
while IFS= read -r endpoint || [ -n "$endpoint" ]; do
    endpoint=$(echo "$endpoint" | sed 's/^\s*//;s/\s*$//')
    [ -z "$endpoint" ] && continue
    [ "${endpoint#\#}" != "$endpoint" ] && continue

    smart_json=$(curl -fsSL --max-time "$HTTP_TIMEOUT" "$endpoint" 2>/dev/null)
    [ -z "$smart_json" ] && continue

    parsed=$(echo "$smart_json" | python3 -c "
import sys, json, urllib.parse
try:
    data = json.load(sys.stdin)
    if 'device' not in data: sys.exit(1)
    device = data.get('device', {}).get('name', 'unknown')
    if isinstance(device, str) and device.startswith('/dev/'):
        device = device.split('/')[-1]
    if not device or str(device).strip() == '':
        parsed_url = urllib.parse.urlparse('$endpoint')
        q = urllib.parse.parse_qs(parsed_url.query)
        disk = q.get('disk', ['unknown'])[0]
        device = disk.split('/')[-1] if '/' in disk else disk
    r = {'device': str(device), 'model': data.get('model_name', 'Unknown'), 'serial': data.get('serial_number', 'Unknown'), 'type': 'unknown', 'healthy': 1}
    di = data.get('device', {})
    if di.get('protocol') == 'NVMe': r['type'] = 'nvme'
    elif di.get('protocol') == 'ATA': r['type'] = 'sata'
    ss = data.get('smart_status', {})
    if 'passed' in ss: r['healthy'] = 1 if ss['passed'] else 0
    nv = data.get('nvme_smart_health_information_log', {})
    if nv:
        r['temperature'] = nv.get('temperature')
        r['power_on_hours'] = nv.get('power_on_hours')
        r['percentage_used'] = nv.get('percentage_used')
        if 'data_units_written' in nv: r['data_written_bytes'] = nv['data_units_written'] * 512000
        if 'available_spare' in nv: r['available_spare_percent'] = nv['available_spare']
    pt = data.get('power_on_time', {})
    if 'hours' in pt: r['power_on_hours'] = pt['hours']
    ti = data.get('temperature', {})
    if 'current' in ti and 'temperature' not in r: r['temperature'] = ti['current']
    for a in data.get('ata_smart_attributes', {}).get('table', []):
        if a.get('id') == 194 and 'temperature' not in r: r['temperature'] = a.get('raw', {}).get('value')
    r = {k: v for k, v in r.items() if v is not None}
    print(json.dumps(r))
except: sys.exit(1)
" 2>/dev/null)

    if [ -n "$parsed" ]; then
        [ "$first" = true ] && first=false || echo "," >> "$TEMP_FILE"
        echo "$parsed" >> "$TEMP_FILE"
    fi
done < "$CONFIG_FILE"

echo "]}" >> "$TEMP_FILE"

# Only move if temp file exists and is valid
if [ -f "$TEMP_FILE" ]; then
    mv "$TEMP_FILE" "$OUTPUT_FILE"
    chmod 644 "$OUTPUT_FILE"
fi
