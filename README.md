# Rmm-monitoring-stack

Out-of-band GPU server monitoring for mixed Dell, Lenovo, Supermicro, HPE, and Tyrone
fleets. Nothing runs on the server guest OS. All data comes from the BMC (Baseboard
Management Controller) over the management network using the Redfish API.

---

## How It Works (Architecture)
```
┌───────────────────────────────────────────────────────────────────┐
│                       Your Fleet (245 servers)                    │
│                                                                    │
│  Dell iDRAC (183)   Lenovo XCC (15)   SuperMicro BMC (41)         │
│  H100/H200            H100               H100                     │
│      │                   │                  │                    │
│              NVIDIA HGX/AMI baseboard BMC (6, labeled             │
│              "HPE" in inventory — real chassis iLO not used)      │
│              H100, full per-GPU sensors (GPU0_PROC..GPU7_PROC)    │
│                          │                                        │
└──────────────────────────┼────────────────────────────────────────┘
                           │
                    Management Network (out-of-band VLAN)
                           │  HTTPS/443
                           ▼
          ┌────────────────────────────────────┐
          │         Redfish Exporter           │
          │         (Go, port 9610)            │
          │                                    │
          │  1. Discovers Redfish resources    │
          │  2. Normalizes vendor differences  │
          │  3. Caches results for 6 minutes   │
          │  4. Serves Prometheus metrics      │
          └─────────────────┬──────────────────┘
                            │ /redfish?target=<bmc-ip>
                            ▼
          ┌────────────────────────────────────┐
          │           Prometheus               │
          │           (port 9090)              │
          │                                    │
          │  Scrapes exporter every 5 minutes  │
          │  Evaluates alert rules             │
          │  Stores 90 days of metrics         │
          └──────────┬──────────────┬──────────┘
                     │              │
          ┌──────────▼───┐  ┌───────▼──────────┐
          │   Grafana    │  │   Alertmanager   │
          │  (port 3000) │  │   (port 9093)    │
          │              │  │                  │
          │  Dashboards  │  │  Slack / PagerDuty│
          └──────────────┘  └──────────────────┘
```

### Key design decisions

| Decision | Reason |
|---|---|
| Pull from BMC, not push from server | Works even when the OS is down, hung, or being reimaged |
| Standard Redfish API only | Works across Dell, Supermicro, HPE, Lenovo, Tyrone without vendor-specific code |
| 6-minute cache in exporter | BMC gets hit once per cycle; Prometheus polls exporter but never floods BMC |
| 5-minute Prometheus scrape interval | Matches the cache window; 245 servers is still light network load (~0.8 BMC hits/sec) |
| Labels from inventory CSV | Vendor, site, GPU type, and hostname (optional) stay attached to every metric in Grafana |

---

## Data Flow — Step by Step

```
1.  You populate inventory.csv with BMC IPs, vendor, site, GPU type.

2.  generate_targets.py converts inventory.csv → prometheus/redfish_targets.yml
    (Prometheus file service discovery format).

3.  Prometheus reads redfish_targets.yml every 5 minutes (auto-refresh,
    no restart needed when you add/remove servers).

4.  Every 5 minutes, Prometheus calls:
      http://redfish_exporter:9610/redfish?target=<bmc-ip>
    once per BMC in the targets file.

5.  The exporter checks its cache:
      - Cache HIT  → returns stored metrics immediately (no BMC contact)
      - Cache MISS → contacts the BMC over HTTPS, crawls the Redfish
                     resource graph, normalizes the data, caches it for
                     6 minutes, then returns the metrics.

6.  Prometheus stores the metrics with the labels from your inventory:
      vendor, site, gpu, instance (= BMC IP)

7.  Grafana queries Prometheus on demand when you load a dashboard.

8.  Prometheus evaluates alert rules every 60 seconds.
    If a rule fires (e.g. GPU > 80°C for 5 minutes), it sends the alert
    to Alertmanager, which routes it to Slack.
```

---

## Project Structure

```
monitoring-stack/
│
├── cmd/exporter/main.go          # Exporter entry point (HTTP server, flags)
│
├── internal/
│   ├── redfish/client.go         # Generic Redfish HTTPS client (no vendor code)
│   ├── collector/collector.go    # Crawls Redfish graph, normalizes, caches
│   ├── model/model.go            # Shared data structures (Metric, Snapshot)
│   ├── cache/cache.go            # Thread-safe TTL cache
│   ├── metrics/http.go           # Formats metrics as Prometheus text
│   └── config/config.go          # Loads exporter.yml + env variables
│
├── configs/
│   └── exporter.yml              # BMC credentials, timeouts, cache settings
│
├── prometheus/
│   ├── prometheus.yml            # Scrape jobs, alert rule files
│   ├── alert_rules.yml           # Alert definitions
│   └── redfish_targets.yml       # Auto-generated from inventory.csv
│
├── alertmanager/
│   └── alertmanager.yml          # Slack/PagerDuty routing
│
├── grafana/provisioning/
│   ├── datasources/              # Prometheus datasource (auto-configured)
│   └── dashboards/json/          # Dashboard JSON files (auto-loaded)
│       ├── server_detail.json       ← per-server view (select by BMC IP)
│       ├── oem_fleet.json           ← OEM + GPU type drill-down
│       ├── gpu_temperature.json     ← fleet-wide GPU temps
│       ├── top_10_hottest_gpus.json ← top 10 with site/vendor filter
│       └── gpu_count_mismatch.json  ← servers where visible GPUs != inventory gpu_count
│
├── inventory.csv                 # Your BMC IP list (you maintain this)
├── inventory.example.csv         # Template showing the format
├── generate_targets.py           # inventory.csv → redfish_targets.yml
├── docker-compose.yml            # 4-service stack
├── Dockerfile                    # Multi-stage Go build → distroless runtime
└── Makefile                      # build, test, run shortcuts
```

