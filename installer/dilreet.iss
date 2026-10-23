; Inno Setup script for Dilreet. Build: iscc /DAppVersion=0.3.0 installer\dilreet.iss
#ifndef AppVersion
  #define AppVersion "0.3.0"
#endif
[Setup]
AppId={{8C4C3B9E-5D7A-4A8E-9F3B-D1E2E7A0B3C1}
AppName=Dilreet
AppVersion={#AppVersion}
AppPublisher=Dilreet
DefaultDirName={localappdata}\Programs\Dilreet
DefaultGroupName=Dilreet
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
OutputDir=..\dist
OutputBaseFilename=Dilreet-Setup-{#AppVersion}
SetupIconFile=..\winres\icon.ico
UninstallDisplayIcon={app}\dilreet.exe
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
CloseApplications=yes

[Files]
Source: "..\dilreet.exe"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\Dilreet"; Filename: "{app}\dilreet.exe"
Name: "{autodesktop}\Dilreet"; Filename: "{app}\dilreet.exe"; Tasks: desktopicon

[Tasks]
Name: "desktopicon"; Description: "Create a desktop shortcut"; Flags: unchecked

[Run]
Filename: "{app}\dilreet.exe"; Description: "Start Dilreet"; Flags: nowait postinstall skipifsilent

; Your projects (Documents\Dilreet) and settings (%APPDATA%\Dilreet) are kept on uninstall.
