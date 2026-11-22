# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# Builds the Windows installer into packaging\dist, on Windows: the bundled collector (upstream's
# otelcol-contrib, pinned in ..\contrib.json), the Supervisor, then an .msi (pydantic-opamp-supervisor.wxs, WiX 5):
#   go\opampsupervisor\packaging\windows\build.ps1 -Version 0.1.0
# The MSI version is numbers only (major.minor.build). With -CertificateThumbprint, signtool
# signs both executables, the two helper scripts and the .msi with that certificate from the
# machine's store, so a host whose execution policy is AllSigned runs the scripts. The scripts
# are copied next to the executables and signed there; the ones in this directory stay as they are.
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [ValidateSet('amd64', 'arm64')][string]$Arch = 'amd64',
    [string]$CertificateThumbprint
)
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $true
$packaging = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $packaging "dist\windows-$Arch"
New-Item -ItemType Directory -Force -Path $bin | Out-Null
$env:GOOS = 'windows'; $env:GOARCH = $Arch; $env:CGO_ENABLED = '0'

function Sign([string[]]$paths) {
    if ($CertificateThumbprint) {
        signtool sign /sm /sha1 $CertificateThumbprint /fd sha256 /tr http://timestamp.digicert.com /td sha256 @paths
    }
}

Push-Location (Split-Path -Parent $packaging)
try {
    # The fetcher runs on this machine, whatever GOARCH the packages are for.
    $env:GOOS = ''; $env:GOARCH = ''
    # Into dist\otelcol, as on Linux and macOS: the install tests validate configs with it there.
    go run ./internal/fetchcontrib -os windows -arch $Arch -out packaging\dist\otelcol\otelcol.exe
    $env:GOOS = 'windows'; $env:GOARCH = $Arch
    Copy-Item packaging\dist\otelcol\otelcol.exe (Join-Path $bin 'otelcol.exe') -Force
    go build -trimpath -o (Join-Path $bin 'opampsupervisor.exe') .
} finally {
    Pop-Location
}
$scripts = foreach ($name in 'pydantic-opamp-enroll.ps1', 'pydantic-opamp-uninstall.ps1') {
    Copy-Item (Join-Path $PSScriptRoot $name) (Join-Path $bin $name) -Force
    Join-Path $bin $name
}
Sign (@((Join-Path $bin 'opampsupervisor.exe'), (Join-Path $bin 'otelcol.exe')) + $scripts)

$msi = Join-Path $packaging "dist\pydantic-opamp-supervisor-$Version-windows-$Arch.msi"
$platform = if ($Arch -eq 'amd64') { 'x64' } else { 'arm64' }
Push-Location $PSScriptRoot
try {
    wix build -arch $platform -d "Version=$Version" -d "Bin=$bin" pydantic-opamp-supervisor.wxs -o $msi
} finally {
    Pop-Location
}
Sign @($msi)
Get-Item $msi
