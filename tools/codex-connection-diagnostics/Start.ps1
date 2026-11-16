param([switch]$DesktopContext)
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$config = Get-Content -LiteralPath (Join-Path $root 'collector-config.json') -Raw | ConvertFrom-Json
foreach ($file in @($config.node, $config.powerShell, $config.wsModule)) {
    if (-not [IO.Path]::IsPathFullyQualified($file) -or -not (Test-Path -LiteralPath $file)) { throw 'A configured diagnostic runtime path is missing.' }
}
if (-not [IO.Path]::IsPathFullyQualified($config.codexHome)) { throw 'Codex home must be absolute.' }
if (-not $DesktopContext) {
    $shell = New-Object -ComObject Shell.Application
    $handle = 0
    $desktop = $shell.Windows().FindWindowSW(0, 0, 8, [ref]$handle, 1)
    if ($null -eq $desktop) { throw 'Explorer desktop is required to read the physical Magpie log.' }
    $desktop.Document.Application.ShellExecute($config.powerShell, ('-NoProfile -File "' + $PSCommandPath + '" -DesktopContext'), $root, 'open', 0)
    return
}
$status = Join-Path $root 'status.json'
if (Test-Path -LiteralPath $status) {
    $previous = Get-Content -LiteralPath $status -Raw | ConvertFrom-Json
    if ($previous.running -and ([DateTimeOffset]::Now - [DateTimeOffset]::Parse($previous.updatedAt)).TotalSeconds -lt 15 -and
        (Get-Process -Id $previous.pid -ErrorAction SilentlyContinue)) { return }
}
$arguments = @('--disable-warning=ExperimentalWarning', ('"' + (Join-Path $root 'collector.mjs') + '"'),
    '--root', ('"' + $root + '"'), '--app-log', ('"' + (Join-Path $env:LOCALAPPDATA 'magpie/logs/app.log') + '"'),
    '--codex-home', ('"' + $config.codexHome + '"'), '--ws-module', ('"' + $config.wsModule + '"'))
Start-Process -FilePath $config.node -ArgumentList $arguments -WindowStyle Hidden -RedirectStandardOutput (Join-Path $root 'stdout.log') -RedirectStandardError (Join-Path $root 'stderr.log')
