# Troubleshooting Remote Docker

## Start with status and diagnostics

On the Mac:

```bash
remote-docker status
remote-docker sync status
remote-docker doctor
```

Use `remote-docker recover` only after reading the reported checks. Recovery reconnects managed infrastructure; it does not repeat a Docker command and does not delete images, containers, or volumes.

## The Mac still uses local Docker

Check the managed context:

```bash
docker context show
docker context inspect remote-docker
docker info
```

The context description must be `Managed by Remote Docker`, and its endpoint must use the managed `ssh://remote-docker-device-...` host. Remote Docker refuses `--host`, `-H`, and `--context` overrides because they could bypass the paired endpoint.

## `/usr/local/bin/docker` already exists

The macOS installer never overwrites an unrelated Docker command. Decide which installation should own that path, remove or relocate it yourself, and run the installer again. Uninstall removes the link only when it still points to the exact Remote Docker launcher and carries the package ownership marker.

## Windows is not discovered

- Confirm both computers use the same private network and are not isolated by a guest Wi-Fi policy.
- Set the Windows network profile to Private.
- Confirm the Windows tray application is running.
- Check that local firewall management is available and that security software is not blocking local discovery.
- Do not open Docker API ports.

## Pairing code or identity error

Never confirm different six-digit codes. If a previously paired SSH identity changes, stop and determine why. After a deliberate reinstall that changed the managed identity, unpair and pair again rather than editing known-host files manually.

## A bind path is rejected

Register the containing source directory:

```bash
remote-docker workspace add /absolute/path/to/project
```

Symlinks that escape a registered directory and sibling directories with a similar name remain blocked. Named volumes do not need workspace registration because they live entirely on Windows.

The first release accepts source workspaces only below `/Users` on the Mac. Move a project from an external volume or another filesystem root into the normal macOS user directory before registering it.

## Synchronization does not become ready

Run `remote-docker sync status` and check:

- the Windows PC is reachable;
- the workspace is registered;
- neither computer is out of disk space;
- the path is writable on both sides;
- there is no unresolved conflict copy or filesystem error.

Do not put databases, Docker named-volume contents, image layers, or build cache inside a synchronized workspace.

## A published port is unavailable

Remote Docker mirrors supported TCP ports to the same `localhost` port on the Mac. Stop the local process using that port or change the Docker publication. The client reports a conflict before it starts a new container and never terminates another process automatically.

UDP publications and host networking are not supported in the first release.

## Recovery after Wi-Fi loss or Windows restart

Existing containers continue on Windows while the Mac is disconnected. After the network returns, the agent restores the SSH connection, source synchronization, and local TCP relays. Containers with a Docker restart policy return after a Windows restart when the managed Engine starts.

If automatic recovery does not complete:

```bash
remote-docker doctor
remote-docker recover
```

Then inspect the Windows tray status and WSL availability. Do not unregister the managed distribution as a troubleshooting shortcut.

## Collecting useful issue information

Include the application version, macOS and Windows versions, the safe output of `remote-docker doctor`, and the exact failing Docker command. Remove project names and paths if they are private. Never attach private keys, credential-store exports, registry authentication, environment files, or raw Syncthing configuration.
