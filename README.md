# Remote Docker

Remote Docker lets a Mac use Docker Engine on a Windows PC in the same private network. Containers, images, volumes, databases, build cache, CPU load, and most Docker memory usage stay on Windows. The editor, terminal, source code, Git, and ordinary development tools stay on the Mac.

> Status: pre-release development. Packages are intentionally unsigned and must be verified before installation.

## What it feels like

After the computers are paired, Docker commands on the Mac keep their normal syntax:

```bash
docker compose up -d
docker ps
docker compose exec app sh
docker logs -f app
docker compose down
```

Only Docker work is redirected. Shell commands, editors, tests, Git operations, and source files remain local to the Mac.

## How it works

```text
Mac client                              Windows host
-----------------------------------     -----------------------------------
Editor, terminal and source files       Remote Docker desktop application
Docker CLI and Compose             <->  Dedicated managed WSL2 environment
Selected workspace synchronization      Docker Engine, images and volumes
localhost port access                   Containers and build cache
```

The Mac application has the fixed **Client** role. The Windows application has the fixed **Docker host** role. The first version supports one trusted Mac-to-Windows pair.

Source directories selected as workspaces are synchronized into the Linux filesystem inside the managed WSL2 environment. Heavy Docker data is never synchronized back to the Mac. Published TCP ports are relayed to the same `localhost` ports on the Mac.

## Normal setup flow

1. Install Remote Docker on Windows with the visible Setup wizard. Choose the application and data locations and let Setup prepare WSL2.
2. Install Remote Docker on the Mac.
3. Start both applications manually. Both initially show **Paused** and do not start Docker work by themselves.
4. On Windows, choose **Start hosting**. On the Mac, choose **Search for a PC**.
5. Select the expected Windows PC, compare the device names and six-digit code shown on both screens, then approve the request on Windows.
6. Add the Mac source directories that Docker may use as bind mounts.
7. Run normal `docker` and `docker compose` commands in the Mac terminal.

There is no application autostart on either computer. After a Windows reboot, launch Remote Docker manually and start hosting again. Wi-Fi interruptions can reconnect automatically while both applications remain running.

Detailed steps are in [Installation](docs/INSTALL.md). Recovery guidance is in [Troubleshooting](docs/TROUBLESHOOTING.md).

## Application states

- **Paused**: the application is open, but discovery, hosting, synchronization, relays, and managed Docker work are stopped.
- **Searching / Waiting for connection**: the selected role is active and the other computer can find or connect to it.
- **Pairing**: both screens show the participants and the same six-digit comparison code; Windows can approve or reject.
- **Connected**: both applications show the peer, roles, connection state, and synchronized workspaces.
- **Disconnected**: both sides explain that the link ended and which side initiated it when known.

Closing the window keeps the application available from the menu-bar or tray icon. **Finish work** exits the application and stops all background work owned by Remote Docker.

## Workspaces and data

Only explicitly added Mac directories may be used as source bind mounts. The first release accepts source workspaces below `/Users` and rejects paths that escape a registered workspace. External-volume workspaces are not mapped implicitly.

Data placement is deliberate:

- source files: Mac plus a synchronized WSL copy;
- containers, images, named volumes, databases, and build cache: Windows only;
- application settings and managed WSL data: the locations selected during Windows Setup.

## Resource visibility

The Resources screen labels responsibility instead of presenting a combined machine total:

- **Mac sends source files and commands**: Remote Docker application CPU/RAM and whether a local Docker engine is running;
- **Windows runs Docker workloads**: Remote Docker application CPU/RAM, managed environment state, and container count.

Metrics that cannot be attributed safely to Remote Docker are shown as unavailable with a reason. The application does not substitute whole-machine or unrelated-process usage.

## Security

The Docker API is not exposed as an unsecured Wi-Fi port. Pairing uses an authenticated encrypted channel, shows the same short comparison code on both devices, and pins the host identity. Access can be revoked by disconnecting and removing the trusted device.

The intended environment is two trusted computers on one private Wi-Fi or Ethernet network. Public networks, internet exposure, multi-host clustering, Kubernetes, UDP relays, and host networking are outside the first release.

## Free unsigned releases

The project is designed to remain buildable and distributable without paid Apple or Windows signing certificates. Release packages are therefore unsigned. Each release must include SHA-256 checksums, a manifest that records the source commit, and an SBOM.

Verify those files before approving an operating-system warning. Never disable Gatekeeper, SmartScreen, antivirus, Smart App Control, or another system security feature globally for Remote Docker.

## Release gate

A release is ready only after focused CI, clean installation, pairing approval, Docker and Compose compatibility, source synchronization, lifecycle cleanup, reconnect behavior, LAN security checks, and resource attribution have passed on a real Mac and Windows pair. Until then, artifacts are development previews.
