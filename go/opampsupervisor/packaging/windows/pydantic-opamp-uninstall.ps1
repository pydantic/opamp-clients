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

# Fleet hears of the uninstall from the Supervisor itself, as it stops: the request file asks it to
# tell its server, with the credentials it connects with (this script never reads them), and it
# writes what came of it to the notice file. Best effort, bounded by the Supervisor (10 seconds):
# the uninstall goes on whatever came of it.
$script:fleetTold = $false
function Request-Notice([string]$storage) {
    try {
        if (-not (Test-Path -LiteralPath $storage -PathType Container)) { return $false }
        Remove-Item -LiteralPath (Join-Path $storage 'uninstall-notice') -Force -ErrorAction SilentlyContinue
        [IO.File]::WriteAllText((Join-Path $storage 'uninstall-requested'), '')
        return $true
    } catch { return $false }
}
function Clear-Request([string]$storage) {
    Remove-Item -LiteralPath (Join-Path $storage 'uninstall-requested') -Force -ErrorAction SilentlyContinue
}
# Saying what came of it must not fail the removal: the files are read and removed if they can be.
function Write-Notice([string]$storage) {
    try {
        $line = ''
        $notice = Join-Path $storage 'uninstall-notice'
        if (Test-Path -LiteralPath $notice) { $line = ([IO.File]::ReadAllText($notice)).Trim() }
        if ($line -eq 'told') {
            $script:fleetTold = $true
            Write-Output 'Fleet was told this host is being removed.'
        } elseif ($line -like 'not told: *') {
            Write-Output "Fleet was not told this host is being removed: $($line.Substring(10))."
        } else {
            Write-Output 'Fleet was not told this host is being removed: the Supervisor did not say, as it stopped.'
        }
        Remove-Item -LiteralPath $notice -Force -ErrorAction SilentlyContinue
    } catch { }
    Clear-Request $storage
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
        $storage = Join-Path $root 'var'
        $requested = $running.Status -eq 'Running' -and (Request-Notice $storage)
        try {
            Stop-Service -Name $service -ErrorAction Stop
            $running.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
        } finally {
            # A stop that fails leaves no request: the Supervisor that goes on running must not
            # tell the server at a later stop.
            if ($requested) { Write-Notice $storage }
        }
    }
    # Keep executable recovery code under Program Files, separate from retained data.
    New-Item -ItemType Directory -Force -Path $programs | Out-Null
    if ($PSCommandPath -ne $recovery) { Copy-Item -LiteralPath $PSCommandPath -Destination $recovery -Force }
    # The property, as the package now sets it, for one installed by an older package: the
    # removal shuts down no other program that has one of its files open.
    foreach ($product in $products) {
        $result = Start-Process -FilePath msiexec.exe -ArgumentList @('/x', $product, '/qn', '/norestart', 'MSIRESTARTMANAGERCONTROL=DisableShutdown') -Wait -PassThru
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
# What is left, and why, as the Linux and macOS script says it: the copy of this script under
# Program Files is what deletes the kept data later, once the package (and its copy) is gone.
if (-not $DryRun) {
    if ($Purge) {
        Write-Output 'Removed the Collector service, the Pydantic OpAMP Supervisor package and its data.'
    } else {
        Write-Output "Removed the Collector service and the Pydantic OpAMP Supervisor package. Kept $root for a reinstall."
        Write-Output "To delete it too, run: & '$recovery' -Purge"
    }
    if (-not $script:fleetTold) { Write-Output 'The OpAMP server still lists this host until its credentials are revoked there.' }
}
