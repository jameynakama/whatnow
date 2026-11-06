# whatnow: Docker personal mode

Run `whatnow serve` in a Linux container with Docker Compose. This is one person's planner, with
the same settings and integrations as a local install. Shared hosting remains the separate
`whatnow host` workflow in `docs/hosting.md`.

## Build and runtime

- Build the static web UI with Node 24 and `npm ci`, then embed it in a Go 1.27 binary, as the
  existing build does. Keep the runtime image free of build tools.
- Use an Ubuntu 24.04 runtime image as UID/GID 10001, with a read-only root filesystem,
  temporary files in `/tmp`, no Linux capabilities, and no privilege escalation. Tini forwards
  signals and reaps child processes. The build uses the builder's native architecture.
- Listen on port 7777 inside the container. Compose publishes it on host loopback by default.
  `WHATNOW_BIND_ADDRESS` and `WHATNOW_PORT` allow an explicit interface and host port. The
  existing allowed-host check still applies; it does not authenticate visitors. Remote access
  uses SSH forwarding, a private network, or a proxy that authenticates the entire app.
- Pass `TZ` to the process and include timezone data so personal-mode planning uses the
  configured local day. Check health through the settings API. Docker's restart policy handles
  process exits, not an unhealthy status alone.

## Secrets and persistence

Personal mode already stores secrets through the OS keychain. Give it a private D-Bus session
and GNOME Keyring rather than adding another application secret backend. Unlock the login
keyring before starting the app, and refuse startup if the password is missing, empty, or
cannot unlock the collection.

Compose mounts the password from a local file. Pass it to the daemon through stdin, never an
argument or environment variable. Exclude the password and `.env` from Git and the build
context. These local Compose secrets are files, not an encrypted secret vault.

Keep SQLite at `/data/whatnow.db` and the encrypted keyring under
`/data/.local/share/keyrings`, in one named volume owned by the runtime user. Container
replacement preserves both. Back up the entire volume while the service is stopped, and
retain the original password separately. Restore into an empty volume with that password.

## Validation

Run `just test`, `just lint`, and `just e2e` for the existing application. Run
`sh docker/smoke-test.sh` for behavior those suites do not exercise: the built UI and API under
the runtime restrictions, saving an API key, retaining a task and key across container
replacement, restoring a volume archive, and rejecting missing, empty, and incorrect keyring
passwords. Use disposable data, publish no ports, and make no model calls.

Docker Desktop, ARM64 builds, and real integration/model connections need separate testing on
the intended deployment. The smoke check runs on the local Docker engine's architecture.