---

## Dashboards

Five dashboards are auto-provisioned. They form a drill-down chain:

```
OEM Fleet – GPU Drill-Down
  │
  │  Pick an OEM (Dell / Supermicro / HPE / Lenovo)
  │  Pick a GPU type (H200 / H100)
  │  See: Top 10 Hottest GPUs, GPU Health table, Server Health table
  │
  └─► Server Detail  (click any server in the tables or bar gauge)
        │
        │  Pre-selected BMC IP
        │  See: System Health, GPU Temps, GPU Health, CPU/Memory/PSU/Disk,
        │       Power, Fan Speed, All Sensors, GPU Inventory
        │
        └─► (drill back up using browser back or the OEM filter)

GPU Temperature          — fleet-wide time series for all GPU sensors
Top 10 Hottest GPUs      — fleet-wide bar gauge + trend, filter by site/vendor
GPU Count Mismatch       — servers where visible GPUs != inventory gpu_count
```

**"Server Count" and "Total GPUs" on OEM Fleet** are inventory counts, not
live telemetry counts — see "Fleet-inventory counts vs. live telemetry
counts" in Metrics Reference for why, and expect `Total GPUs` to read
slightly below your true fleet total whenever any host is temporarily
unreachable (it excludes down hosts' configured GPU counts until they
recover).

### Dashboard variables

| Dashboard | Variables | What they filter |
|---|---|---|
| OEM Fleet | OEM (multi-select), GPU Type (multi-select) | All panels |
| Server Detail | OEM, GPU Type (narrow IP list), BMC IP | All panels |
| Top 10 Hottest | Site (multi), Vendor (multi) | All panels |
| GPU Temperature | _(none — shows all)_ | — |
| GPU Count Mismatch | Site (multi), Vendor (multi) | All panels |

**GPU Count Mismatch uses `redfish_gpu_temperature_celsius` (not `redfish_gpu_health`) as the "visible GPU"
signal, with an explicit SuperMicro exception** (compared against 3, not `gpu_count` — see Vendor Notes) — not
`redfish_gpu_health`, because Lenovo's firmware never exposes a GPU Processor resource at all, so `redfish_gpu_health`
is completely absent there even though `redfish_gpu_temperature_celsius` (via a PCIe-slot-sensor fallback) is a
genuine 1:1 match with `gpu_count`. A server that has **never once** successfully scraped has no
`redfish_gpu_count_configured` series at all (the exporter only emits it after actually reaching the BMC), so it
won't appear on this dashboard — those are already covered by `RedfishTargetDown` and the `up{job="redfish"}`
panels elsewhere.

---

## Metrics Reference

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `redfish_system_health` | gauge | instance, vendor, name | 1=OK, 0=not OK |
| `redfish_gpu_temperature_celsius` | gauge | instance, vendor, gpu, name | GPU sensor temp |
| `redfish_gpu_health` | gauge | instance, vendor, gpu, name, model | 1=OK, 0=not OK |
| `redfish_gpu_info` | gauge | instance, vendor, gpu, name, model, processor_type | Inventory (value always 1) |
| `redfish_cpu_health` | gauge | instance, vendor, name | 1=OK, 0=not OK |
| `redfish_memory_health` | gauge | instance, vendor, name | 1=OK, 0=not OK |
| `redfish_disk_health` | gauge | instance, vendor, name | 1=OK, 0=not OK |
| `redfish_psu_health` | gauge | instance, vendor, name | 1=OK, 0=not OK |
| `redfish_temperature_celsius` | gauge | instance, vendor, name | Any Redfish sensor |
| `redfish_fan_speed_rpm` | gauge | instance, vendor, name | Fan tachometer |
| `redfish_power_watts` | gauge | instance, vendor, name | Node power draw |
| `redfish_gpu_count_configured` | gauge | instance | Physical GPU count from inventory's `gpu_count` column — NOT derived from this scrape's telemetry, so it's present even when `redfish_gpu_temperature_celsius`/`redfish_gpu_health` are missing for that host (see Vendor Notes). Only emitted while the target is reachable (crawl reaches the Redfish service root) — a fully down host drops out of `sum(redfish_gpu_count_configured)` until it recovers |

