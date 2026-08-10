BeforeAll {
    $ErrorActionPreference = 'Stop'

    $repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
    $windowsPackaging = Join-Path $repoRoot 'packaging\windows'
    $installerSource = Join-Path $windowsPackaging 'installer\RemoteDocker.nsi'
    $pagesSource = Join-Path $windowsPackaging 'installer\remote-docker-pages.nsh'
    $stringsSource = Join-Path $windowsPackaging 'installer\strings.nsh'
    $buildScript = Join-Path $windowsPackaging 'build-installer.ps1'
    $updateScript = Join-Path $windowsPackaging 'install-agent.ps1'
    $pathValidationScript = Join-Path $windowsPackaging 'scripts\path-validation.ps1'
    $provisionScript = Join-Path $windowsPackaging 'scripts\provision.ps1'
    $statusScript = Join-Path $windowsPackaging 'scripts\provision-status.ps1'
    $uninstallScript = Join-Path $windowsPackaging 'scripts\uninstall.ps1'
    $testScript = Join-Path $PSScriptRoot 'windows_package.Tests.ps1'

    function Read-RepositoryFile {
        param([Parameter(Mandatory = $true)][string]$Path)

        (Get-Content -LiteralPath $Path -Raw).Replace("`r`n", "`n")
    }
}

