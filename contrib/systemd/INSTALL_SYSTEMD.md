# BetaWatch systemd Installation Guide

This guide explains how to install and run BetaWatch as two systemd
services: `betawatch-ingest`, which follows the OSM minute diffs into a
shared SQLite database, and `betawatch-feed`, which serves the web view
from that database. A timer, `betawatch-check-commons.timer`, runs a daily
check of the Wikimedia Commons files those elements reference.

## Prerequisites

- BetaWatch binaries built and ready to deploy
- systemd on your system
- Root or sudo access

## Installation Steps

### 1. Build the Binaries

```bash
go build -o betawatch-ingest ./cmd/ingest
go build -o betawatch-feed ./cmd/feed
```

### 2. Create System User and Directories

```bash
# Create betawatch user and group
sudo useradd --system --home /var/lib/betawatch --shell /usr/sbin/nologin betawatch

# Create directories
sudo mkdir -p /etc/betawatch /var/lib/betawatch

# Set permissions
sudo chown betawatch:betawatch /var/lib/betawatch
sudo chmod 700 /var/lib/betawatch
sudo chown betawatch:betawatch /etc/betawatch
sudo chmod 755 /etc/betawatch
```

### 3. Install Configuration File

Both services read the same config file, since it holds the SQLite
database path they share.

```bash
sudo cp etc-betawatch-config.yaml /etc/betawatch/config.yaml
sudo chown betawatch:betawatch /etc/betawatch/config.yaml
sudo chmod 640 /etc/betawatch/config.yaml
```

Edit the configuration as needed:

```bash
sudo nano /etc/betawatch/config.yaml
```

### 4. Install Binaries

```bash
sudo cp betawatch-ingest /usr/local/bin/betawatch-ingest
sudo cp betawatch-feed /usr/local/bin/betawatch-feed
sudo chmod 755 /usr/local/bin/betawatch-ingest /usr/local/bin/betawatch-feed
```

### 5. Install Systemd Service Files

```bash
sudo cp betawatch-ingest.service betawatch-feed.service \
    betawatch-check-commons.service betawatch-check-commons.timer /etc/systemd/system/
sudo systemctl daemon-reload
```

### 6. Enable and Start the Services

```bash
# Enable services to start on boot
sudo systemctl enable betawatch-ingest betawatch-feed

# Enable the daily Wikimedia Commons check and start its timer
sudo systemctl enable --now betawatch-check-commons.timer

# Start the services
sudo systemctl start betawatch-ingest betawatch-feed

# Check status
sudo systemctl status betawatch-ingest betawatch-feed
sudo systemctl list-timers betawatch-check-commons.timer
```

## Managing the Services

### View Logs

```bash
# Real-time logs
sudo journalctl -u betawatch-ingest -f
sudo journalctl -u betawatch-feed -f
sudo journalctl -u betawatch-check-commons -f

# Last 50 lines
sudo journalctl -u betawatch-ingest -n 50
sudo journalctl -u betawatch-feed -n 50
sudo journalctl -u betawatch-check-commons -n 50

# Since a specific time
sudo journalctl -u betawatch-ingest --since "2 hours ago"
```

### Restart Services

```bash
sudo systemctl restart betawatch-ingest betawatch-feed
```

### Stop Services

```bash
sudo systemctl stop betawatch-ingest betawatch-feed
```

### Check Service Status

```bash
sudo systemctl status betawatch-ingest betawatch-feed
```

## Configuration Changes

After modifying `/etc/betawatch/config.yaml`:

```bash
# Reload configuration (restart both services)
sudo systemctl restart betawatch-ingest betawatch-feed

# Watch logs to verify changes
sudo journalctl -u betawatch-ingest -u betawatch-feed -f
```

## Accessing the Web View

If `http_addr` is set in the config (default `:8080`), access the web view at:

```
http://your-server:8080
```

## Wikimedia Commons check

`betawatch-check-commons.timer` runs `betawatch-ingest -check-commons` once
a day. It looks up every `wikimedia_commons` File: value of the stored
elements that has not been checked yet, records the file's dimensions, size
and MIME type in the `commons_files` table, and drops rows for files no
element references anymore. Files are treated as immutable, so a file is
looked up once, never again.

Run the check immediately instead of waiting for the timer:

```bash
sudo systemctl start betawatch-check-commons.service
sudo journalctl -u betawatch-check-commons -n 20
```

The problems page flags elements whose checked files are missing or
invalid; elements whose files have not been checked yet are not flagged.

## MCP endpoint

With `mcp_enabled: true`, `betawatch-feed` also serves a Model Context
Protocol endpoint (streamable HTTP) at `http://your-server:8080/assistant`. It
offers two read-only tools to an LLM: `get_schema` (tables, columns,
indexes) and `query` (one `SELECT`, at most 10000 rows, cancelled after 10
seconds).

It has no authentication. Anyone who can reach the port can run queries,
and a heavy one loads the server. Restrict `/assistant` in your reverse proxy
(an IP allow-list or basic auth) before turning it on at a public address.

## Troubleshooting

### A service fails to start

Check logs:
```bash
sudo journalctl -u betawatch-ingest -n 30
sudo journalctl -u betawatch-feed -n 30
```

Common issues:
- Config file not readable: Check file permissions
- Port already in use: Change `http_addr` in config (only `betawatch-feed` binds a port)
- State or database directory not writable: Check `/var/lib/betawatch` permissions
- `betawatch-feed` finds no data: `betawatch-ingest` populates the database; give it a moment on first run, or check its logs

### Slow startup

Adjust `backfill_duration` to catch up on fewer minutes:
```yaml
backfill_duration: "30m"  # Catch up on last 30 minutes instead of 2 hours
```

This only affects `betawatch-ingest`.

## Uninstall

```bash
sudo systemctl disable --now betawatch-check-commons.timer
sudo systemctl stop betawatch-ingest betawatch-feed
sudo systemctl disable betawatch-ingest betawatch-feed
sudo rm /etc/systemd/system/betawatch-ingest.service /etc/systemd/system/betawatch-feed.service \
        /etc/systemd/system/betawatch-check-commons.service /etc/systemd/system/betawatch-check-commons.timer
sudo rm /usr/local/bin/betawatch-ingest /usr/local/bin/betawatch-feed
sudo rm -rf /etc/betawatch /var/lib/betawatch
sudo userdel betawatch
sudo systemctl daemon-reload
```
