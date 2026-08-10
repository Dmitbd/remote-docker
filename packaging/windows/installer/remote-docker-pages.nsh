Var InstallLocationDialog
Var BaseDirectoryInput
Var BaseBrowseButton
Var DesktopShortcutCheckbox

Function SelectBaseDirectory
  nsDialogs::SelectFolderDialog "$(SelectBaseDirectory)" "$BaseDirectory"
  Pop $0
  ${If} $0 != error
    StrCpy $BaseDirectory $0
    ${NSD_SetText} $BaseDirectoryInput $BaseDirectory
  ${EndIf}
FunctionEnd

Function InstallLocationPageCreate
  ${If} $ExistingInstall == "1"
    Abort
  ${EndIf}

  nsDialogs::Create 1018
  Pop $InstallLocationDialog
  ${If} $InstallLocationDialog == error
    Abort
  ${EndIf}

  ${NSD_CreateLabel} 0 0 100% 18u "$(InstallLocationTitle)"
  Pop $0
  CreateFont $1 "$(^Font)" "$(^FontSize)" 700
  SendMessage $0 ${WM_SETFONT} $1 1
  ${NSD_CreateLabel} 0 24u 100% 38u "$(InstallLocationHelp)"
  Pop $0
  ${NSD_CreateDirRequest} 0 72u 78% 14u "$BaseDirectory"
  Pop $BaseDirectoryInput
  ${NSD_CreateBrowseButton} 80% 71u 20% 15u "$(BrowseButton)"
  Pop $BaseBrowseButton
  ${NSD_OnClick} $BaseBrowseButton SelectBaseDirectory
  ${NSD_CreateCheckbox} 0 102u 100% 14u "$(CreateDesktopShortcut)"
  Pop $DesktopShortcutCheckbox
  ${NSD_SetState} $DesktopShortcutCheckbox $CreateDesktopShortcut
  nsDialogs::Show
FunctionEnd

Function InstallLocationPageLeave
  ${NSD_GetText} $BaseDirectoryInput $BaseDirectory
  ${NSD_GetState} $DesktopShortcutCheckbox $CreateDesktopShortcut
  ${GetRoot} "$BaseDirectory" $0
  ${If} $0 == ""
    MessageBox MB_OK|MB_ICONEXCLAMATION "$(InvalidBaseDirectory)"
    Abort
  ${EndIf}
  GetFullPathName $BaseDirectory "$BaseDirectory"
  ${GetRoot} "$BaseDirectory" $0
  ${If} $BaseDirectory == $0
    MessageBox MB_OK|MB_ICONEXCLAMATION "$(InvalidBaseDirectory)"
    Abort
  ${EndIf}
  StrCpy $INSTDIR "$BaseDirectory\App"
  StrCpy $DataDirectory "$BaseDirectory\Data"
FunctionEnd
