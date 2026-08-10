Unicode true
ManifestDPIAware true
RequestExecutionLevel admin
SetCompressor /SOLID lzma
SetCompressorDictSize 32

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "nsDialogs.nsh"
!include "FileFunc.nsh"
!include "WinVer.nsh"
!include "x64.nsh"

!ifndef PRODUCT_VERSION
  !error "PRODUCT_VERSION is required"
!endif
!ifndef OUTPUT_FILE
  !error "OUTPUT_FILE is required"
!endif
!ifndef UI_SOURCE
  !error "UI_SOURCE is required"
!endif

Name "Remote Docker"
Caption "Установка Remote Docker"
OutFile "${OUTPUT_FILE}"
InstallDir "$PROGRAMFILES64\Remote Docker"
Icon "${ICON_SOURCE}"
UninstallIcon "${ICON_SOURCE}"
BrandingText "Remote Docker"
VIProductVersion "${PRODUCT_VERSION}.0"

Var DataDirectory
Var BaseDirectory
Var ExistingApplicationRoot
Var ExistingDataRoot
Var ExistingInstall
Var CreateDesktopShortcut
Var ProvisionExit
Var ProvisionOutput
Var ProgressPath
Var LogPath
Var WebView2Version

!include "remote-docker-pages.nsh"

!define MUI_ABORTWARNING
!define MUI_ICON "${ICON_SOURCE}"
!define MUI_UNICON "${ICON_SOURCE}"
!define MUI_WELCOMEPAGE_TITLE "Установка Remote Docker"
!define MUI_WELCOMEPAGE_TEXT "$(ProductDescription)$\r$\n$\r$\nПриложение запускается только вручную."
!define MUI_FINISHPAGE_RUN "$INSTDIR\RemoteDocker.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Запустить Remote Docker"
!define MUI_FINISHPAGE_RUN_NOTCHECKED

!insertmacro MUI_PAGE_WELCOME
Page custom InstallLocationPageCreate InstallLocationPageLeave
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "Russian"
!include "strings.nsh"

VIAddVersionKey /LANG=${LANG_RUSSIAN} "ProductName" "Remote Docker"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "FileDescription" "Remote Docker Setup"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "FileVersion" "${PRODUCT_VERSION}"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "ProductVersion" "${PRODUCT_VERSION}"
VIAddVersionKey /LANG=${LANG_RUSSIAN} "LegalCopyright" "Copyright Remote Docker contributors"

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_OK|MB_ICONSTOP "Remote Docker поддерживает только 64-разрядную Windows."
    Abort
  ${EndIf}
  ${IfNot} ${AtLeastWin10}
    MessageBox MB_OK|MB_ICONSTOP "Требуется Windows 10 или Windows 11."
    Abort
  ${EndIf}
  SetRegView 64
  ReadRegStr $WebView2Version HKLM "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  ${If} $WebView2Version == ""
    ReadRegStr $WebView2Version HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
  ${EndIf}
  ${If} $WebView2Version == ""
    SetRegView 32
    ReadRegStr $WebView2Version HKLM "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
    ${If} $WebView2Version == ""
      ReadRegStr $WebView2Version HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
    ${EndIf}
    SetRegView 64
  ${EndIf}
  ${If} $WebView2Version == ""
    MessageBox MB_YESNO|MB_ICONEXCLAMATION "Для интерфейса Remote Docker требуется Microsoft Edge WebView2 Runtime. Открыть официальный загрузчик Microsoft?" IDYES open_webview2 IDNO no_webview2
open_webview2:
    ExecShell "open" "https://go.microsoft.com/fwlink/p/?LinkId=2124703"