`instance` = BMC IP address. `vendor`, `gpu`, `site`, `gpu_count`, and the
optional `hostname` come from `inventory.csv` and are attached as target
labels to every metric from that host (Prometheus merges them in
automatically) — only `redfish_gpu_count_configured` additionally uses
`gpu_count` as its actual *value*, via `__param_gpu_count` (see
`prometheus/prometheus.yml`). `hostname` is per-host and typically only set
for servers you've named in inventory — blank for the rest, which is fine
(see Step 3).

**Fleet-inventory counts vs. live telemetry counts — which metric to use:**
- "How many servers do I have?" → `count(up{job="redfish"})`. Counts every
  configured target regardless of reachability. Do NOT use
  `count(redfish_system_health)` — some hosts don't emit it at all, and
  NVIDIA HGX-baseboard hosts emit *two* (the host system and the baseboard
  itself), so a raw count both under- and over-counts.
- "How many GPUs do I have?" → `sum(redfish_gpu_count_configured)`. Do NOT
  use `count(redfish_gpu_temperature_celsius)` — see the `gpu_count`
  explanation in Step 3 above for why sensor-counting is unreliable across
  vendors.

---

## Alert Rules

| Alert | Condition | Severity | Fires After |
|---|---|---|---|
| `GPUTemperatureHigh` | `redfish_gpu_temperature_celsius > 80` | warning | 5 min |
| `GPUHotWhileIdle` | GPU sensor > 45°C while the host's power draw trails well behind its same-vendor/GPU-count peers (bottom 15th percentile) — see the alert's own comment in `alert_rules.yml` for why this is peer-relative rather than a fleet-wide watt cutoff | warning | 15 min |
| `HighTemperature` | `redfish_temperature_celsius > 85` | warning | 5 min |
| `GPUFailure` | `redfish_gpu_health == 0` | critical | 5 min |
| `ServerHealthCritical` | `redfish_system_health == 0` | critical | 5 min |
| `PowerSupplyFailure` | `redfish_psu_health == 0` | critical | 2 min |
| `DiskFailure` | `redfish_disk_health == 0` | critical | 5 min |
| `RedfishTargetDown` | BMC unreachable | warning | 10 min |

---

## Setup

### Prerequisites

- Linux VM with Docker and Docker Compose (Ubuntu 22.04 recommended)
- Network access from the VM to your BMC/management VLAN (TCP 443)
- A read-only monitoring account created on each BMC

```bash
# Install Docker if not already installed
curl -fsSL https://get.docker.com | sh
sudo apt install docker-compose-plugin -y
sudo usermod -aG docker $USER   # log out and back in after this
```

---

### Step 1 — Copy the stack to your monitoring VM

```bash
# From your local machine
scp -r monitoring-stack/ user@<monitoring-vm-ip>:/opt/

# Log into the VM
ssh user@<monitoring-vm-ip>
cd /opt/monitoring-stack
```

---

### Step 2 — Set credentials

```bash
cp .env.example .env
chmod 600 .env
nano .env
```

Set two values:

```
REDFISH_PASSWORD=<read-only-bmc-account-password>
GRAFANA_ADMIN_PASSWORD=<your-grafana-admin-password>
```

The BMC username is set in `configs/exporter.yml` (`username: monitoring` by default).
If your BMC monitoring account has a different username, edit that file.

**Per-BMC overrides** (credentials, crawl concurrency, and/or crawl budget):

```yaml
# configs/exporter.yml
hosts:
  default:
    username: monitoring
    password_env: REDFISH_PASSWORD
  "10.10.5.10":
    username: svc_monitor
    password_env: REDFISH_SMC_PASSWORD   # add this to .env too
  # A concurrency-only override inherits username/password from
  # hosts.default — useful for a BMC that can't tolerate the fleet-default
  # per-host concurrency (some embedded controllers apply an anti-hammering
  # lockout under concurrent Basic-Auth requests).
  "10.10.6.10":
    concurrency: 1
  # max_resources/max_depth overrides are for BMCs whose resource graph is
  # far larger than the fleet default budget covers — e.g. NVIDIA HGX
  # baseboard controllers expose 40+ Chassis members alone (per-GPU ERoT
  # security co-processor, NVSwitch, PCIeRetimer, PCIeSwitch, plus the real
  # per-GPU chassis) and per-core CPU/per-DIMM memory detail several levels
  # deep, several times the resource count of a typical Dell/Lenovo/
  # SuperMicro host. Raising max_resources lets the crawl reach the real GPU
  # chassis' Sensors/ThermalSubsystem resources instead of exhausting its
  # budget on ERoT/NVSwitch/PCIeRetimer chassis first (the crawl visits
  # links alphabetically, and "HGX_ERoT_GPU_SXM_*" sorts before
  # "HGX_GPU_SXM_*"); capping max_depth lower than the fleet default trades
  # away per-core CPU/per-drive granularity (which sits deeper) in exchange
  # for actually finishing the crawl within scrape_timeout — GPU sensor
  # readings sit at depth 4, per-core CPU detail at depth 6. See
  # Troubleshooting → "GPU temperature crawl silently truncated on NVIDIA
  # HGX baseboard controllers" for how these numbers were derived.
  "10.10.7.10":
    max_resources: 600
    max_depth: 4
```

