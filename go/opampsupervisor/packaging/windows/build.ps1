# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
#
# Builds the Windows installer into packaging\dist, on Windows: the bundled collector (ocb,
# ..\collector.yaml), the Supervisor, then an .msi (pydantic-opamp-supervisor.wxs, WiX 5):
#   go\opampsupervisor\packaging\windows\build.ps1 -Version 0.1.0
# The MSI version is numbers only (major.minor.build). With -CertificateThumbprint, signtool
# signs both executables and the .msi with that certificate from the machine's store.
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
        signtool sign /sha1 $CertificateThumbprint /fd sha256 /tr http://timestamp.digicert.com /td sha256 @paths
    }
}

Push-Location (Split-Path -Parent $packaging)
try {
    go run go.opentelemetry.io/collector/cmd/builder@v0.163.0 --config packaging/collector.yaml
    Copy-Item packaging\dist\otelcol\otelcol.exe (Join-Path $bin 'otelcol.exe') -Force
    go build -trimpath -o (Join-Path $bin 'opampsupervisor.exe') .
} finally {
    Pop-Location
}
Sign @((Join-Path $bin 'opampsupervisor.exe'), (Join-Path $bin 'otelcol.exe'))

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
