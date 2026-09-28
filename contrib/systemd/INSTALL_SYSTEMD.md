# BetaWatch systemd Installation Guide

This guide explains how to install and run BetaWatch as a systemd service.

## Prerequisites

- BetaWatch binary built and ready to deploy
- systemd on your system
- Root or sudo access

## Installation Steps

### 1. Build the Binary

```bash
cd cmd/betawatch
go build -o betawatch
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

Copy and configure the production config:

```bash
sudo cp etc-betawatch-config.yaml /etc/betawatch/config.yaml
sudo chown betawatch:betawatch /etc/betawatch/config.yaml
sudo chmod 640 /etc/betawatch/config.yaml
```

Edit the configuration as needed:

```bash
sudo nano /etc/betawatch/config.yaml
```

### 4. Install Binary

```bash
sudo cp cmd/betawatch/betawatch /usr/local/bin/betawatch
sudo chmod 755 /usr/local/bin/betawatch
```

### 5. Install Systemd Service File

```bash
sudo cp betawatch.service /etc/systemd/system/
sudo systemctl daemon-reload
```

### 6. Enable and Start the Service

```bash
# Enable service to start on boot
sudo systemctl enable betawatch

# Start the service
sudo systemctl start betawatch

# Check status
sudo systemctl status betawatch
```

## Managing the Service

### View Logs

```bash
# Real-time logs
sudo journalctl -u betawatch -f

# Last 50 lines
sudo journalctl -u betawatch -n 50

# Since a specific time
sudo journalctl -u betawatch --since "2 hours ago"
```

### Restart Service

```bash
sudo systemctl restart betawatch
```

### Stop Service

```bash
sudo systemctl stop betawatch
```

### Check Service Status

```bash
sudo systemctl status betawatch
```

## Configuration Changes

After modifying `/etc/betawatch/config.yaml`:

```bash
# Reload configuration (restart service)
sudo systemctl restart betawatch

# Watch logs to verify changes
sudo journalctl -u betawatch -f
```

## Accessing the Web View

If `http_addr` is set in the config (default `:8080`), access the web view at:

```
http://your-server:8080
```

## Troubleshooting

### Service fails to start

Check logs:
```bash
sudo journalctl -u betawatch -n 30
```

Common issues:
- Config file not readable: Check file permissions
- Port already in use: Change `http_addr` in config
- State directory not writable: Check `/var/lib/betawatch` permissions

### High memory usage

Adjust `retain_duration` in config to keep fewer changes:
```yaml
retain_duration: "72h"  # Keep 3 days instead of 7
```

### Slow startup

Adjust `backfill_duration` to catch up on fewer minutes:
```yaml
backfill_duration: "30m"  # Catch up on last 30 minutes instead of 2 hours
```

## Uninstall

```bash
sudo systemctl stop betawatch
sudo systemctl disable betawatch
sudo rm /etc/systemd/system/betawatch.service
sudo rm /usr/local/bin/betawatch
sudo rm -rf /etc/betawatch /var/lib/betawatch
sudo userdel betawatch
sudo systemctl daemon-reload
```