---

### Step 3 — Build the inventory

Copy the example and fill it in with your BMC IPs:

```bash
cp inventory.example.csv inventory.csv
nano inventory.csv
```

Format:

```
ip,vendor,site,gpu,gpu_count,hostname
10.10.1.10,dell,dc1,h200,8,rack1-node1
10.10.1.11,dell,dc1,h200,8,rack1-node2
10.10.2.10,supermicro,dc1,h200,8,
10.10.3.10,supermicro,dc1,h100,8,
10.10.4.10,hpe,dc1,h100,4,
10.10.5.10,tyrone,dc2,b200,8,
```

**Field values:**

| Field | Allowed values | Notes |
|---|---|---|
| `vendor` | `dell`, `lenovo`, `supermicro`, `hpe`, `tyrone` | Used as Grafana filter label |
| `site` | any string, e.g. `dc1`, `dc2` | Your data centre name |
| `gpu` | `b200`, `h200`, `h100` (priority) | GPU card type in that server |
| `gpu_count` | integer, e.g. `4`, `8` | Physical GPU count in that server — see below for why this can't be inferred from live telemetry |
| `hostname` | any string, or blank | Optional. Per-host (unlike the other fields, which are typically shared across many rows) — leave blank for hosts you haven't named. Becomes a Prometheus target label like the others, so it's usable directly as a Grafana filter/variable without any exporter changes. See `generate_targets.py`'s docstring for exactly how it's grouped into `redfish_targets.yml` |

**Why `gpu_count` is a separate inventory field, not derived from sensor data:**
GPU temperature sensor *count* is not a reliable proxy for physical GPU
*count* across vendors — BMC firmware exposes GPU thermal telemetry at
wildly different granularity. On this fleet, Dell and the NVIDIA
HGX-baseboard controllers (see Vendor Notes) report one temperature sensor
per physical GPU (a true 1:1), but SuperMicro's Redfish implementation only
exposes 3 zone/aggregate sensors per 8-GPU server (`GPU Temp`, `GPU Inlet
Temp`, `GPU HSC Temp` — not one per GPU). Counting sensor series on a mixed
fleet like that undercounts GPUs on whichever vendors report coarser
telemetry. `gpu_count` is the authoritative, inventory-sourced fact instead;
it's threaded through `generate_targets.py` → Prometheus (`gpu_count` target
label → `__param_gpu_count`, see `prometheus/prometheus.yml`) → the exporter,
which emits it as `redfish_gpu_count_configured` (see Metrics Reference).

Lines starting with `#` are skipped (use for comments or temporarily disabled hosts).

Generate the Prometheus targets file:

```bash
python3 generate_targets.py inventory.csv > prometheus/redfish_targets.yml
```

Verify it looks correct:

```bash
cat prometheus/redfish_targets.yml
```

Expected output format:

```yaml
- targets:
    - '10.10.1.10'
  labels:
    vendor: dell
    site: dc1
    gpu: h200
    gpu_count: "8"
    hostname: rack1-node1

- targets:
    - '10.10.2.10'
    - '10.10.3.10'
  labels:
    vendor: supermicro
    site: dc1
    gpu: h200
    gpu_count: "8"
```

Rows sharing the same (vendor, site, gpu, gpu_count, hostname) group into one target block — so
`10.10.1.10` and `10.10.1.11` above, despite matching on everything else, land in *separate* blocks
because each has its own distinct `hostname`, while `10.10.2.10`/`10.10.3.10` (both blank hostname)
group together as before.

---

### Step 4 — Configure alert notifications

Edit `alertmanager/alertmanager.yml` and replace the placeholder webhook URL with
your real Slack incoming webhook or PagerDuty routing key:

```yaml
receivers:
  - name: 'slack'
    slack_configs:
      - api_url: 'https://hooks.slack.com/services/YOUR/REAL/WEBHOOK'
        channel: '#gpu-alerts'
```

---

### Step 5 — Start the stack

```bash
docker compose up -d
```

First run downloads images and builds the Go exporter (~2 minutes).
Subsequent starts use Docker's cache and take a few seconds.

Check all four services are running:

```bash
docker compose ps
```

Expected:

```
NAME                    STATUS    PORTS
redfish_exporter        running   0.0.0.0:9610->9610/tcp
prometheus              running   0.0.0.0:9090->9090/tcp
alertmanager            running   0.0.0.0:9093->9093/tcp
grafana                 running   0.0.0.0:3000->3000/tcp
```

---

