param(
    [string]$ExePath = (Join-Path (Resolve-Path (Join-Path $PSScriptRoot "..")).Path "jsx-window.exe"),
    [switch]$SetDefaultAssociation
)

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$progID = "Jsxx.JSX.Window"
$menuKey = "HKCU:\Software\Classes\SystemFileAssociations\.jsx\shell\OpenInJsxWindow"
$progKey = "HKCU:\Software\Classes\$progID"

if (-not (Test-Path $ExePath)) {
    throw "Executable not found at $ExePath. Build it first with scripts\build-jsx-window.ps1."
}

$exeFullPath = (Resolve-Path $ExePath).Path
$command = "`"$exeFullPath`" `"%1`""

New-Item -Path $menuKey -Force | Out-Null
Set-Item -Path $menuKey -Value "Open in JSX Window"
New-ItemProperty -Path $menuKey -Name "Icon" -Value $exeFullPath -PropertyType String -Force | Out-Null
New-Item -Path "$menuKey\command" -Force | Out-Null
Set-Item -Path "$menuKey\command" -Value $command

New-Item -Path $progKey -Force | Out-Null
Set-Item -Path $progKey -Value "JSX Window File"
New-ItemProperty -Path $progKey -Name "FriendlyTypeName" -Value "JSX Window File" -PropertyType String -Force | Out-Null
New-Item -Path "$progKey\DefaultIcon" -Force | Out-Null
Set-Item -Path "$progKey\DefaultIcon" -Value $exeFullPath
New-Item -Path "$progKey\shell\open\command" -Force | Out-Null
Set-Item -Path "$progKey\shell\open\command" -Value $command

if ($SetDefaultAssociation) {
    New-Item -Path "HKCU:\Software\Classes\.jsx" -Force | Out-Null
    Set-Item -Path "HKCU:\Software\Classes\.jsx" -Value $progID
    Write-Host "Set $progID as the default handler for .jsx under the current user."
} else {
    Write-Host "Registered the 'Open in JSX Window' context menu for .jsx files."
    Write-Host "Run again with -SetDefaultAssociation if you want this app to become the default .jsx handler."
}

Write-Host "Executable: $exeFullPath"
Write-Host "Project root: $repoRoot"
