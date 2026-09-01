# Rmm-monitoring-stack

Out-of-band GPU server monitoring for mixed Dell, Lenovo, Supermicro, HPE, and Tyrone
fleets. Nothing runs on the server guest OS. All data comes from the BMC (Baseboard
Management Controller) over the management network using the Redfish API.

---

## How It Works (Architecture)
```
┌─────────────────────────────────────────────────────────────────┐
│                       Your Fleet (85 servers)                    │
│                                                                  │
│  Dell iDRAC   Lenovo XCC   Supermicro BMC   HPE iLO   Tyrone BMC │
│  (H200/H100)   (H100)       (H200/H100)     (H100)      (B200)  │
│      │            │              │             │           │    │
└──────┼────────────┼──────────────┼─────────────┼───────────┼───┘
       │            │              │             │           │
       │       Management Network (out-of-band VLAN)          │
       └────────────┴───────┬──────┴─────────────┴───────────┘
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
| Standard Redfish API only | Works across Dell, Supermicro, HPE, Tyrone without vendor-specific code |
| 6-minute cache in exporter | BMC gets hit once per cycle; Prometheus polls exporter but never floods BMC |
| 5-minute Prometheus scrape interval | Matches the cache window; 85 servers is light network load (~0.3 BMC hits/sec) |
| Labels from inventory CSV | Vendor, site, GPU type stay attached to every metric in Grafana |

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
│       └── top_10_hottest_gpus.json ← top 10 with site/vendor filter
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

Four dashboards are auto-provisioned. They form a drill-down chain:

```
OEM Fleet – GPU Drill-Down
  │
  │  Pick an OEM (Dell / Supermicro / HPE / Tyrone)
  │  Pick a GPU type (B200 / H200 / H100)
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

`instance` = BMC IP address. `vendor`, `gpu`, `site`, `gpu_count` come from
`inventory.csv` and are attached as target labels to every metric from that
host (Prometheus merges them in automatically) — only
`redfish_gpu_count_configured` additionally uses `gpu_count` as its actual
*value*, via `__param_gpu_count` (see `prometheus/prometheus.yml`).

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
| `HighTemperature` | `redfish_temperature_celsius > 80` | warning | 5 min |
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

**Per-BMC overrides** (credentials and/or crawl concurrency):

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
ip,vendor,site,gpu,gpu_count
10.10.1.10,dell,dc1,h200,8
10.10.1.11,dell,dc1,h200,8
10.10.2.10,supermicro,dc1,h200,8
10.10.3.10,supermicro,dc1,h100,8
10.10.4.10,hpe,dc1,h100,4
10.10.5.10,tyrone,dc2,b200,8
```

**Field values:**

| Field | Allowed values | Notes |
|---|---|---|
| `vendor` | `dell`, `lenovo`, `supermicro`, `hpe`, `tyrone` | Used as Grafana filter label |
| `site` | any string, e.g. `dc1`, `dc2` | Your data centre name |
| `gpu` | `b200`, `h200`, `h100` (priority) | GPU card type in that server |
| `gpu_count` | integer, e.g. `4`, `8` | Physical GPU count in that server — see below for why this can't be inferred from live telemetry |

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
    - '10.10.1.11'
  labels:
    vendor: dell
    site: dc1
    gpu: h200
    gpu_count: "8"
```

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

All four dashboards are in the **Redfish Infrastructure** folder in the left nav.
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
`curl -u monitoring:...` to the same host succeeds every time — this is a
known, **unresolved** issue on this fleet's NVIDIA HGX/AMI baseboard
controllers (see Vendor Notes → HPE). Ruled out so far: wrong credentials,
account lockout, keep-alive/connection-reuse auth bugs, client
fingerprinting, cross-host shared-auth-backend collisions, Docker network
path. It only reproduces during a full-fleet concurrent scrape, never in an
isolated manual test — suspected (unconfirmed) link to congestion on the
shared BMC/OOB network under a high `global_concurrency`. If you hit this,
don't assume it's the same per-host credential drift covered above — verify
with an isolated `curl` test first, since the fix for that (PATCH the
password) won't do anything here.

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
| Lenovo | XCC | No GPU Processor resource exposed on this fleet's firmware — the exporter falls back to reading GPU temp/health from `Chassis/.../Thermal` PCIe slot sensors (matched by name containing `gpu` or `pcie`) instead of `Systems/.../Processors`. On this fleet these are genuinely 4-GPU servers, and the fallback reports exactly 4 sensors — a true 1:1, not a coverage gap |
| Supermicro | BMC / IPMI | **Coarser than other vendors: only 3 zone/aggregate GPU-related sensors per 8-GPU server** (`GPU Temp`, `GPU Inlet Temp`, `GPU HSC Temp`), not one per physical GPU. This is a firmware/Redfish-exposure limitation, not an exporter bug — there is currently no known Redfish path on this firmware that exposes per-GPU thermal data. This is exactly why `gpu_count` in inventory (not sensor counting) is the source of truth for GPU totals — see Step 3 and Metrics Reference |
| HPE | iLO 5 / iLO 6, **or an embedded NVIDIA HGX/AMI MegaRAC baseboard controller** | Some hosts labeled vendor "HPE" in inventory are actually the GPU baseboard's own separate embedded BMC (AMI MegaRAC firmware, distinct from the chassis's real HPE iLO) — identifiable via `/redfish/v1/Chassis` members like `HGX_Baseboard_0`, `HGX_GPU_SXM_*`, `HGX_ERoT_*`. These report full per-GPU sensors (`GPU0_PROC`...`GPU7_PROC`) when reachable, a true 1:1. **Known issue:** these controllers' `monitoring` account can intermittently return `401 Unauthorized` on `/Systems`/`/Chassis` specifically during a full-fleet concurrent scrape, even though isolated requests always succeed — not resolved as of this writing, suspected but unconfirmed link to congestion on the shared BMC/OOB network under high `global_concurrency`. See Troubleshooting → "GPU temp data missing but scrape succeeds." |
| Tyrone | Tyrone BMC | Verify GPU sensor path — standard `/Chassis/Thermal` preferred; OEM extension adapter needed if not standard |

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

At 85 servers with a 5-minute scrape interval:

- ~0.3 BMC Redfish hits per second across the fleet — light on the management network
- A single exporter instance is sufficient
- `global_concurrency` in `configs/exporter.yml` is set to 30, sized so a full
  cold-cache sweep (e.g. right after an exporter restart) finishes within the
  6-minute cache TTL instead of queuing crawls past it — see the comment on
  that setting for the math
- If you later expand past ~150–200 servers with heavy Redfish payloads, run a second
  exporter and split targets by site across two Prometheus scrape jobs

**When you add more servers:** re-run `generate_targets.py`, then re-check
`global_concurrency` (`configs/exporter.yml`) and `scrape_timeout` (the
`redfish` job in `prometheus/prometheus.yml`) — both were sized for 85 hosts.
Rule of thumb: worst-case cold-sweep time is
`(server_count / global_concurrency) * ~100s`; keep that comfortably under
`cache_ttl` (6m) by raising `global_concurrency`, and keep the Prometheus
job's `scrape_timeout` above `exporter scrape_timeout (200s) + expected queue
wait` while staying under `scrape_interval` (300s).

To run a local test without a real BMC:

```bash
make run
curl "http://localhost:9610/-/healthy"
```
>>>>>>> 880c386 (RMM Monitoring metrics working for GPU's under load)
