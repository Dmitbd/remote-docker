# Remote Docker

Use a Windows PC as the Docker host while keeping your editor, terminal, source code, Git, and frontend tooling on a Mac.

> Status: early development. The project architecture is defined, but the first working release has not been published yet.

## Why

Docker Desktop can consume a significant amount of memory and CPU on a development Mac. Remote Docker moves containers, images, builds, databases, volumes, and build cache to a second Windows PC connected to the same local network.

The intended experience stays familiar:

```bash
SKIP_RATATOSKR=1 make up-real
docker ps
docker compose exec app alembic upgrade head
docker logs -f app
pnpm dev
```

`docker` and `docker compose` use the Windows machine. `make`, Git, `pnpm`, tests, the editor, and the frontend dev server continue to run on the Mac.

## How it works

Remote Docker consists of two paired applications:

- a lightweight macOS agent that provides the Docker CLI connection, source synchronization, and local port forwarding;
- a Windows agent that manages a dedicated WSL2 environment with Docker Engine.

```text
Mac                                      Windows PC
-----------------------------------      --------------------------------
Editor, Git, make, pnpm                   Windows agent
Docker CLI and Compose             <->   Managed WSL2 environment
Source synchronization                   Docker Engine and BuildKit
localhost port forwarding                Containers, images and volumes
```

The Docker API is not exposed as an unsecured port on Wi-Fi. Paired devices communicate through an authenticated encrypted channel.

## Intended capabilities

- Standard `docker` and `docker compose` commands without project-specific replacements.
- Commands invoked indirectly by Makefiles and shell scripts.
- Interactive TTY, `Ctrl+C`, logs, exec, attach, events, stats, copy, build, pull, and push.
- Two-way synchronization for source bind mounts, including files generated inside containers.
- Docker volumes, databases, image layers, and BuildKit cache stored only on Windows.
- Published container ports available through the same `localhost` ports on the Mac.
- Automatic reconnection after Wi-Fi interruptions or Windows restarts.
- WSL2 and Docker diagnostics without silently deleting container data.

## Initial target

The first end-to-end target is the Giga Cowork development stack: Midgard, Yggdrasil, Mimir, Heimdall, PostgreSQL, Redis, Restate, Keycloak, and dynamically created sandbox containers.

The project is designed as a general remote Docker tool rather than a launcher for one Cowork command.

## Scope of the first release

- macOS and Windows on the same trusted local network;
- Linux containers running in a managed WSL2 distribution;
- TCP port forwarding;
- command-line Docker compatibility and small status applications;
- one Windows Docker host paired with one Mac.

Kubernetes, internet access, multi-host clustering, and a full Docker Desktop-style container dashboard are outside the first release.

## Project state

Implementation will begin with a minimal connection and Docker compatibility prototype, followed by bind-mount synchronization, port forwarding, recovery, and validation against the real Cowork stack.
