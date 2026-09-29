# BetaWatch systemd Installation Guide

This guide explains how to install and run BetaWatch as two systemd
services: `betawatch-ingest`, which follows the OSM minute diffs into a
shared SQLite database, and `betawatch-feed`, which serves the web view
from that database.

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
sudo cp betawatch-ingest.service betawatch-feed.service /etc/systemd/system/
sudo systemctl daemon-reload
```

### 6. Enable and Start the Services

```bash
# Enable services to start on boot
sudo systemctl enable betawatch-ingest betawatch-feed

# Start the services
sudo systemctl start betawatch-ingest betawatch-feed

# Check status
sudo systemctl status betawatch-ingest betawatch-feed
```

## Managing the Services

### View Logs

```bash
# Real-time logs
sudo journalctl -u betawatch-ingest -f
sudo journalctl -u betawatch-feed -f

# Last 50 lines
sudo journalctl -u betawatch-ingest -n 50
sudo journalctl -u betawatch-feed -n 50

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
sudo systemctl stop betawatch-ingest betawatch-feed
sudo systemctl disable betawatch-ingest betawatch-feed
sudo rm /etc/systemd/system/betawatch-ingest.service /etc/systemd/system/betawatch-feed.service
sudo rm /usr/local/bin/betawatch-ingest /usr/local/bin/betawatch-feed
sudo rm -rf /etc/betawatch /var/lib/betawatch
sudo userdel betawatch
sudo systemctl daemon-reload
```