### Step 6 — Verify

**Test the exporter directly** (replace with a real BMC IP from your inventory):

```bash
curl "http://localhost:9610/redfish?target=10.10.1.10"
```

You should see Prometheus-format metrics like:

```
redfish_system_health{instance="10.10.1.10",name="...",vendor="..."} 1
redfish_gpu_temperature_celsius{instance="10.10.1.10",name="GPU 1 Temp",...} 67.5
```

**Check Prometheus is scraping targets:**

Open `http://<vm-ip>:9090/targets` in your browser.
All entries under the `redfish` job should show state `UP`.
(Allow one full 5-minute scrape cycle on first start.)

**Open Grafana:**

`http://<vm-ip>:3000` → log in with `admin` / your password.

All five dashboards are in the **Redfish Infrastructure** folder in the left nav.
Start with **OEM Fleet – GPU Drill-Down** to verify your fleet is showing up.

---

### Step 7 — Firewall checklist

```
Monitoring VM → BMC IPs    TCP 443 outbound   (Redfish API)
Your browser → VM          TCP 3000           (Grafana)
Your browser → VM          TCP 9090           (Prometheus UI, optional)
Your browser → VM          TCP 9093           (Alertmanager UI, optional)
```

No ports need to be open inbound on the actual GPU servers.

---

## Day-2 Operations

### Adding or removing servers

Edit `inventory.csv`, then regenerate:

```bash
python3 generate_targets.py inventory.csv > prometheus/redfish_targets.yml
```

Prometheus picks up the new file within 5 minutes. No restart needed.

### Restarting after a VM reboot

```bash
cd /opt/monitoring-stack
docker compose up -d
```

To make this automatic on boot:

```bash
sudo systemctl enable docker
# docker compose uses restart: unless-stopped already set in docker-compose.yml
```

### Viewing logs

```bash
docker compose logs -f redfish_exporter   # exporter scrape activity
docker compose logs -f prometheus         # scrape errors, alert evaluations
docker compose logs -f alertmanager       # alert routing
docker compose logs -f grafana            # dashboard provisioning errors
```

### Rebuilding after a code change

```bash
docker compose build redfish_exporter
docker compose up -d redfish_exporter
```

### Checking alert status

Open `http://<vm-ip>:9093` (Alertmanager UI) — shows active alerts and silences.
Open `http://<vm-ip>:9090/alerts` (Prometheus alerts page) — shows rule evaluation state.

---

## Troubleshooting

### BMC returns no GPU temperature metrics

1. Check the exporter directly: `curl "http://localhost:9610/redfish?target=<bmc-ip>"`
2. Look for `redfish_gpu_temperature_celsius` in the output. If missing:
   - Dell: check iDRAC firmware is current; GPU thermals appear under `/Chassis/Thermal`
   - Supermicro: old firmware has incomplete Redfish — update BMC firmware first
   - HPE: GPU thermals may require a valid iLO Advanced licence on older platforms
   - Tyrone: check if Tyrone exposes GPU sensors under standard `/Chassis/Thermal` path
3. Increase `max_resources` and `max_depth` in `configs/exporter.yml` if the crawl
   is hitting the limit before reaching thermal endpoints
4. Check `docker compose logs redfish_exporter | grep <bmc-ip>` for
   `"Redfish collection deadline"` errors — this means the crawl was still
   running when the exporter's `scrape_timeout` (or Prometheus's own
   `scrape_timeout` tearing down the request) hit, so the scrape now fails
   loudly instead of silently returning partial data. If this happens after
   adding hosts, raise `global_concurrency` in `configs/exporter.yml` — more
   BMCs competing for the same crawl slots delays the ones queued behind them.

### Prometheus target shows DOWN

```bash
# Check exporter can reach the BMC
curl -k -u monitoring:password https://<bmc-ip>/redfish/v1/

# Check exporter logs for connection errors
docker compose logs redfish_exporter | grep <bmc-ip>
```

### Grafana shows "No data"

- The first scrape cycle takes up to 5 minutes after start
- Check Prometheus targets page — targets must be `UP`
- Verify the `instance` label in Prometheus matches the BMC IP in the dashboard variable

### Per-BMC credentials not working

Ensure the matching `password_env` variable is set in `.env` and that you ran
`docker compose up -d` after editing `.env` (environment variables are only read at startup).

### One host returns 401 while the rest of the fleet works

This means that specific BMC's `monitoring` account password has drifted
out of sync with `hosts.default` in `configs/exporter.yml` — it happens per
host, independent of any code/config change here, and has been seen on both
a standard Dell iDRAC and an AMI/HGX baseboard controller on this fleet.

1. Confirm it's a real credential mismatch, not a lockout:
   ```bash
   curl -k -u monitoring:$REDFISH_PASSWORD https://<bmc-ip>/redfish/v1/Systems
   ```
   A `401`/`AccessDenied` error body here (not a lockout message) means bad
   credentials, not a rate limit.