no_webview2:
    Abort
  ${EndIf}
  SetShellVarContext all
  StrCpy $ExistingInstall "0"
  StrCpy $CreateDesktopShortcut ${BST_CHECKED}
  ReadRegStr $ExistingApplicationRoot HKLM "Software\Remote Docker" "InstallDirectory"
  ReadRegStr $ExistingDataRoot HKLM "Software\Remote Docker" "DataDirectory"
  ${If} $ExistingApplicationRoot != ""
  ${AndIf} $ExistingDataRoot != ""
    StrCpy $ExistingInstall "1"
    StrCpy $INSTDIR $ExistingApplicationRoot
    StrCpy $DataDirectory $ExistingDataRoot
    IfFileExists "$DESKTOP\Remote Docker.lnk" 0 +2
    StrCpy $CreateDesktopShortcut ${BST_CHECKED}
    IfFileExists "$DESKTOP\Remote Docker.lnk" +2 0
    StrCpy $CreateDesktopShortcut ${BST_UNCHECKED}
  ${Else}
    StrCpy $BaseDirectory "$PROGRAMFILES64\Remote Docker"
    StrCpy $INSTDIR "$BaseDirectory\App"
    StrCpy $DataDirectory "$BaseDirectory\Data"
  ${EndIf}
FunctionEnd

Section "Основные файлы и Docker-среда" CoreSection
  SectionIn RO
  SetShellVarContext all
  SetRegView 64
  CreateDirectory "$INSTDIR"
  CreateDirectory "$INSTDIR\assets"
  CreateDirectory "$INSTDIR\tools"
  CreateDirectory "$DataDirectory"
  StrCpy $ProgressPath "$DataDirectory\installer-progress.jsonl"
  StrCpy $LogPath "$DataDirectory\installer.log"
  FileOpen $0 "$LogPath" w
  FileWrite $0 "Remote Docker ${PRODUCT_VERSION}$\r$\n"
  FileClose $0

  DetailPrint "$(InstallingFiles)"
  IfFileExists "$INSTDIR\RemoteDocker.exe" 0 +4
  nsExec::ExecToStack /TIMEOUT=15000 '"$INSTDIR\RemoteDocker.exe" --shutdown'
  Pop $0
  Pop $1
  SetOutPath "$INSTDIR"
  File /oname=RemoteDocker.exe "${APP_SOURCE}"
  File /oname=remote-docker-ui.exe "${UI_SOURCE}"
  File /oname=remote-docker.ico "${ICON_SOURCE}"
  SetOutPath "$INSTDIR\assets"
  File /oname=remote-docker-rootfs.tar.zst "${ROOTFS_SOURCE}"
  File /oname=remote-docker-rootfs.tar.zst.sha256 "${ROOTFS_CHECKSUM_SOURCE}"
  File /oname=remote-docker-remote-linux-amd64 "${RUNTIME_SOURCE}"
  File /oname=remote-docker-remote-linux-amd64.sha256 "${RUNTIME_CHECKSUM_SOURCE}"
  SetOutPath "$INSTDIR\tools"
  File /oname=probe.ps1 "${PROBE_SOURCE}"
  File /oname=provision.ps1 "${PROVISION_SOURCE}"
  File /oname=provision-status.ps1 "${STATUS_SOURCE}"
  File /oname=path-validation.ps1 "${PATH_VALIDATION_SOURCE}"
  File /oname=uninstall.ps1 "${UNINSTALL_SOURCE}"
  File /oname=install-agent.ps1 "${UPDATE_SOURCE}"

  Delete "$DataDirectory\installer-reboot.pending"
