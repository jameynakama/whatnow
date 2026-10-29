# Docker

Run personal mode (`whatnow serve`) with Docker Compose. The image includes the web UI, Go
binary, and a private GNOME Keyring for model API keys and integration secrets. A named volume
keeps the database and encrypted keyring across container replacements. Configure the app
through the web UI.

You need Docker with Linux container support and Docker Compose. Use Docker Engine on Linux
or Docker Desktop on macOS or Windows. The container uses an Ubuntu 24.04 base image.

The lifecycle smoke test has passed on Linux x86-64. Docker Desktop and ARM64 have not been
tested.

Personal mode has no login. By default, the app is accessible only from the Docker host.
For remote access, restrict access through a private network or an authenticating proxy.
Anyone who can reach the app through an allowed hostname has access to the same personal data.

## First start

Install [Docker Engine](https://docs.docker.com/engine/install/) and the
[Compose plugin](https://docs.docker.com/compose/install/linux/) on Linux, or
[Docker Desktop](https://docs.docker.com/get-started/get-docker/) on macOS or Windows.
Check that `docker compose version` works. The image builds with Go and Node, so you do not
need to install those tools on the host.

Run these commands from the root of the cloned repository in a POSIX shell, such as bash or
zsh. On Windows, use a WSL shell with the repository in its Linux filesystem so the password
file permissions below can be applied.

```sh
cp .env.example .env
mkdir -m 700 secrets
openssl rand -base64 32 | tr -d '\n' > secrets/keyring_password
chmod 444 secrets/keyring_password
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 whatnow
```

Set `TZ` in `.env` to your IANA timezone, such as `America/Denver` or `Europe/London`.
The first build downloads dependencies and compiles the app, so it can take several minutes on
an older laptop. Subsequent builds reuse cached dependencies. Builds use the Docker engine's
native architecture, including x86-64 and ARM64, with no architecture hardcoded in the image.

The password file must be readable by the container's user (UID 10001). Compose mounts local
file secrets with their existing permissions, so the file is readable while its parent directory
is accessible only to your host user. Keep that directory at mode 700. The password is passed
to GNOME Keyring through stdin, not through environment variables or command arguments.
Compose secrets here are local files, not an encrypted secret vault.

Keep a copy of this password in your password manager. **Do not regenerate it on upgrades:**
the existing keyring needs the same password after a restart or restore. The container refuses
to start if it cannot unlock the keyring. The password file and `.env` are excluded from Git
and the Docker build context.

The app is published only at `127.0.0.1:7777` on the server by default. If your browser is on
that machine, visit `http://localhost:7777` directly. To set it up from another computer,
open an SSH tunnel in a separate terminal:

```sh
ssh -N -L 7777:127.0.0.1:7777 your-user@your-server
```

Visit `http://localhost:7777` on your computer, then configure your model and API key.
If the server's port 7777 is already used, change `WHATNOW_PORT` in `.env` and the destination
port in the SSH command (for example, `-L 7777:127.0.0.1:7780`).

## Optional remote access

No tunnel service or external account is required. You can use SSH forwarding, a private LAN,
a VPN such as WireGuard, or a reverse proxy that fits your existing setup.

For direct LAN or VPN access, set `WHATNOW_BIND_ADDRESS` in `.env` to the server's address on
that interface, then run `docker compose up -d` to apply it. For example, if the server's
WireGuard address is `10.8.0.1`, set `WHATNOW_BIND_ADDRESS=10.8.0.1`. That interface must be
available when the container starts. Keep access restricted to the intended network.

Before switching from loopback, use the local UI or SSH forwarding to add the hostname or IPv4
address you will visit under **Remote access** in whatnow's settings. Enter only the hostname
or address, without a scheme, port, or path. This setting permits the Host header; it does not
authenticate visitors.

A proxy running directly on the server can forward to `http://127.0.0.1:7777`. A proxy or tunnel
connector running in Docker can share a Docker network with whatnow and forward to
`http://whatnow:7777`. Configure that shared network in your own Compose override and preserve
the browser's Host header. Only attach trusted containers to that network.

If you publish the app on the Internet, put authentication in front of the entire app, including
its API, and prevent direct access that bypasses it. Local-only and private VPN deployments do
not require a public hostname or a tunnel provider.

## Updates and maintenance

```sh
git pull
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 whatnow
```

To refresh the base images too, run `docker compose build --pull` before `docker compose up -d`.
Review incoming changes before updating. The service restarts after a crash or host
reboot as long as Docker starts on boot. The health check reports whether the settings API
responds; an unhealthy status alone does not trigger Docker's restart policy.

`docker compose down` stops and removes the container but keeps its volume. **Do not add `-v`**
unless you intend to delete the database and keyring. The service uses a read-only root
filesystem, with writable persistent storage at `/data` and temporary runtime files in `/tmp`.

If startup reports that the keyring is locked, check the password file and restore its original
contents. If it reports a permissions error, check that `secrets` is mode 700 and the password
file is mode 444. Named volumes are initialized with the image's ownership; a replacement bind
mount must be writable by UID/GID 10001.

Addresses such as `localhost` in integration/model settings refer to the whatnow container.
For another container, use its DNS name on a shared Docker network. For a service on the host,
use an address reachable from Docker and ensure that service listens on it.

## Backup and restore

Back up the whole `/data` volume, including `.local/share/keyrings`, and keep the password
separately in your password manager. Stop the service while archiving to keep SQLite and the
keyring consistent:

```sh
umask 077
docker compose stop whatnow
docker compose run --rm --no-deps -T --entrypoint tar whatnow -C /data -czf - . > whatnow-backup.tar.gz
docker compose start whatnow
chmod 600 whatnow-backup.tar.gz
```

The archive includes all personal data. Keep it private. To restore into a fresh volume, put
the original password in `secrets/keyring_password` and run:

```sh
docker compose build
docker compose run --rm --no-deps -T --entrypoint tar whatnow -C /data -xzf - < whatnow-backup.tar.gz
docker compose up -d
```

Restore into an empty volume to avoid mixing databases or keyrings from different installations.
Without the original password, the restored database is readable but its saved secrets cannot
be unlocked.

## Development checks

Run the usual contribution checks (`just test`, `just lint`, and `just e2e`), then test the
container lifecycle from the repository root:

```sh
sh docker/smoke-test.sh
```

The script builds an image and uses temporary containers, volumes, and password files. It
checks the web UI, API key storage, database and keyring persistence across container
replacement, backup and restore, and rejection of missing, empty, or incorrect passwords.
It publishes no ports, calls no model, and removes its containers, volumes, and temporary files
on exit. The built image remains available as `whatnow-smoke:local`.