2. Log into that BMC's Redfish `AccountService` with an **admin** account to
   find the `monitoring` account and check its state:
   ```bash
   curl -k -u <admin-user>:<admin-pass> https://<bmc-ip>/redfish/v1/AccountService/Accounts
   ```
   Fetch each member to find the one with `"UserName": "monitoring"` — its
   account ID is **not fixed** (seen as `4`, `5`, and `6` on different hosts
   in this fleet), don't assume it's always the same number. Check
   `Locked`/`Enabled` — if both are healthy, it's a genuine password
   mismatch, not a lockout.
3. Resync the password via a Redfish `AccountService` PATCH (needs the
   current `ETag`, some firmware rejects the write without it):
   ```bash
   etag=$(curl -sk -u <admin-user>:<admin-pass> -D - -o /dev/null \
     https://<bmc-ip>/redfish/v1/AccountService/Accounts/<id> \
     | grep -i '^etag' | tr -d '\r' | sed 's/.*: //')
   curl -sk -u <admin-user>:<admin-pass> -X PATCH \
     -H "Content-Type: application/json" -H "If-Match: $etag" \
     -d "{\"Password\": \"$REDFISH_PASSWORD\"}" \
     https://<bmc-ip>/redfish/v1/AccountService/Accounts/<id>
   ```
4. Verify with the same `curl -u monitoring:...` check from step 1 — should
   now return `200`.

**If you rotate the fleet-wide default password** (`REDFISH_PASSWORD` in
`.env`), confirm the new password actually works against a few *other*,
currently-healthy hosts across different vendors **before** running
`docker compose up -d redfish_exporter` to apply it — the exporter only
reads `.env` at container start, so a bad rotation won't break anything
until you restart, but restarting with an unconfirmed rotation can take the
*entire* fleet down at once if the BMCs weren't actually updated to match.

### GPU temp data missing but scrape succeeds (`redfish_scrape_success=1`, no `redfish_gpu_temperature_celsius`)

If `redfish_gpu_health`/`redfish_gpu_info` are present but
`redfish_gpu_temperature_celsius` is completely absent for a host, first
check whether that vendor's firmware only exposes zone/aggregate sensors
rather than per-GPU ones (see Vendor Notes → Supermicro) — that's a firmware
limitation, not fixable here.

