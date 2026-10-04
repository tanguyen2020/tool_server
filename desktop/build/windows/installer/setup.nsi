; Windows installer for Server Dashboard.
; Installs for the current user (no administrator rights) in %LOCALAPPDATA%\Programs\ServerDashboard, a
; folder the app can write to, so it can update itself later without this installer.
;
; makensis /DVERSION=1.2.3 /DSRC=<path to ServerDashboard.exe> /DOUTFILE=<setup.exe> setup.nsi

Unicode true
!include "MUI2.nsh"
!include "FileFunc.nsh"

!ifndef VERSION
  !define VERSION "0.0.0"
!endif
!ifndef SRC
  !error "Pass /DSRC=<path to ServerDashboard.exe>"
!endif
!ifndef OUTFILE
  !define OUTFILE "ServerDashboard-setup.exe"
!endif

!define APPNAME "Server Dashboard"
!define EXE "ServerDashboard.exe"
!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\ServerDashboard"

Name "${APPNAME}"
OutFile "${OUTFILE}"
InstallDir "$LOCALAPPDATA\Programs\ServerDashboard"
InstallDirRegKey HKCU "${UNINSTKEY}" "InstallLocation"
RequestExecutionLevel user
SetCompressor /SOLID lzma
BrandingText "${APPNAME} ${VERSION}"
ShowInstDetails nevershow
ShowUninstDetails nevershow

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
!define MUI_ABORTWARNING
!define MUI_WELCOMEPAGE_TEXT "This installs ${APPNAME} ${VERSION} for your Windows account (no administrator rights needed).$\r$\n$\r$\nThe app keeps itself up to date: new versions install automatically, there is no need to uninstall first.$\r$\n$\r$\nYour servers, settings and history are kept when you reinstall or update."
!define MUI_FINISHPAGE_RUN "$INSTDIR\${EXE}"
!define MUI_FINISHPAGE_RUN_TEXT "Start ${APPNAME}"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Section "Install"
  SetOutPath "$INSTDIR"

  ; A running copy locks its file: ask to close it (reinstalling over an open app).
  retry:
  ClearErrors
  IfFileExists "$INSTDIR\${EXE}" 0 copy
  Delete "$INSTDIR\${EXE}"
  IfErrors 0 copy
  MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "${APPNAME} is running.$\r$\nClose it, then click Retry." /SD IDCANCEL IDRETRY retry
  Abort
  copy:
  File "/oname=${EXE}" "${SRC}"
  Delete "$INSTDIR\${EXE}.old"
  Delete "$INSTDIR\${EXE}.update"
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  CreateShortcut "$SMPROGRAMS\${APPNAME}.lnk" "$INSTDIR\${EXE}"
  CreateShortcut "$DESKTOP\${APPNAME}.lnk" "$INSTDIR\${EXE}"

  ; "Apps & features" entry (the app keeps DisplayVersion current after it updates itself).
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayName" "${APPNAME}"
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINSTKEY}" "Publisher" "TaNguyen"
  WriteRegStr HKCU "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\${EXE}"
  WriteRegStr HKCU "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINSTKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr HKCU "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegStr HKCU "${UNINSTKEY}" "URLInfoAbout" "https://github.com/tanguyen2020/tool_server"
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINSTKEY}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  IntFmt $0 "0x%08X" $0
  WriteRegDWORD HKCU "${UNINSTKEY}" "EstimatedSize" "$0"
SectionEnd

Section "Uninstall"
  uretry:
  ClearErrors
  IfFileExists "$INSTDIR\${EXE}" 0 udel
  Delete "$INSTDIR\${EXE}"
  IfErrors 0 udel
  MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "${APPNAME} is running.$\r$\nClose it, then click Retry." /SD IDCANCEL IDRETRY uretry
  Abort
  udel:
  Delete "$INSTDIR\${EXE}.old"
  Delete "$INSTDIR\${EXE}.update"
  Delete "$INSTDIR\${EXE}.update.part"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\${APPNAME}.lnk"
  Delete "$DESKTOP\${APPNAME}.lnk"
  DeleteRegKey HKCU "${UNINSTKEY}"

  ; The server list, settings and history stay unless the user asks (silent uninstall keeps them).
  MessageBox MB_YESNO|MB_ICONQUESTION|MB_DEFBUTTON2 "Also delete your server list, settings and history?$\r$\n$\r$\n$APPDATA\ServerDashboard" /SD IDNO IDNO keep
  RMDir /r "$APPDATA\ServerDashboard"
  keep:
SectionEnd
