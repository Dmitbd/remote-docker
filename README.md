# Remote Docker

Run Docker on a Windows PC while keeping your development workflow on a Mac.

> Status: pre-release development. Signed packages and real Mac-to-Windows acceptance are required before the first public release.

## Why

Docker Desktop can consume a significant amount of memory and CPU on a development Mac. Remote Docker moves containers, image builds, databases, volumes, and build cache to a second Windows PC connected to the same local network.

Your editor, terminal, source code, Git, tests, and local development tools stay on the Mac. Standard Docker commands use the Windows machine:

```bash
docker compose up -d
docker ps
docker compose exec api ./migrate
docker logs -f api
docker compose down
```

## How it works

Remote Docker consists of two paired applications:

- a lightweight macOS agent that provides Docker CLI connectivity, source synchronization, and local port forwarding;
- a Windows agent that manages a dedicated WSL2 environment with Docker Engine.

```text
Mac                                      Windows PC
-----------------------------------      --------------------------------
Editor, terminal, Git                    Windows agent
Docker CLI and Compose             <->   Managed WSL2 environment
Source synchronization                   Docker Engine and BuildKit
localhost port forwarding                Containers, images and volumes
```

Only Docker operations are remote. Other commands continue to run normally on the Mac.

## Setup overview

1. Install the Windows agent.
2. Let it create and validate a dedicated WSL2 environment with Docker Engine.
3. Install the macOS agent and Docker CLI integration.
4. Pair both computers with a one-time code while they are on the same network.
5. Select the local project directories that may be used as bind mounts.
6. Continue using standard `docker` and `docker compose` commands.

Docker Desktop is not required on either computer after the remote environment is ready.

Detailed release-candidate instructions are in [Install Remote Docker](docs/INSTALL.md). Recovery and safety guidance is in [Troubleshooting Remote Docker](docs/TROUBLESHOOTING.md).

## Data model

Project directories used as bind mounts are synchronized between the Mac and the managed WSL2 environment. This allows source changes to reach containers and files generated inside containers to return to the Mac.

The first release supports registered source directories below the normal macOS `/Users` root. External-volume bind mounts are rejected explicitly.

Heavy Docker data remains on Windows:

- images and container layers;
- named volumes and databases;
- BuildKit cache;
- container writable data.

Published TCP ports are forwarded back to the same `localhost` ports on the Mac, so browsers, API clients, database tools, and local frontend servers can use familiar addresses.

## Intended capabilities

- Standard `docker` and `docker compose` syntax.
- Commands invoked indirectly by Makefiles and shell scripts.
- Interactive TTY, `Ctrl+C`, logs, exec, attach, events, stats, copy, build, pull, and push.
- Two-way synchronization for source bind mounts.
- Automatic localhost port forwarding.
- Automatic reconnection after Wi-Fi interruptions or Windows restarts.
- WSL2 and Docker diagnostics without silently deleting container data.
- Clear errors for unavailable hosts, conflicting ports, and unsupported bind paths.

## Security

The Docker API is never exposed as an unsecured port on Wi-Fi. Paired devices communicate through an authenticated encrypted channel, and access can be revoked by removing the pairing. Private WSL service identities are encrypted at rest with a key protected by Windows Credential Manager and are materialized only in the Linux runtime filesystem while services load them.

The initial version is intended for two trusted computers on the same private local network.

## Scope of the first release

- macOS client and Windows host;
- Linux containers in a managed WSL2 distribution;
- one paired Windows Docker host;
- TCP port forwarding;
- command-line Docker compatibility;
- small status applications for connection, synchronization, and diagnostics.

Kubernetes, internet access, multi-host clustering, UDP forwarding, and a full Docker Desktop-style dashboard are outside the first release.

## Release gate

The repository contains the client, agents, managed WSL environment, package definitions, and focused automated tests. A release is considered ready only after Windows CI, signing and notarization, clean-machine installation, Docker and Compose compatibility, two-way source synchronization, reconnect behavior, LAN security checks, and performance measurements all pass on a real Mac and Windows pair.

Until that gate is complete, build artifacts are development previews rather than a supported release.
