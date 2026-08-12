# Troubleshooting Remote Docker

## Nothing starts after login or reboot

That is expected. Remote Docker has no autostart on macOS or Windows.

1. Launch the application manually on both computers.
2. On Windows, choose **Start hosting**.
3. On the Mac, reconnect to the trusted Windows host or start a new search if it is not paired.

Before manual launch, Remote Docker should not leave visible consoles, its desktop process, or its managed WSL workload running.

## The installer is unknown or unsigned

Packages are intentionally unsigned. Before approval, verify the platform SHA-256 file and confirm that the manifest `source_commit` matches the release tag.

On macOS, use **System Settings → Privacy & Security → Open Anyway** only for the verified package. Never disable Gatekeeper globally.

On Windows, review SmartScreen only after verification. If Smart App Control blocks the installer without an approval choice, do not disable system protection globally.

## Windows Setup asks for a restart

Restart Windows, then manually open the same Setup EXE again. Setup rechecks prerequisites and continues visibly. It does not register a startup task, reopen itself silently, or leave command windows waiting after reboot.

If Setup fails, use its retry action and keep the displayed log. Do not unregister the managed WSL distribution as a troubleshooting shortcut because that can delete Docker data.

## The Windows PC is not discovered

- Verify that Remote Docker is open on Windows and says **Waiting for connection**.
- If it says **Paused**, choose **Start hosting**.
- Confirm both computers use the same private network and are not isolated by guest Wi-Fi.
- Set the Windows network profile to Private.
- Check that local firewall management is available and security software is not blocking private-network discovery.
- Do not open Docker API ports.

Search is explicit on the Mac. If it was stopped, choose **Search for a PC** again.

## Pairing shows no code or the codes differ

The six-digit code appears on both application screens after the Mac selects a Windows PC. It is only compared; it is not entered.

Approve on Windows only if both device names and codes match. Reject mismatched requests. If a pinned host identity changes after a deliberate reinstall, remove the trusted device through the UI and pair again instead of editing identity files manually.

## Only one device can connect

The first version intentionally supports one trusted Mac and one Windows host. Remove the existing trusted Mac explicitly before pairing another one.

## The window disappeared but Docker still works

Closing the window keeps Remote Docker available in the macOS menu bar or Windows tray. Open it from that icon.

To stop its work while keeping the app open, choose **Pause**. To exit fully and stop all owned background work, choose **Finish work**.

## The Mac still uses a local Docker engine

The Resources screen reports whether a known local Docker socket is active. Also inspect the managed context:

```bash
docker context show
docker context inspect remote-docker
docker info
```

The context description must be `Managed by Remote Docker; owner=<token>`, and its endpoint must use `ssh://remote-docker-device-...`. A legacy context without the ownership token is left unchanged and reported as a conflict instead of being claimed automatically.

Remote Docker serializes its own context operations across processes and checks the exact ownership token, endpoint, and description before and after each change. Docker CLI does not provide an atomic compare-and-swap operation for contexts, so a manual or third-party `docker context create`, `update`, or `rm` issued during the small interval between those checks can still race with the app. Avoid changing `remote-docker` manually while pairing or removing a device; an observed mismatch is handled safely by leaving the context unchanged.

## `/usr/local/bin/docker` already exists

The macOS package never overwrites an unrelated command. Decide which installation owns that path, resolve it yourself, and install again. Removal deletes the link only when it still points to the exact Remote Docker launcher and carries the package ownership marker.

## A bind path is rejected

Add the containing project directory in the Mac application's **Workspaces** screen or run:

```bash
remote-docker workspace add /absolute/path/to/project
```

The path is a Mac path. Symlinks escaping the workspace and sibling directories with similar names remain blocked. Named volumes need no registration because they live on Windows. The first release accepts workspaces only below `/Users`.

## Synchronization is not ready

Check the Workspaces screen or run `remote-docker sync status`. Confirm that:

- both apps are running and connected;
- the workspace is registered;
- neither computer is out of disk space;
- the source path is writable;
- no filesystem error or unresolved conflict copy is reported.

Do not place databases, named-volume contents, image layers, or build cache inside a synchronized workspace.

## The Mac reports that its local synchronization identity is damaged

Remote Docker validates its encrypted local Syncthing identity before starting connection-owned processes.

If this Mac has no saved or pending device state, the application clears only its unusable local identity and owner-scoped Syncthing credentials. Registered workspaces and source files remain unchanged. Start the Mac client and search again; the next safe bootstrap creates a new identity automatically.

If a trusted device or pending cleanup still exists, automatic rotation is intentionally blocked because changing the device identity on only one computer would break synchronization trust. The application shows `local_sync_identity_corrupt` and does not start partial background work. Do not delete Keychain entries, config files, Windows trust, or WSL data manually; first use the normal device recovery or removal flow on both computers.

Permission, timeout, and credential-store access errors are not treated as identity corruption and do not trigger deletion.

## A published port is unavailable

Remote Docker mirrors supported TCP ports to the same `localhost` port on the Mac. Stop the local process using that port or change the Docker publication. Remote Docker reports conflicts and does not terminate unrelated processes.

UDP publications and host networking are not supported in the first release.

## Recovery after Wi-Fi loss or Windows restart

If both applications keep running, the connection, synchronization, and local relays should recover after a temporary Wi-Fi interruption.

A Windows restart is different: the application does not autostart. Launch it manually and choose **Start hosting**. Containers with a Docker restart policy can return when the managed environment starts again.

If reconnection still fails:

```bash
remote-docker status
remote-docker doctor
remote-docker recover
```

Recovery reconnects owned infrastructure; it does not repeat a Docker command or delete images, containers, and volumes.

## Resource numbers are unavailable

Remote Docker reports only measurements it can attribute safely. Whole-machine WSL, unrelated virtual machines, and unrelated Docker processes are not presented as Remote Docker usage. An unavailable value includes a reason; use Windows Task Manager for a whole-PC view.

## Collecting issue information

Include the application version, macOS and Windows versions, the safe output of `remote-docker doctor`, Setup logs when applicable, and the exact failing Docker command. Remove private names and paths. Never attach private keys, credential-store exports, registry credentials, environment files, or raw synchronization configuration.
