# Provisioner

A lightweight demo provisioner for [Autentico](https://github.com/eugenioenko/autentico). Spawns ephemeral Autentico instances on demand, proxies traffic to them by subdomain, and cleans them up after a configurable TTL.

## How it works

1. A visitor hits `/launch` and clicks to start a demo
2. The provisioner spawns a fresh `autentico` process with isolated data, waits for it to be fully ready, then redirects the user to `{slug}.{BASE_DOMAIN}`
3. Instances expire after `DEMO_TTL` (default 24 h) and are cleaned up automatically

## Configuration

All settings are via environment variables:

| Variable           | Default                 | Description                                  |
|--------------------|-------------------------|----------------------------------------------|
| `BASE_DOMAIN`      | `demo.autentico.top`    | Root domain; instances get a subdomain        |
| `BASE_SCHEME`      | `https`                 | `http` for local testing                     |
| `BASE_PORT`        | _(empty)_               | Override port in URLs, e.g. `8080` for local |
| `AUTENTICO_BINARY` | `./autentico`           | Path to the autentico binary                 |
| `DATA_DIR`         | `./demos`               | Directory for per-instance data              |
| `LISTEN_ADDR`      | `:8080`                 | Address the provisioner listens on           |
| `DEMO_TTL`         | `24h`                   | How long instances live before cleanup       |
| `MAX_INSTANCES`    | `15`                    | Hard cap on concurrent instances             |
| `MAX_PER_IP`       | `3`                     | Max active instances per client IP           |

## Running with systemd

A sample unit file is included at `autentico-provisioner.service`. Copy it to `/etc/systemd/system/`, adjust the paths, then:

```bash
systemctl daemon-reload
systemctl enable --now autentico-provisioner
```

## Building

```bash
go build -o provisioner .
```

## Releases

Pre-built binaries for Linux amd64 and arm64 are attached to each [GitHub release](https://github.com/eugenioenko/provisioner/releases).
