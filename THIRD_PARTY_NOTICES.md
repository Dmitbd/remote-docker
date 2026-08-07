# Third-Party Notices

Remote Docker packages the following upstream software. The version pins and
artifact checksums used by the package build are recorded in
`packaging/versions.json` and `packaging/checksums.txt`.

## Packaged command-line tools

| Component | License | Source and license |
| --- | --- | --- |
| Docker CLI | Apache License 2.0 | <https://github.com/docker/cli> |
| Docker Compose | Apache License 2.0 | <https://github.com/docker/compose> |
| Syncthing | Mozilla Public License 2.0 | <https://github.com/syncthing/syncthing> |

The corresponding source for the packaged Syncthing release is available from
its official release page at
<https://github.com/syncthing/syncthing/releases/tag/v2.1.1>.

## Go toolchain and linked Go modules

The Go toolchain and standard library are distributed under the Go project's
BSD-style license: <https://go.dev/LICENSE>.

Remote Docker binaries also include code from these modules:

| Module | License | Source and license |
| --- | --- | --- |
| `fyne.io/systray` | MIT | <https://github.com/fyne-io/systray> |
| `github.com/Microsoft/go-winio` | MIT | <https://github.com/microsoft/go-winio> |
| `github.com/grandcat/zeroconf` | MIT | <https://github.com/grandcat/zeroconf> |
| `github.com/zalando/go-keyring` | MIT | <https://github.com/zalando/go-keyring> |
| `github.com/cenkalti/backoff` | MIT | <https://github.com/cenkalti/backoff> |
| `github.com/danieljoos/wincred` | MIT | <https://github.com/danieljoos/wincred> |
| `github.com/godbus/dbus/v5` | BSD-2-Clause | <https://github.com/godbus/dbus> |
| `github.com/miekg/dns` | BSD-3-Clause | <https://github.com/miekg/dns> |
| `golang.org/x/crypto` | BSD-3-Clause | <https://cs.opensource.google/go/x/crypto> |
| `golang.org/x/net` | BSD-3-Clause | <https://cs.opensource.google/go/x/net> |
| `golang.org/x/sys` | BSD-3-Clause | <https://cs.opensource.google/go/x/sys> |

Copyright notices and complete license texts remain available in the linked
upstream source distributions. This notice does not alter any upstream license.
