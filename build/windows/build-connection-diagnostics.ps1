[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$OutputDirectory,
    [string]$Go = 'go',
    [ValidatePattern('^\d+\.\d+\.\d+$')][string]$BaseVersion = '0.1.3827'
)
$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$destination = [IO.Path]::GetFullPath($OutputDirectory)
if ($destination.StartsWith($repo + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'Keep build output outside the source worktree.'
}
$version = 'dev-lzc-connections-' + $BaseVersion
New-Item -ItemType Directory -Path $destination -Force | Out-Null
function Invoke-Go([string[]]$Arguments) {
    & $Go @Arguments
    if ($LASTEXITCODE -ne 0) { throw "Go command failed ($LASTEXITCODE): $($Arguments -join ' ')" }
}
Push-Location $repo
try {
    if ((git branch --show-current) -ne 'lzc/codex-connection-diagnostics') { throw 'Build from lzc/codex-connection-diagnostics.' }
    if (git status --porcelain=v1) { throw 'Commit and verify the exact diagnostic source before building.' }
    $revision = git rev-parse HEAD
    if ($LASTEXITCODE -ne 0) { throw 'Could not read source revision.' }
    $resources = Join-Path $repo 'rsrc_windows_amd64.syso'
    $packed = Join-Path $repo 'internal/gui/assets.zip'
    $launcher = Join-Path $repo 'internal/update/launcher-amd64.com'
    foreach ($generated in @($resources, $packed, $launcher)) {
        if (Test-Path -LiteralPath $generated) { throw "Generated file already exists; inspect it first: $generated" }
    }
    $oldGOOS = $env:GOOS
    $oldGOARCH = $env:GOARCH
    $oldCGO = $env:CGO_ENABLED
    try {
        $env:GOOS = 'windows'
        $env:GOARCH = 'amd64'
        $env:CGO_ENABLED = '0'
        Invoke-Go -Arguments @('run', './build/assetpack', 'internal/gui/assets', 'internal/gui/assets.zip')
        Invoke-Go -Arguments @('run', 'github.com/tc-hib/go-winres@v0.3.3', 'make', '--in', 'build/windows/winres.json', '--arch', 'amd64', '--out', 'rsrc', '--file-version', $BaseVersion, '--product-version', $BaseVersion)
        Invoke-Go -Arguments @('build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w', '-o', $launcher, './internal/update/launcher')
        $binary = Join-Path $destination 'magpie.exe'
        Invoke-Go -Arguments @('build', '-tags', 'production,comlauncher,packedassets', '-trimpath', "-ldflags=-s -w -X main.version=$version -H windowsgui", '-o', $binary, '.')
        [ordered]@{
            version = $version
            upstream = "v$BaseVersion"
            revision = $revision.Trim()
            sha256 = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash
            size = (Get-Item -LiteralPath $binary).Length
            builtAt = [DateTimeOffset]::Now.ToString('o')
            automaticOfficialReplacement = $false
        } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $destination 'build.json') -Encoding utf8
    } finally {
        $env:GOOS = $oldGOOS
        $env:GOARCH = $oldGOARCH
        $env:CGO_ENABLED = $oldCGO
        foreach ($generated in @($resources, $packed, $launcher)) {
            if (Test-Path -LiteralPath $generated) { Remove-Item -LiteralPath $generated }
        }
    }
} finally { Pop-Location }
