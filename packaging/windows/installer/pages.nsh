Var PreflightDialog
Var DataDialog
Var DataDirectoryInput
Var DataBrowseButton

Function PreflightPageCreate
  nsDialogs::Create 1018
  Pop $PreflightDialog
  ${If} $PreflightDialog == error
    Abort
  ${EndIf}

  ${NSD_CreateLabel} 0 0 100% 18u "$(PreflightTitle)"
  Pop $0
  CreateFont $1 "$(^Font)" "$(^FontSize)" 700
  SendMessage $0 ${WM_SETFONT} $1 1
  ${NSD_CreateLabel} 0 24u 100% 48u "$(PreflightBody)"
  Pop $0
  ${NSD_CreateLabel} 0 82u 100% 24u "$(VirtualizationPreflight)"
  Pop $0
  nsDialogs::Show
FunctionEnd

Function SelectDataDirectory
  nsDialogs::SelectFolderDialog "$(SelectDataDirectory)" "$DataDirectory"
  Pop $0
  ${If} $0 != error
    StrCpy $DataDirectory $0
    ${NSD_SetText} $DataDirectoryInput $DataDirectory
  ${EndIf}
FunctionEnd

Function DataPageCreate
  nsDialogs::Create 1018
  Pop $DataDialog
  ${If} $DataDialog == error
    Abort
  ${EndIf}

  ${NSD_CreateLabel} 0 0 100% 18u "$(SelectDataDirectory)"
  Pop $0
  CreateFont $1 "$(^Font)" "$(^FontSize)" 700
  SendMessage $0 ${WM_SETFONT} $1 1
  ${NSD_CreateLabel} 0 24u 100% 38u "$(SelectDataDirectoryHelp)"
  Pop $0
  ${NSD_CreateDirRequest} 0 72u 78% 14u "$DataDirectory"
  Pop $DataDirectoryInput
  ${NSD_CreateBrowseButton} 80% 71u 20% 15u "Обзор..."
  Pop $DataBrowseButton
  ${NSD_OnClick} $DataBrowseButton SelectDataDirectory
  nsDialogs::Show
FunctionEnd

Function DataPageLeave
  ${NSD_GetText} $DataDirectoryInput $DataDirectory
  ${GetRoot} "$DataDirectory" $0
  ${If} $0 == ""
    MessageBox MB_OK|MB_ICONEXCLAMATION "$(InvalidDataDirectory)"
    Abort
  ${EndIf}
FunctionEnd
