# Install Remote Docker

Remote Docker uses a Mac for source code and normal development tools, and a Windows 11 PC on the same private network for Docker Engine, images, containers, volumes, and build cache.

The first public release is not available yet. These instructions describe the intended signed-package flow and are also the acceptance checklist for release candidates.

## Requirements

- A Mac supported by the published macOS package.
- A Windows 11 x64 PC with hardware virtualization enabled.
- Administrator access for the initial Windows setup only.
- Both computers connected to the same trusted private Wi-Fi or Ethernet network.
- Enough Windows disk space for WSL2, images, build cache, and project data.

Docker Desktop is not required. Do not expose Docker TCP ports 2375 or 2376 on either computer.

## 1. Install and prepare Windows

1. Download the signed Windows MSI and its checksum from the same GitHub Release.
2. Verify the checksum and the Authenticode signature before opening it.
3. Install the MSI for the computer.
4. Open an elevated PowerShell window and run the packaged provisioning helper with explicit confirmation:

   ```powershell
   & 'C:\Program Files\Remote Docker\tools\provision.ps1' -ConfirmProvisioning
   ```

5. Restart Windows if the helper reports that WSL features were enabled, then run the helper again.
6. Confirm that the Remote Docker tray application reports that the managed environment is ready for pairing.

The helper creates a dedicated WSL2 distribution. It does not use Docker Desktop and does not alter unrelated WSL distributions.

## 2. Install the Mac client

1. Download the signed and notarized macOS package and its checksum from the same release.
2. Verify the checksum and installer signature.
3. Run the package. If `/usr/local/bin/docker` already belongs to another installation, Remote Docker stops without replacing it. Resolve that conflict explicitly and run the installer again.
4. Confirm that the Remote Docker menu-bar application is running.

## 3. Pair the computers

Keep both screens visible and use the menu-bar and tray pairing flow. Select the Windows PC discovered on the private network, compare the six-digit code on both computers, and confirm only when the codes match.

The command-line equivalent on the Mac is:

```bash
remote-docker pair candidates --json
remote-docker pair start DEVICE_ID --json
remote-docker pair confirm SESSION_ID SIX_DIGIT_CODE
remote-docker status
```

Pairing pins the SSH host identity. A changed identity is treated as an error and requires explicit unpairing and pairing again.

## 4. Register source directories

Register only directories that containers are allowed to use as bind mounts:

```bash
remote-docker workspace add "$HOME/Projects/example"
remote-docker workspace list
remote-docker sync status
```

Paths outside registered directories are rejected before Docker starts. Generated and version-control directories such as `.git`, `node_modules`, build output, and editor metadata are excluded by the managed synchronization policy. Environment files are not excluded automatically.

In the first release, a registered source directory must resolve inside `/Users` on the Mac. External volumes and source trees outside the normal macOS user directory are rejected rather than mapped implicitly.

## 5. Use Docker normally

Use standard commands from the Mac terminal:

```bash
docker info
docker compose up -d
docker compose logs -f
docker compose exec app sh
docker compose down
```

The shell, editor, Git, language tools, and tests still run on the Mac. Only Docker and Docker Compose use the Windows host.

## Safe removal

Normal Windows uninstall removes application files and owned firewall/startup entries but preserves the managed WSL distribution and Docker data. Permanent data deletion is a separate elevated operation with an exact confirmation phrase:

```powershell
& 'C:\Program Files\Remote Docker\tools\uninstall.ps1' `
  -DeleteData `
  -DataRemovalConfirmation DELETE-REMOTE-DOCKER-DATA
```

Run permanent deletion only before uninstalling the application files, and only after backing up any required volumes.
