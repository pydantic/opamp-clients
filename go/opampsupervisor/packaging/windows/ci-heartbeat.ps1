# Copyright (c) 2026 Pydantic Services Inc.
# SPDX-License-Identifier: MIT
# CI only: what the Windows runner looks like every 30 s, written to a check run of its own. When a
# step stops the runner from answering, the job ends with no log at all; this process runs apart
# from the runner, so its last report shows what the machine was doing then. Stops when -Stop
# exists, or after -Minutes.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Repo,
    [Parameter(Mandatory)][string]$Sha,
    [Parameter(Mandatory)][string]$Name,
    [Parameter(Mandatory)][string]$Log,
    [Parameter(Mandatory)][string]$Stop,
    [int]$Minutes = 40
)
$ErrorActionPreference = 'Continue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$api = "https://api.github.com/repos/$Repo/check-runs"
$headers = @{ Authorization = "Bearer $env:HEARTBEAT_TOKEN"; Accept = 'application/vnd.github+json' }
[Diagnostics.Process]::GetCurrentProcess().PriorityClass = 'High'

function Report {
    $os = Get-CimInstance Win32_OperatingSystem
    $procs = @(Get-CimInstance Win32_Process)
    $lines = @(
        "now $((Get-Date).ToUniversalTime().ToString('o'))  booted $($os.LastBootUpTime.ToUniversalTime().ToString('o'))"
        "memory free $([int]($os.FreePhysicalMemory / 1024)) MB of $([int]($os.TotalVisibleMemorySize / 1024)) MB; $($procs.Count) processes"
        (Get-PSDrive -PSProvider FileSystem | ForEach-Object { "drive $($_.Name): $([int]($_.Free / 1MB)) MB free" })
        "CPU load $((Get-CimInstance Win32_Processor | Measure-Object -Average LoadPercentage).Average)%"
        ''
        '## Ours, msiexec, PowerShell, the runner'
        ($procs | Where-Object { $_.Name -match 'opamp|otelcol|msiexec|install\.test|powershell|pwsh|Runner|bash|conhost' } |
            Sort-Object ProcessId | ForEach-Object { "$($_.ProcessId) <- $($_.ParentProcessId) $($_.Name) $([int]($_.WorkingSetSize / 1MB)) MB, CPU $([int](($_.KernelModeTime + $_.UserModeTime) / 1e7)) s: $($_.CommandLine)" })
        ''
        '## Most memory'
        ($procs | Sort-Object WorkingSetSize -Descending | Select-Object -First 10 |
            ForEach-Object { "$($_.ProcessId) $($_.Name) $([int]($_.WorkingSetSize / 1MB)) MB" })
        ''
        '## Services'
        (Get-Service pydantic-opamp-supervisor, msiserver -ErrorAction SilentlyContinue | ForEach-Object { "$($_.Name) $($_.Status)" })
        ''
        '## Defender'
        (Get-MpComputerStatus -ErrorAction SilentlyContinue | ForEach-Object { "real-time $($_.RealTimeProtectionEnabled), on access $($_.OnAccessProtectionEnabled), behavior $($_.BehaviorMonitorEnabled), network inspection $($_.NISEnabled)" })
        (Get-MpThreatDetection -ErrorAction SilentlyContinue | ForEach-Object { "detected $($_.InitialDetectionTime.ToUniversalTime().ToString('HH:mm:ss')) threat $($_.ThreatID) action $($_.ActionSuccess): $($_.Resources -join ', ')" })
        (Get-MpThreat -ErrorAction SilentlyContinue | ForEach-Object { "threat $($_.ThreatID) $($_.ThreatName) severity $($_.SeverityID)" })
        (Get-WinEvent -LogName 'Microsoft-Windows-Windows Defender/Operational' -MaxEvents 10 -ErrorAction SilentlyContinue |
            Sort-Object TimeCreated | ForEach-Object { "$($_.TimeCreated.ToUniversalTime().ToString('HH:mm:ss')) $($_.Id) $(($_.Message -split "`n")[0..6] -join ' | ')" })
        ''
        '## The Supervisor, last 15 events'
        (Get-WinEvent -FilterHashtable @{LogName = 'Application'; ProviderName = 'pydantic-opamp-supervisor'} -MaxEvents 15 -ErrorAction SilentlyContinue |
            Sort-Object TimeCreated | ForEach-Object { "$($_.TimeCreated.ToUniversalTime().ToString('HH:mm:ss')) $($_.Message)" })
        ''
        '## The test, last 80 lines'
        (Get-Content -Tail 80 -LiteralPath $Log -ErrorAction SilentlyContinue)
    )
    $text = ($lines | Out-String)
    if ($text.Length -gt 60000) { $text = $text.Substring($text.Length - 60000) }
    $text
}

function Post([string]$status, [string]$summary, [string]$text, [string]$conclusion) {
    $fence = '```'
    $body = @{ status = $status; output = @{ title = $summary; summary = $summary; text = $fence + "`n" + $text + "`n" + $fence } }
    if ($conclusion) { $body.conclusion = $conclusion }
    $json = $body | ConvertTo-Json -Depth 5
    try {
        if ($script:id) {
            Invoke-RestMethod -Method Patch -Uri "$api/$($script:id)" -Headers $headers -Body $json -ContentType 'application/json' | Out-Null
        } else {
            $body.name = $Name; $body.head_sha = $Sha
            $script:id = (Invoke-RestMethod -Method Post -Uri $api -Headers $headers -Body ($body | ConvertTo-Json -Depth 5) -ContentType 'application/json').id
        }
    } catch { }
}

$script:id = $null
$deadline = (Get-Date).AddMinutes($Minutes)
while ((Get-Date) -lt $deadline -and -not (Test-Path -LiteralPath $Stop)) {
    Post 'in_progress' "alive at $((Get-Date).ToUniversalTime().ToString('HH:mm:ss')) UTC" (Report) ''
    Start-Sleep -Seconds 30
}
$finished = Test-Path -LiteralPath $Stop
Post 'completed' $(if ($finished) { 'the job finished' } else { "the job didn't finish in $Minutes minutes" }) (Report) $(if ($finished) { 'success' } else { 'failure' })