If instead the exporter logs show `401 Unauthorized` specifically on
`/Chassis` or `/Systems` partway through the crawl (`docker compose logs
redfish_exporter | grep <bmc-ip>`), but a manual, isolated
`curl -u monitoring:...` to the same host succeeds — this was previously an
unresolved issue on this fleet's NVIDIA HGX/AMI baseboard controllers (see
Vendor Notes → HPE), root-caused and fixed 2026-09-14: each BMC's own
`AccountService.AccountLockoutThreshold` was `5` (30-second lockout window).
HTTP Basic Auth re-authenticates on *every single request*, so a ~100+
resource crawl looks like a brute-force attempt to the BMC even with fully
correct credentials — that's what made it reproduce only under a real
fleet-wide scrape and never in an isolated manual test (one request never
crosses the threshold). Fix, per host, with an **Administrator** account
(the `monitoring` account is ReadOnly and can't read/write `AccountService`
— you'll get `Security.1.0.InsufficientPrivilege` if you try):

```bash
# 1. Check the current policy
curl -sk -u <admin-user>:<admin-pass> https://<bmc-ip>/redfish/v1/AccountService \
  | python3 -m json.tool
# Look for AccountLockoutThreshold, AccountLockoutDuration,
# AccountLockoutCounterResetAfter

# 2. Get a FRESH ETag immediately before patching (a stale one from an
#    earlier GET will 412 Precondition Failed — the ETag changes whenever
#    anything on this resource changes, so fetch right before you write)
etag=$(curl -sk -u <admin-user>:<admin-pass> https://<bmc-ip>/redfish/v1/AccountService \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['@odata.etag'])")

# 3. Disable the lockout (0 = disabled on AMI MegaRAC firmware) — reasonable
#    here since `monitoring` is a single-purpose, read-only automation
#    account, not a human login that needs brute-force protection
curl -sk -X PATCH -u <admin-user>:<admin-pass> \
  -H 'Content-Type: application/json' -H "If-Match: $etag" \
  -d '{"AccountLockoutThreshold": 0}' \
  https://<bmc-ip>/redfish/v1/AccountService

# 4. Verify — check the BMC's own AuditLog for new Security.1.0.LoginFailure/
#    AccessDenied entries over the next ~15-20 min (past a full crawl cycle);
#    none appearing is the real confirmation, not just one clean curl
curl -sk -u <admin-user>:<admin-pass> \
  "https://<bmc-ip>/redfish/v1/Managers/Self/LogServices/AuditLog/Entries?\$skip=<total-1>-50" \
  | python3 -m json.tool
```

Fixing the lockout is often not the whole story — see the next entry for a
second, independent issue on these same controllers.

### GPU temperature crawl silently truncated on NVIDIA HGX baseboard controllers

Even with the account lockout above fixed (or on a host that was never
locked out), these controllers can still report `redfish_scrape_success=1`
with `redfish_gpu_health`/`redfish_gpu_info`/`redfish_cpu_health` all
present but **zero** `redfish_gpu_temperature_celsius` — with no error
logged at all. This is a crawl-budget problem, not an auth problem:

- These hosts expose **40+ Chassis members** alone — per-GPU `HGX_ERoT_*`
  security co-processor, `HGX_NVSwitch_*`, `HGX_PCIeRetimer_*`,
  `HGX_PCIeSwitch_*`, plus the real per-GPU `HGX_GPU_SXM_*` chassis —
  several times the resource count of a typical Dell/Lenovo/SuperMicro host.
- The crawl (`internal/collector/collector.go`) visits links in
  **alphabetical order**, and `HGX_ERoT_GPU_SXM_*` sorts before
  `HGX_GPU_SXM_*` — so the fleet-default `max_resources` (120) can be
  silently exhausted on ERoT/NVSwitch/PCIeRetimer chassis before the crawl
  ever reaches the real GPU chassis' `Sensors`/`ThermalSubsystem`
  resources. This is a budget cutoff, not a fetch failure, so nothing gets
  logged.
- These hosts also expose `Systems/Self/Processors/{cpu}/SubProcessors/{core}`
  — one resource per physical CPU core — and per-DIMM memory detail, which
  inflates the crawl well past what raising `max_resources` alone can
  afford within `scrape_timeout` (200s).

Confirm it's this issue, not a permissions problem, by checking the
`monitoring` account can read the real GPU chassis directly:

```bash
curl -sk -u monitoring:$REDFISH_PASSWORD \
  https://<bmc-ip>/redfish/v1/Chassis/HGX_GPU_SXM_1/Sensors | python3 -m json.tool
# Should return real sensor members (…_TEMP_0, …_Power_0, etc.) with no
# access-denied error. If it does, the exporter just isn't reaching this
# path during its own crawl — it's not a credentials/role problem.
```

Fix with a per-host override (see Setup → Per-BMC overrides for the exact
config): raise `max_resources` well above the fleet default (600 was
sufficient on this fleet's 8-GPU HGX hosts), **and** cap `max_depth` at 4 —
GPU sensor readings sit at depth 4
(`Chassis/{id}/Sensors/{reading}`), shallower than per-core CPU detail at
depth 6, so capping depth trades away that per-core/per-drive granularity
for these hosts specifically in exchange for actually reaching GPU thermal
data within `scrape_timeout`. Rebuild and redeploy after editing
`configs/exporter.yml`, since this needs the corresponding code support in
`internal/config/config.go`/`internal/collector/collector.go` (already
present as of this fix) — a config-only edit isn't enough on an older build:

```bash
docker compose build redfish_exporter
docker compose up -d redfish_exporter
```

### Config edit doesn't seem to take effect after `docker compose up -d` / `/-/reload`

On Docker Desktop (observed on macOS), a bind-mounted config file can get
"stuck" serving stale content after certain kinds of host-side edits, even
though the reload/restart command reports success with no errors logged.
Before debugging your YAML/PromQL logic, rule this out:

```bash
# Compare line counts / grep for a string you just added
docker exec <container> wc -l <path-inside-container>
wc -l <path-on-host>
```

If they don't match, `docker compose restart <service>` (not just
`curl -X POST :9090/-/reload`) forces a fresh mount and picks up the real
current file.

### Grafana login stops working after `docker compose up -d` on an existing deployment

`GF_SECURITY_ADMIN_PASSWORD` (from `GRAFANA_ADMIN_PASSWORD` in `.env`) only
seeds the admin password on Grafana's **first-ever** database
initialization — it's silently ignored on every subsequent container
restart if the `grafana_data` Docker volume already exists from a previous
run (e.g. you changed `.env` after the stack had already been running for a
while). If `admin` / your `.env` password stops working, reset it directly:

```bash
docker exec grafana grafana cli admin reset-admin-password <new-password>
```

(Grafana 13.x renamed the binary from standalone `grafana-cli` to
`grafana cli` — use whichever matches your image's version.)

---

## Vendor Notes

| Vendor | BMC | GPU Thermal Coverage |
|---|---|---|
| Dell | iDRAC 9 / iDRAC 10 | Full — standard Redfish Thermal, one sensor per physical GPU (true 1:1), H200 baseboard sensors included |
| Lenovo | XCC | No GPU Processor resource exposed on this fleet's firmware — the exporter falls back to reading GPU temp/health from `Chassis/.../Thermal` PCIe slot sensors (matched by name containing `gpu` or `pcie`) instead of `Systems/.../Processors`. On this fleet these are genuinely 4-GPU servers, and the fallback reports exactly 4 sensors — a true 1:1, not a coverage gap. **`redfish_gpu_health`/`redfish_gpu_info` are permanently absent for these hosts** (no GPU Processor resource exists to derive them from), not a bug — only temperature/health-via-the-fallback work. Any dashboard/alert built on `redfish_gpu_health` needs a Lenovo-aware, temperature-based fallback, same as the GPU Count Mismatch dashboard's own approach (see Dashboards → GPU Count Mismatch) |
| Supermicro | BMC / IPMI | **Coarser than other vendors: only 3 zone/aggregate GPU-related sensors per 8-GPU server** (`GPU Temp`, `GPU Inlet Temp`, `GPU HSC Temp`), not one per physical GPU. This is a firmware/Redfish-exposure limitation, not an exporter bug — there is currently no known Redfish path on this firmware that exposes per-GPU thermal data. This is exactly why `gpu_count` in inventory (not sensor counting) is the source of truth for GPU totals — see Step 3 and Metrics Reference. Note: `redfish_gpu_health`/`redfish_gpu_info` *are* fully 1:1 per physical GPU on this firmware (confirmed: 8/8 health entries even when only 3/8 temperature readings are exposed) — only temperature is coarse |
| HPE | iLO 5 / iLO 6, **or an embedded NVIDIA HGX/AMI MegaRAC baseboard controller** | Some hosts labeled vendor "HPE" in inventory are actually the GPU baseboard's own separate embedded BMC (AMI MegaRAC firmware, distinct from the chassis's real HPE iLO) — identifiable via `/redfish/v1/Chassis` members like `HGX_Baseboard_0`, `HGX_GPU_SXM_*`, `HGX_ERoT_*`. These report full per-GPU sensors (`GPU0_PROC`...`GPU7_PROC`) when reachable, a true 1:1 (6 such hosts on this fleet: `10.20.32.75/114/122/123/124/125`). **Two previously-unresolved issues here were root-caused and fixed 2026-09-14** (see Troubleshooting → "GPU temp data missing but scrape succeeds" and "GPU temperature crawl silently truncated…"): (1) each BMC's own `AccountService.AccountLockoutThreshold` (5 failed logins/30s) was tripped by Basic Auth re-authenticating on every crawl request — fixed by setting it to `0` per host; (2) these hosts' 40+-member Chassis graph (ERoT/NVSwitch/PCIeRetimer chassis alphabetically ahead of the real GPU chassis, plus per-core CPU/per-DIMM detail) silently exhausted the fleet-default crawl budget before reaching GPU sensors — fixed with a per-host `max_resources`/`max_depth` override. |
| Tyrone | Tyrone BMC | Not currently present on this fleet — listed as a supported `vendor` value for future use. If you add Tyrone hosts, verify GPU sensor path first: standard `/Chassis/Thermal` preferred, OEM extension adapter needed if not standard |

**A per-vendor BMC credential can silently drift from the fleet-wide
default** even when nothing in this repo changed — seen on both an AMI/HGX
board and a standard Dell iDRAC on this fleet, unrelated occurrences. Symptom
is `401 Unauthorized` / `AccessDenied` on a specific host while the rest of
the fleet using the same `hosts.default` credentials in `configs/exporter.yml`
works fine. This is a genuine password mismatch on that BMC, not a bug in the
exporter — see Troubleshooting → "One host returns 401 while the rest of the
fleet works."

---

## Scaling Notes

At 245 servers with a 5-minute scrape interval:

- ~0.8 BMC Redfish hits per second across the fleet — still light on the management network
- A single exporter instance is sufficient
- `global_concurrency` in `configs/exporter.yml` is set to 85 (raised from 30
  when the fleet grew from 85 to 245 hosts), sized so a full cold-cache sweep
  (e.g. right after an exporter restart) finishes within the 6-minute cache
  TTL instead of queuing crawls past it — see the comment on that setting for
  the math, and note it deliberately preserves the same ~3-batch worst-case
  timing envelope validated at 85/30 rather than scaling 1:1 with server count
- We're already past the ~150–200-server threshold below for running a
  second exporter, and haven't needed to yet — a single instance still
  clears a full cold sweep within `cache_ttl` at `global_concurrency: 85`.
  Re-check the math in the config comment before growing further
- If you later expand well past 245 servers with heavy Redfish payloads, run
  a second exporter and split targets by site across two Prometheus scrape
  jobs

**When you add more servers:** re-run `generate_targets.py`, then re-check
`global_concurrency` (`configs/exporter.yml`) and `scrape_timeout` (the
`redfish` job in `prometheus/prometheus.yml`) — both are currently sized for
245 hosts. Rule of thumb: worst-case cold-sweep time is
`(server_count / global_concurrency) * ~100s`; keep that comfortably under
`cache_ttl` (6m) by raising `global_concurrency`, and keep the Prometheus
job's `scrape_timeout` above `exporter scrape_timeout (200s) + expected queue
wait` while staying under `scrape_interval` (300s).

To run a local test without a real BMC:

```bash
make run
curl "http://localhost:9610/-/healthy"
```