Describe 'Windows Setup EXE contract' {
    It 'contains one NSIS wizard and no legacy MSI sources' {
        @(
            $installerSource,
            $pagesSource,
            $stringsSource,
            $buildScript,
            $updateScript,
            $pathValidationScript,
            $provisionScript,
            $statusScript,
            $uninstallScript
        ) | ForEach-Object { $_ | Should -Exist }

        (Join-Path $windowsPackaging 'RemoteDocker.wxs') | Should -Not -Exist
        (Join-Path $windowsPackaging 'build-msi.ps1') | Should -Not -Exist
        (Join-Path $windowsPackaging 'nuget.config') | Should -Not -Exist
        (Join-Path $windowsPackaging 'installer\pages.nsh') | Should -Not -Exist
    }

    It 'shows one explicit manual installation-location flow' {
        $installer = Read-RepositoryFile $installerSource
        $pages = Read-RepositoryFile $pagesSource
        $strings = Read-RepositoryFile $stringsSource
        $languageLoad = $installer.IndexOf('!insertmacro MUI_LANGUAGE "Russian"')
        $stringsLoad = $installer.IndexOf('!include "strings.nsh"')
        $versionInfo = $installer.IndexOf('VIAddVersionKey /LANG=${LANG_RUSSIAN}')

        $installer | Should -Match 'MUI_PAGE_WELCOME'
        $installer | Should -Match 'Page custom InstallLocationPageCreate InstallLocationPageLeave'
        $installer | Should -Not -Match 'PreflightPageCreate'
        $installer | Should -Not -Match 'MUI_PAGE_COMPONENTS'
        $installer | Should -Not -Match 'MUI_PAGE_DIRECTORY'
        $installer | Should -Not -Match 'DataPageCreate|DataPageLeave'
        $installer | Should -Match 'MUI_PAGE_INSTFILES'
        $installer | Should -Match 'MUI_FINISHPAGE_RUN_NOTCHECKED'
        $languageLoad | Should -BeGreaterThan -1
        $stringsLoad | Should -BeGreaterThan $languageLoad
        $versionInfo | Should -BeGreaterThan $languageLoad
        $installer | Should -Match 'VIAddVersionKey /LANG=\$\{LANG_RUSSIAN\} "LegalCopyright"'
        $installer | Should -Not -Match '\$PROGRAMDATA'
        $installer | Should -Match 'StrCpy \$DataDirectory "\$APPDATA\\RemoteDocker"'
        $installer | Should -Match 'CreateShortCut "\$SMPROGRAMS\\Remote Docker\\Remote Docker\.lnk"'
        $installer | Should -Not -Match 'Section /o .*DesktopShortcut'
        $installer | Should -Match 'CreateShortCut "\$DESKTOP\\Remote Docker\.lnk"'
        $installer | Should -Match 'RemoteDocker\.exe'
        $installer | Should -Match 'remote-docker\.ico'
        $pages | Should -Match 'BaseDirectoryInput'
        $pages | Should -Match 'DesktopShortcutCheckbox'
        $pages | Should -Match 'StrCpy \$INSTDIR "\$BaseDirectory\\App"'
        $pages | Should -Match 'StrCpy \$DataDirectory "\$BaseDirectory\\Data"'
        $strings | Should -Match 'InstallLocationTitle'
        $strings | Should -Match 'CreateDesktopShortcut'
        $strings | Should -Match 'InstallingFiles'
        $strings | Should -Match 'ProvisioningWSL'
        $strings | Should -Match 'ConfiguringDocker'
        $strings | Should -Match 'ConfiguringFirewall'
        $strings | Should -Match 'InstallRetry'
        $strings | Should -Match 'InstallLogPath'
    }

    It 'reuses registered roots without automatically migrating existing data' {
        $installer = Read-RepositoryFile $installerSource
        $pages = Read-RepositoryFile $pagesSource
        $combined = "$installer`n$pages"

        $installer | Should -Match 'ReadRegStr \$ExistingApplicationRoot HKLM "Software\\Remote Docker" "InstallDirectory"'
        $installer | Should -Match 'ReadRegStr \$ExistingDataRoot HKLM "Software\\Remote Docker" "DataDirectory"'
        $installer | Should -Match 'StrCpy \$INSTDIR \$ExistingApplicationRoot'
        $installer | Should -Match 'StrCpy \$DataDirectory \$ExistingDataRoot'
        $pages | Should -Match '\$ExistingInstall == "1"'
        $combined | Should -Not -Match 'Move-Item|Copy-Item|Rename-Item'
    }

    It 'never registers the application for automatic startup' {
        $publicInstallerFiles = @($installerSource, $pagesSource, $stringsSource)
        $combined = ($publicInstallerFiles | ForEach-Object { Read-RepositoryFile $_ }) -join "`n"

        $combined | Should -Not -Match 'CurrentVersion\\Run(?:Once)?'
        $combined | Should -Not -Match 'Startup\\|StartupFolder'
        $combined | Should -Not -Match 'CreateService|ServiceInstall|Register-ScheduledTask|New-ScheduledTask|schtasks'
        $combined | Should -Not -Match 'RemoteDockerAgent|RemoteDockerTray'
        $combined | Should -Not -Match 'автозапуск|при входе в Windows'
    }

    It 'pins and verifies NSIS and builds the stable Wails child' {
        $script = Read-RepositoryFile $buildScript

        $script | Should -Match '\$GoVersion\s*=\s*''1\.26\.5'''
        $script | Should -Match '\$NsisVersion\s*=\s*''3\.12'''
        $script | Should -Match '\$NsisPackageSha256\s*=\s*''[a-f0-9]{64}'''
        $script | Should -Match '\$NsisSha256\s*=\s*''[a-f0-9]{64}'''
        $script | Should -Match 'community\.chocolatey\.org/api/v2/package/nsis\.install/\$NsisVersion\.0'
        $script | Should -Match 'tools\\nsis-\$NsisVersion-setup\.exe'
        $script | Should -Match 'Packaged NSIS SHA-256 mismatch'
        $script | Should -Match '\$attempt\s*-le\s*3'
        $script | Should -Match '\$LlvmMingwVersion\s*=\s*''20260616'''
        $script | Should -Match '\$LlvmMingwSha256\s*=\s*''[a-f0-9]{64}'''
        $script | Should -Match 'Get-FileHash'
        $script | Should -Match 'CGO_ENABLED\s*=\s*''1'''
        $script | Should -Match 'x86_64-w64-mingw32-gcc\.exe'
        $script | Should -Match '-H=windowsgui'
        $script | Should -Match 'cmd/remote-docker-desktop'
        $script | Should -Match 'cmd/remote-docker-ui'
        $script | Should -Match 'remote-docker-ui\.exe'
        $script | Should -Match 'wv2runtime\.error'
        $script | Should -Match 'Copy-Item -LiteralPath \$desktopResourceObject -Destination \$uiResourceObject -Force'
        $script | Should -Match 'cmd/remote-docker-remote'
        $script | Should -Match 'Remote-Docker-\$Version-x64-Setup\.exe'
        $script | Should -Match "'/INPUTCHARSET'"
        $script | Should -Match "'UTF8'"
        $script | Should -Match "'/WX'"
        $script | Should -Not -Match 'cmd/remote-docker-agent|cmd/remote-docker-tray|\.msi'
    }

    It 'requires WebView2 explicitly without a silent application-start download' {
        $installer = Read-RepositoryFile $installerSource

        $installer | Should -Match 'EdgeUpdate\\Clients\\\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5\}'
        $installer | Should -Match 'Microsoft Edge WebView2 Runtime'
        $installer | Should -Match 'SetRegView 32'
        $installer | Should -Match 'https://go\.microsoft\.com/fwlink/p/\?LinkId=2124703'
        $installer | Should -Match 'MessageBox MB_YESNO'
        $installer | Should -Not -Match 'Invoke-WebRequest|URLDownloadToFile|nsExec::ExecToStack[^\n]*WebView2'
    }

    It 'installs and removes the host and UI child together' {
        $installer = Read-RepositoryFile $installerSource
        $build = Read-RepositoryFile $buildScript

        $build | Should -Match '/DUI_SOURCE=\$uiSource'
        $installer | Should -Match 'File /oname=RemoteDocker\.exe "\$\{APP_SOURCE\}"'
        $installer | Should -Match 'File /oname=remote-docker-ui\.exe "\$\{UI_SOURCE\}"'
        $installer | Should -Match 'Delete "\$INSTDIR\\RemoteDocker\.exe"'
        $installer | Should -Match 'Delete "\$INSTDIR\\remote-docker-ui\.exe"'
    }

    It 'provisions validated application and data roots without starting the app' {
        $script = Read-RepositoryFile $provisionScript
        $rootCheck = $script.IndexOf('Assert-ManagedDirectory -Path $DataRoot')
        $import = $script.IndexOf("'--import'")

        $script | Should -Match '\[string\]\$ApplicationRoot'
        $script | Should -Match '\[string\]\$DataRoot'
        $script | Should -Match '\[string\]\$ProgressPath'
        $script | Should -Match '\[string\]\$LogPath'
        $script | Should -Match '\[int\]\$PairingPort\s*=\s*49221'
        $script | Should -Match 'Join-Path \$PSScriptRoot ''path-validation\.ps1'''
        $script | Should -Match 'FileAttributes\]::ReparsePoint'
        $script | Should -Match 'Join-Path \$ApplicationRoot ''RemoteDocker\.exe'''
        $script | Should -Match 'Join-Path \$DataRoot ''wsl'''
        $script | Should -Match 'Write-RemoteDockerProvisionStatus'
        $script | Should -Match '--prepare-wsl'
        $script | Should -Match '(?s)Start-Process\s+`?\s*-FilePath\s+\$desktopExecutable\s+`?\s*-ArgumentList\s+@\(''--prepare-wsl''\)\s+`?\s*-Wait\s+`?\s*-PassThru'
        $script | Should -Match '\$identityPreparation\.ExitCode'
        $script | Should -Match '''/usr/local/bin/remote-docker-remote'',\s*''runtime-status'''
        $script | Should -Match '-Profile\s+Private'
        $script | Should -Match '-Program\s+\$desktopExecutable'
        $script | Should -Match '-RemoteAddress\s+LocalSubnet'
        $script | Should -Match 'Name = ''RemoteDocker\.Managed\.Tunnel\.TCP''; DisplayName = ''Remote Docker Managed Tunnel''; Protocol = ''TCP'''
        $script | Should -Match 'Name = ''RemoteDocker\.Managed\.Discovery\.UDP''; DisplayName = ''Remote Docker Managed Discovery''; Protocol = ''UDP'''
        $script | Should -Match '-LocalPort\s+\$PairingPort'
        $script | Should -Match '\$legacyRule\.Group\s+-eq\s+\$firewallRuleGroup'
        $script | Should -Not -Match 'Port = \$SshBridgePort|Port = \$SyncthingBridgePort'
        $script | Should -Match "(?s)Invoke-External.*'wsl\.exe'.*'--terminate'"
        $rootCheck | Should -BeGreaterThan -1
        $import | Should -BeGreaterThan $rootCheck
        $script | Should -Not -Match 'Start-Process[^\n]+RemoteDocker|Get-Process\s+-Name\s+''RemoteDocker'
        $script | Should -Not -Match 'RemoteDockerAgent|RemoteDockerTray|ScheduledTask|CurrentVersion\\Run'
    }

    It 'uses one Windows PowerShell 5.1-compatible path validator' {
        $helper = Read-RepositoryFile $pathValidationScript
        $scripts = @($provisionScript, $uninstallScript, $updateScript)
        $combined = ($scripts | ForEach-Object { Read-RepositoryFile $_ }) -join "`n"

        $helper | Should -Match 'function Test-RemoteDockerFullyQualifiedPath'
        $helper | Should -Match 'function Assert-RemoteDockerCanonicalPath'
        $helper | Should -Match '\[System\.IO\.Path\]::GetPathRoot\(\$Path\)'
        $helper | Should -Match '\[System\.IO\.Path\]::GetFullPath\(\$Path\)'
        $helper | Should -Match '\$root -eq'
        $helper | Should -Match '\^\[A-Za-z\]:\$'
        $helper | Should -Not -Match 'IsPathFullyQualified'
        $combined | Should -Not -Match 'IsPathFullyQualified'
        foreach ($scriptPath in $scripts) {
            (Read-RepositoryFile $scriptPath) | Should -Match 'Join-Path \$PSScriptRoot ''path-validation\.ps1'''
        }
        (Read-RepositoryFile $installerSource) | Should -Match 'File /oname=path-validation\.ps1'
    }

    It 'uses hidden installer execution and finite reboot state' {
        $installer = Read-RepositoryFile $installerSource
        $status = Read-RepositoryFile $statusScript

        $installer | Should -Match 'nsExec::ExecToStack'
        $installer | Should -Match '-WindowStyle Hidden'
        $installer | Should -Match 'installer-reboot\.pending'
        $installer | Should -Match 'Delete .*installer-reboot\.pending'
        $installer | Should -Match 'SetRebootFlag true'
        $installer | Should -Match '(?s)\$ProvisionExit == 3010.*SetErrorLevel 3010.*Quit.*CreatingShortcuts'
        $installer | Should -Not -Match 'Exec(?:Wait)?\s+''?"?\$INSTDIR\\RemoteDocker\.exe'
        $status | Should -Match 'ConvertTo-Json'
        $status | Should -Match 'Add-Content\s+-LiteralPath\s+\$ProgressPath'
    }

    It 'creates a real log and exits cleanly when provisioning is cancelled' {
        $installer = Read-RepositoryFile $installerSource
        $script = Read-RepositoryFile $provisionScript

        $logCreate = $installer.IndexOf('FileOpen $0 "$LogPath" w')
        $provision = $installer.IndexOf('nsExec::ExecToStack /TIMEOUT=3600000')
        $register = $installer.IndexOf('WriteRegStr HKLM "Software\Remote Docker"')

        $logCreate | Should -BeGreaterThan -1
        $provision | Should -BeGreaterThan $logCreate
        $register | Should -BeGreaterThan $provision
        $installer | Should -Match '\$ProvisionOutput'
        $installer | Should -Match '(?s)provision_failed:.*SetErrorLevel 1.*Quit'
        $installer | Should -Not -Match 'Abort "\$\(InstallFailed\)'
        $script | Should -Match '\$logReady = \$false'
        $script | Should -Match '\$progressReady = \$false'
        $script | Should -Match '\[Console\]::Error\.WriteLine\(\$reason\)'
        $script | Should -Match '(?s)try \{.*if \(-not \$ConfirmProvisioning\).*Assert-ManagedDirectory -Path \$ApplicationRoot.*catch \{'
        $script | Should -Not -Match 'throw \$reason'
    }

    It 'preserves managed WSL data by default and deletes only with exact confirmation' {
        $script = Read-RepositoryFile $uninstallScript
        $normalExit = $script.IndexOf('if (-not $DeleteData)')
        $unregister = $script.IndexOf('--unregister')
        $treeCheck = $script.IndexOf('Assert-NoReparseTree -Path $distroRoot')
        $recursiveDelete = $script.IndexOf('Remove-Item -LiteralPath $distroRoot -Recurse -Force')

        $script | Should -Match '\[string\]\$ApplicationRoot'
        $script | Should -Match '\[string\]\$DataRoot'
        $script | Should -Match 'RemoteDocker\.exe'
        $script | Should -Match '--shutdown'
        $script | Should -Match '--delete-wsl-credential'
        $script | Should -Match '''RemoteDocker\.Managed\.Pairing'''
        $script | Should -Match 'ValidateSet\(''DELETE-REMOTE-DOCKER-DATA''\)'
        $script | Should -Match 'remote-docker-managed-v1'
        $script | Should -Match 'FileAttributes\]::ReparsePoint'
        $normalExit | Should -BeGreaterThan -1
        $unregister | Should -BeGreaterThan $normalExit
        $treeCheck | Should -BeGreaterThan $normalExit
        $recursiveDelete | Should -BeGreaterThan $treeCheck
        $script | Should -Not -Match 'ScheduledTask|CurrentVersion\\Run'
        $script | Should -Not -Match 'Remove-Item[^\n]*(?:\$HOME|\$env:USERPROFILE|\\Users\\|\*|\?)'
    }

    It 'updates only the unified desktop executable after verification' {
        $script = Read-RepositoryFile $updateScript
        $hashCheck = $script.IndexOf('Get-FileHash')
        $signatureCheck = $script.IndexOf('Get-AuthenticodeSignature')
        $replace = $script.IndexOf('[System.IO.File]::Replace')

        $script | Should -Match 'RemoteDocker\.exe'
        $script | Should -Match '--shutdown'
        $script | Should -Not -Match 'RemoteDockerAgent|RemoteDockerTray|Start-Process'
        $hashCheck | Should -BeGreaterThan -1
        $signatureCheck | Should -BeGreaterThan $hashCheck
        $replace | Should -BeGreaterThan $signatureCheck
    }

    It 'keeps public installer content generic' {
        $publicFiles = @(
            $installerSource, $pagesSource, $stringsSource, $buildScript, $updateScript,
            $pathValidationScript, $provisionScript, $statusScript, $uninstallScript, $testScript
        )
        $internalPattern = 'S' + 'ber|Co' + 'work|Mid' + 'gard|Ygg' + 'drasil'

        foreach ($file in $publicFiles) {
            if (Test-Path -LiteralPath $file -PathType Leaf) {
                (Read-RepositoryFile $file) | Should -Not -Match $internalPattern
            }
        }
    }
}
