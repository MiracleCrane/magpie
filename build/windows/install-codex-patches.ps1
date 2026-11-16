[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)][string]$PackageDirectory,
    [Parameter(Mandatory)][string]$Target,
    [Parameter(Mandatory)][ValidatePattern('^[A-Fa-f0-9]{64}$')][string]$ExpectedCurrentSha256
)
$ErrorActionPreference = 'Stop'
function Get-ActiveMagpieHosts {
    $processes = @(Get-CimInstance Win32_Process -ErrorAction Stop)
    foreach ($process in $processes) {
        if ($process.Name -notmatch '^(magpie|codex|ChatGPT)\.exe$') { continue }
        if ($process.Name -ieq 'codex.exe') {
            $parent = $processes | Where-Object ProcessId -eq $process.ParentProcessId | Select-Object -First 1
            if ($parent.Name -ieq 'QuotaOrb.exe') { continue }
        }
        $process
    }
}
$package = [IO.Path]::GetFullPath($PackageDirectory)
$targetPath = [IO.Path]::GetFullPath($Target)
$candidate = Join-Path $package 'magpie.exe'
$manifest = Get-Content -LiteralPath (Join-Path $package 'build.json') -Raw | ConvertFrom-Json
if ((Get-FileHash -LiteralPath $candidate -Algorithm SHA256).Hash -ne $manifest.sha256) { throw 'Package checksum mismatch.' }
if ((Get-FileHash -LiteralPath $targetPath -Algorithm SHA256).Hash -ne $ExpectedCurrentSha256) { throw 'Installed Magpie changed after preparation; inspect before replacing it.' }
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class MagpiePackageIcons {
    [DllImport("shell32.dll", CharSet=CharSet.Unicode)]
    public static extern uint ExtractIconEx(string path, int index, IntPtr[] large, IntPtr[] small, uint count);
}
'@
if ([MagpiePackageIcons]::ExtractIconEx($candidate, -1, $null, $null, 0) -eq 0) { throw 'Package has no executable icon resources.' }
$active = @(Get-ActiveMagpieHosts)
if ($active.Count) { throw 'Finish work and fully exit Codex and Magpie, including the tray. No process was stopped or file replaced.' }
if (-not $PSCmdlet.ShouldProcess($targetPath, "Install $($manifest.version) with verified backup")) { return }
$backupRoot = Join-Path (Split-Path -Parent $targetPath) ('backups\' + [DateTime]::Now.ToString('yyyyMMdd-HHmmss') + '-codex-patches')
New-Item -ItemType Directory -Path $backupRoot | Out-Null
$backup = Join-Path $backupRoot 'magpie.exe'
Copy-Item -LiteralPath $targetPath -Destination $backup
if ((Get-FileHash -LiteralPath $backup -Algorithm SHA256).Hash -ne $ExpectedCurrentSha256) { throw 'Backup checksum mismatch.' }
$pending = Join-Path (Split-Path -Parent $targetPath) ('magpie-install-' + [guid]::NewGuid().ToString('N') + '.tmp')
try {
    Copy-Item -LiteralPath $candidate -Destination $pending
    if ((Get-FileHash -LiteralPath $pending -Algorithm SHA256).Hash -ne $manifest.sha256) { throw 'Copied package checksum mismatch.' }
    $active = @(Get-ActiveMagpieHosts)
    if ($active.Count) { throw 'Codex or Magpie started while preparing installation; close them and retry.' }
    Move-Item -LiteralPath $pending -Destination $targetPath -Force
    if ((Get-FileHash -LiteralPath $targetPath -Algorithm SHA256).Hash -ne $manifest.sha256) { throw 'Installed checksum mismatch; restore the verified backup before starting Magpie.' }
} finally { if (Test-Path -LiteralPath $pending) { Remove-Item -LiteralPath $pending } }
$linkPath = Join-Path ([Environment]::GetFolderPath('StartMenu')) 'Programs\Magpie.lnk'
if (Test-Path -LiteralPath $linkPath) {
    $shell = New-Object -ComObject WScript.Shell
    $link = $shell.CreateShortcut($linkPath)
    if ($link.TargetPath -ieq $targetPath) {
        Copy-Item -LiteralPath $linkPath -Destination (Join-Path $backupRoot 'Magpie.lnk')
        $link.IconLocation = $targetPath + ',0'
        $link.Save()
    }
}
[ordered]@{ version=$manifest.version; sha256=$manifest.sha256; target=$targetPath; backup=$backup; installedAt=[DateTimeOffset]::Now.ToString('o') } |
    ConvertTo-Json | Set-Content -LiteralPath (Join-Path $package 'installed.json') -Encoding utf8
Write-Output "Installed $($manifest.version). Reopen Codex from the usual launcher; verify the live Magpie version and taskbar icon. Backup: $backup"