retry_provision:
  DetailPrint "$(ProvisioningWSL)"
  DetailPrint "$(ConfiguringDocker)"
  DetailPrint "$(ConfiguringFirewall)"
  nsExec::ExecToStack /TIMEOUT=3600000 '"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "$INSTDIR\tools\provision.ps1" -ConfirmProvisioning -ApplicationRoot "$INSTDIR" -DataRoot "$DataDirectory" -ProgressPath "$ProgressPath" -LogPath "$LogPath"'
  Pop $ProvisionExit
  Pop $ProvisionOutput
  ${If} $ProvisionExit == 3010
    FileOpen $0 "$DataDirectory\installer-reboot.pending" w
    FileWrite $0 "${PRODUCT_VERSION}$\r$\n"
    FileClose $0
    SetRebootFlag true
    MessageBox MB_OK|MB_ICONINFORMATION "$(RebootRequired)"
    SetErrorLevel 3010
    Quit
  ${ElseIf} $ProvisionExit != 0
    ${If} $ProvisionOutput != ""
      FileOpen $0 "$LogPath" a
      FileWrite $0 "$ProvisionOutput$\r$\n"
      FileClose $0
    ${EndIf}
    MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "$(InstallFailed)$\r$\n$\r$\n$ProvisionOutput$\r$\n$\r$\n$(InstallLogPath) $LogPath$\r$\n$\r$\n$(InstallRetry)" IDRETRY retry_provision IDCANCEL provision_failed
  ${Else}
    Delete "$DataDirectory\installer-reboot.pending"
  ${EndIf}

  DetailPrint "$(CreatingShortcuts)"
  CreateDirectory "$SMPROGRAMS\Remote Docker"
  CreateShortCut "$SMPROGRAMS\Remote Docker\Remote Docker.lnk" "$INSTDIR\RemoteDocker.exe" "" "$INSTDIR\remote-docker.ico"
  CreateShortCut "$SMPROGRAMS\Remote Docker\Удалить Remote Docker.lnk" "$INSTDIR\Uninstall.exe"
  ${If} $CreateDesktopShortcut == ${BST_CHECKED}
    CreateShortCut "$DESKTOP\Remote Docker.lnk" "$INSTDIR\RemoteDocker.exe" "" "$INSTDIR\remote-docker.ico"
  ${Else}
    Delete "$DESKTOP\Remote Docker.lnk"
  ${EndIf}

  WriteRegStr HKLM "Software\Remote Docker" "InstallDirectory" "$INSTDIR"
  WriteRegStr HKLM "Software\Remote Docker" "DataDirectory" "$DataDirectory"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RemoteDocker" "DisplayName" "Remote Docker"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RemoteDocker" "DisplayVersion" "${PRODUCT_VERSION}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RemoteDocker" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RemoteDocker" "DisplayIcon" "$INSTDIR\RemoteDocker.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RemoteDocker" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RemoteDocker" "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RemoteDocker" "NoRepair" 1
  WriteUninstaller "$INSTDIR\Uninstall.exe"
  Goto provision_done

provision_failed:
  Delete "$DataDirectory\installer-reboot.pending"
  SetErrorLevel 1
  Quit
provision_done:
SectionEnd

Section "Uninstall"
  SetShellVarContext all
  SetRegView 64
  ReadRegStr $DataDirectory HKLM "Software\Remote Docker" "DataDirectory"
  ${If} $DataDirectory == ""
    StrCpy $DataDirectory "$APPDATA\RemoteDocker"
  ${EndIf}

  nsExec::ExecToStack /TIMEOUT=15000 '"$INSTDIR\RemoteDocker.exe" --shutdown'
  Pop $0
  Pop $1
  nsExec::ExecToStack /TIMEOUT=120000 '"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "$INSTDIR\tools\uninstall.ps1" -ApplicationRoot "$INSTDIR" -DataRoot "$DataDirectory"'
  Pop $0
  Pop $1

  Delete "$DESKTOP\Remote Docker.lnk"
  Delete "$SMPROGRAMS\Remote Docker\Remote Docker.lnk"
  Delete "$SMPROGRAMS\Remote Docker\Удалить Remote Docker.lnk"
  RMDir "$SMPROGRAMS\Remote Docker"
  Delete "$DataDirectory\installer-reboot.pending"
  Delete "$INSTDIR\RemoteDocker.exe"
  Delete "$INSTDIR\remote-docker-ui.exe"
  Delete "$INSTDIR\remote-docker.ico"
  Delete "$INSTDIR\assets\remote-docker-rootfs.tar.zst"
  Delete "$INSTDIR\assets\remote-docker-rootfs.tar.zst.sha256"
  Delete "$INSTDIR\assets\remote-docker-remote-linux-amd64"
  Delete "$INSTDIR\assets\remote-docker-remote-linux-amd64.sha256"
  Delete "$INSTDIR\tools\probe.ps1"
  Delete "$INSTDIR\tools\provision.ps1"
  Delete "$INSTDIR\tools\provision-status.ps1"
  Delete "$INSTDIR\tools\path-validation.ps1"
  Delete "$INSTDIR\tools\uninstall.ps1"
  Delete "$INSTDIR\tools\install-agent.ps1"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR\assets"
  RMDir "$INSTDIR\tools"
  RMDir "$INSTDIR"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\RemoteDocker"
  DeleteRegKey HKLM "Software\Remote Docker"
SectionEnd
