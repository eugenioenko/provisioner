# Provisioner

A lightweight demo provisioner for [Autentico](https://github.com/eugenioenko/autentico). Spawns ephemeral Autentico instances on demand, proxies traffic to them by subdomain, and cleans them up after a configurable TTL.

Requires **Autentico 3.0 or newer**. Download the Autentico binary separately and point `AUTENTICO_BINARY` at it. Compatibility is tested against released Autentico 3.0.0 and 3.2.0 binaries.

## How it works

1. A visitor hits `/launch` and clicks to start a demo
2. The provisioner runs `autentico start --auto-setup` in a private directory for each demo. Autentico generates its own `.env`, signing key, token secrets, TOTP encryption key and database.
3. Once the instance's OIDC discovery document reports the expected public issuer, the visitor is redirected to `{BASE_SCHEME}://{slug}.{BASE_DOMAIN}/onboard/` to create their administrator account.
4. Instances expire after `DEMO_TTL` (default 24 h) and are cleaned up automatically.

The provisioner's working-directory `.env` and inherited `AUTENTICO_*` variables are not used by child instances. Each demo has independent secrets and settings. Active instance logs are stored at `{DATA_DIR}/{slug}/autentico.log`; failed launches are reaped and their temporary directories removed.

## Configuration

All settings are via environment variables:

| Variable           | Default                 | Description                                  |
|--------------------|-------------------------|----------------------------------------------|
| `BASE_DOMAIN`      | `demo.getautentico.dev` | Root domain; instances get a subdomain        |
| `BASE_SCHEME`      | `https`                 | `http` for local testing                     |
| `BASE_PORT`        | _(empty)_               | Override port in URLs, e.g. `8080` for local |
| `AUTENTICO_BINARY` | `./autentico`           | Path to the autentico binary                 |
| `DATA_DIR`         | `./demos`               | Directory for per-instance data              |
| `LISTEN_ADDR`      | `:8080`                 | Address the provisioner listens on           |
| `DEMO_TTL`         | `24h`                   | How long instances live before cleanup       |
| `MAX_INSTANCES`    | `15`                    | Hard cap on concurrent instances             |
| `MAX_PER_IP`       | `3`                     | Max active instances per client IP           |

For HTTPS deployments, configure wildcard DNS and TLS for `*.{BASE_DOMAIN}`, preserve the incoming `Host` header, and proxy traffic to `LISTEN_ADDR`. Secure cookies are enabled for HTTPS demos. The child instances trust the provisioner's loopback proxy for forwarded client IPs.

For local HTTP testing, set `BASE_SCHEME=http`, `BASE_DOMAIN=localhost`, and `BASE_PORT=8080`. The provisioner passes `--dev` to Autentico so browser cookies work over HTTP. Your browser must resolve `*.localhost` to loopback.

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

## Testing

```bash
go test -race ./...
go vet ./...
AUTENTICO_TEST_BINARY=/absolute/path/to/autentico go test -race -run TestAutenticoCompatibility -v ./...
```

The compatibility test creates disposable HTTP and HTTPS-configured demos, checks secret and database isolation, submits onboarding with CSRF protection, completes admin SSO with Authorization Code + PKCE, calls the authenticated admin API, and verifies TTL cleanup. It sends proxy requests locally and does not need wildcard DNS or a TLS certificate. CI runs this test against both supported reference releases before publishing binaries.

## Releases

Pre-built binaries for Linux amd64 and arm64 are attached to each [GitHub release](https://github.com/eugenioenko/provisioner/releases).
