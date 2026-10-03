# Palm Agent

Static Linux agent for Palm VPN node telemetry and active client IP reports.
The repository intentionally contains no endpoint, node name, or HMAC secret.

## Publish

Create `hihihieee/agent`, copy this directory to its root, and push a tag:

```bash
git tag v2.0.0
git push origin main v2.0.0
```

GitHub Actions publishes static `amd64` and `arm64` binaries plus checksums.

## Install from a release

The node does not need Go in the normal installation mode. The installer works
when the node has either `wget` or `curl`:

```bash
wget -qO- https://raw.githubusercontent.com/hihihieee/agent/main/install.sh | \
  bash -s -- install \
    --secret-file /root/palm-agent.secret \
    --endpoint https://your-control-plane.example/limiter/ingest \
    --node uk-onedash \
    --non-interactive
```

Using `--secret-file` avoids writing the secret to shell history. Create it on
the node with mode `0600`, install the agent, then remove that temporary file.
The installed secret remains in `/etc/palm-agent.env` with mode `0600`.

Re-running `install` replaces the current `palm-agent` installation. If the
old `palm-iplog.service` agent is present, the installer first imports any
missing settings from `/etc/palm-iplog.env`, then removes the old service,
binaries, parser, metrics helper, and legacy environment file before starting
the new service. The two agents never remain enabled in parallel.

## Build on the node

This mode downloads the official Go toolchain into a temporary directory,
automatically selects the current stable version, verifies its published
checksum, runs tests, builds the agent, and removes the toolchain when
installation exits:

```bash
wget -qO- https://raw.githubusercontent.com/hihihieee/agent/main/install.sh | \
  bash -s -- install \
    --build-from-source \
    --secret-file /root/palm-agent.secret \
    --endpoint https://your-control-plane.example/limiter/ingest \
    --node uk-onedash \
    --non-interactive
```

Go is deliberately not installed system-wide: the resulting static binary has
no Go runtime dependency. Set `PALM_GO_VERSION` only when a specific toolchain
version is required.

## Operations

```bash
palm-agent doctor
systemctl status palm-agent
journalctl -u palm-agent -f

wget -qO- https://raw.githubusercontent.com/hihihieee/agent/main/install.sh | \
  bash -s -- update

wget -qO- https://raw.githubusercontent.com/hihihieee/agent/main/install.sh | \
  bash -s -- uninstall --purge
```

## Security model

- A public repository is safe because it contains only code.
- The API rejects requests without the HMAC secret.
- The permanent secret exists only in the control plane environment and in
  `/etc/palm-agent.env` on installed nodes.
- Anyone with root access to a node can read its secret. Obfuscation cannot
  prevent this.
- Avoid passing the secret directly in a command: shell history, process lists,
  CI logs, or chat messages may retain it. Prefer `--secret-file` or the prompt.

The current server accepts a shared secret. This agent keeps that contract.
