# Install Remote Docker

Remote Docker uses a Mac as the client and a Windows 11 PC on the same private network as the Docker host. Packages are intentionally unsigned so the project can remain free to build and distribute.

## Requirements

- Windows 11 x64 with hardware virtualization enabled.
- A supported Mac and macOS package.
- Administrator access during installation.
- The same private Wi-Fi or Ethernet network on both computers.
- Enough Windows disk space for WSL2, images, volumes, cache, and synchronized workspaces.

Docker Desktop is not required. Do not expose Docker ports 2375 or 2376.

## 1. Verify and install Windows

Download these files from the same GitHub Release:

- `Remote-Docker-VERSION-x64-Setup.exe`;
- `Remote-Docker-Windows-x64-SHA256SUMS`;
- `Remote-Docker-Windows-x64-manifest.json`.

Verify the installer in PowerShell:

```powershell
Get-FileHash .\Remote-Docker-*-x64-Setup.exe -Algorithm SHA256
Get-Content .\Remote-Docker-Windows-x64-SHA256SUMS
(Get-Content .\Remote-Docker-Windows-x64-manifest.json | ConvertFrom-Json).source_commit
```

The hash must match the checksum file and the manifest commit must match the release tag on GitHub.

1. Open the Setup EXE. SmartScreen may warn because the file has no paid Authenticode signature. Continue only after verifying its hash and source commit.
2. Use the visible wizard to review prerequisites and choose the application folder and managed-data folder.
3. Optionally create a desktop shortcut. A Start Menu shortcut is always created.
4. Watch the preparation progress. Setup reports success or a specific failure and keeps a log for retry.
5. If Windows features were enabled, restart Windows when requested and run the same Setup EXE again. Setup does not resume or launch silently after reboot.
6. On the final page, launch Remote Docker or close Setup and use the Start Menu shortcut later.

Remote Docker is not added to Windows startup. The application and managed WSL environment are stopped when Setup finishes. Starting the application later is always an explicit user action.

If Smart App Control blocks unsigned software without an approval option, stop. Do not disable Windows security globally; use a machine policy that permits reviewed unsigned software or build the audited source yourself.

## 2. Verify and install macOS

Download the macOS package, checksum file, and manifest from the same release. Then verify them in Terminal:

```bash
shasum -a 256 -c Remote-Docker-macOS-arm64-SHA256SUMS
/usr/bin/ruby -rjson -e 'puts JSON.parse(File.read("Remote-Docker-macOS-arm64-manifest.json")).fetch("source_commit")'
```

The manifest commit must match the release tag.

1. Open the package. If macOS blocks the unsigned package, open **System Settings → Privacy & Security**, review the message, and choose **Open Anyway** only after verification. Do not disable Gatekeeper globally.
2. Complete installation. Remote Docker is placed in `/Applications` but is not launched automatically.
3. If `/usr/local/bin/docker` belongs to another installation, Setup stops without overwriting it. Resolve ownership explicitly and run the package again.
4. Open **Remote Docker** from Applications when you want to use it.

## 3. Pair the computers

No code is typed manually.

1. Launch Remote Docker manually on both computers. Each application starts in **Paused** state.
2. On Windows, choose **Start hosting**. The screen changes to **Waiting for connection** and shows the Windows device name.
3. On the Mac, choose **Search for a PC**.
4. Select the expected Windows computer from the discovered devices and choose **Connect**.
5. Compare the Mac name, Windows name, and six-digit code on both screens.
6. If all values match, choose **Approve** on Windows. Otherwise choose **Reject**.
7. Both screens must change to **Connected** and show `Mac client → Windows Docker host`.

Only one trusted Mac is supported. A second computer cannot connect until the existing trusted device is removed explicitly.

## 4. Add source workspaces

In the Mac application, open **Workspaces**, choose **Add workspace**, and select the project directory. The UI action is equivalent to running this from that directory:

```bash
remote-docker workspace add "$PWD"
```

`$PWD` means the current Mac directory. It does not refer to a Windows folder. Only explicitly added directories below `/Users` may be used as source bind mounts in the first release.

## 5. Use Docker normally

From the Mac terminal:

```bash
docker info
docker compose up -d
docker compose logs -f
docker compose exec app sh
docker compose down
```

The editor, shell, Git, language tools, and ordinary tests still run on the Mac. Docker Engine and its data run on Windows.

## Pause, disconnect, close, and finish

- **Pause** stops the active client or host role and its owned synchronization, relays, and managed WSL work, but leaves the application open.
- **Disconnect** ends the current connection. Windows returns to **Waiting for connection** until it is paused.
- Closing the window keeps the application available from its menu-bar or tray icon.
- **Finish work** exits the application and stops all Remote Docker-owned background work.

After a Windows reboot, Remote Docker remains stopped. Launch it manually and choose **Start hosting** again.

## Safe removal

Normal Windows uninstall removes the application, shortcuts, and owned firewall rules while preserving the managed WSL distribution and Docker data.

Permanent data removal is a separate explicit option and requires typing the exact confirmation phrase. Back up required volumes first. The removal helper validates the selected application/data roots and refuses to delete unrelated paths or an unowned WSL distribution.
