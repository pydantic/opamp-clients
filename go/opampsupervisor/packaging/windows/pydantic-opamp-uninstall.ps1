# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
# Removes the MSI and its service. Keep data and a recovery helper unless -Purge.
[CmdletBinding()]
param([switch]$Purge, [switch]$DryRun)
$ErrorActionPreference = 'Stop'
$root = $env:OPAMP_UNINSTALL_ROOT
if (-not $root) { $root = Join-Path $env:ProgramData 'pydantic-opamp' }
if (-not [IO.Path]::IsPathRooted($root) -or (Split-Path -Leaf $root) -ne 'pydantic-opamp') {
    throw 'The data directory must be an absolute path ending in pydantic-opamp.'
}
$programs = Join-Path $env:ProgramFiles 'pydantic-opamp'
$recovery = Join-Path $programs 'uninstall.ps1'
$service = 'pydantic-opamp-supervisor'

function Assert-PlainParents([string]$path) {
    while ($path) {
        if (Test-Path -LiteralPath $path) {
            if ((Get-Item -Force -LiteralPath $path).Attributes -band [IO.FileAttributes]::ReparsePoint) {
                throw "Refusing redirected path: $path"
            }
        }
        $path = Split-Path -Parent $path
    }
}

# Windows PowerShell 5 must not follow junctions during a purge.
function Remove-OwnedTree([string]$path) {
    if (-not (Test-Path -LiteralPath $path)) { return }
    $item = Get-Item -Force -LiteralPath $path
    if ($item.PSIsContainer) {
        if (-not ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) {
            foreach ($child in Get-ChildItem -Force -LiteralPath $path) { Remove-OwnedTree $child.FullName }
        }
        [IO.Directory]::Delete($path)
    } else {
        [IO.File]::Delete($path)
    }
}

function Uninstall-Collector {
    if ($DryRun) {
        Write-Output "Stop $service and remove its MSI package."
        if ($Purge) { Write-Output "Delete $root, including credentials and log read positions." }
        else { Write-Output "Keep $root for reinstall and a later purge." }
        return
    }
    if (-not $env:OPAMP_UNINSTALL_ROOT) {
        $admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
            [Security.Principal.WindowsBuiltInRole]::Administrator)
        if (-not $admin) { throw 'Run this command in an Administrator terminal.' }
    }
    Assert-PlainParents $root
    Assert-PlainParents $programs
    Assert-PlainParents $recovery
    # RelatedProducts reads Windows Installer registration without Win32_Product repair side effects.
    $installer = New-Object -ComObject WindowsInstaller.Installer
    $products = @($installer.RelatedProducts('{A7C976D2-A6DB-4393-86BC-E59B0FB90C66}'))
    foreach ($product in $products) {
        if ($product -notmatch '^\{[0-9A-Fa-f-]{36}\}$') { throw 'Invalid installed MSI product code.' }
    }
    $running = $null
    try { $running = Get-Service -Name $service -ErrorAction Stop }
    catch { if ($_.FullyQualifiedErrorId -notlike 'NoServiceFoundForGivenName*') { throw } }
    if ($running -and -not $products.Count) {
        throw 'The service has no registered MSI package. Retained data has not been deleted.'
    }
    if ($running) {
        Stop-Service -Name $service -ErrorAction Stop
        $running.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    }
    # Keep executable recovery code under Program Files, separate from retained data.
    New-Item -ItemType Directory -Force -Path $programs | Out-Null
    if ($PSCommandPath -ne $recovery) { Copy-Item -LiteralPath $PSCommandPath -Destination $recovery -Force }
    foreach ($product in $products) {
        $result = Start-Process -FilePath msiexec.exe -ArgumentList @('/x', $product, '/qn', '/norestart') -Wait -PassThru
        if ($result.ExitCode -eq 3010) {
            try { [Console]::Error.WriteLine('Removal completed. Restart Windows to finish removing the Collector.') } catch { }
        } elseif ($result.ExitCode -ne 0) {
            throw "MSI removal failed ($($result.ExitCode)). Retained data has not been deleted."
        }
    }
    if ($Purge) {
        Remove-OwnedTree $root
        Remove-Item -LiteralPath $recovery -Force
        if (-not @(Get-ChildItem -Force -LiteralPath $programs).Count) { [IO.Directory]::Delete($programs) }
    }
}
Uninstall-Collector
