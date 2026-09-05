# 重新生成 docs/system-config-report.md 硬件基线报告
# 用法: pwsh -NoProfile -File scripts/gen-system-report.ps1
$ErrorActionPreference = 'Continue'
$out = Join-Path $PSScriptRoot '..\docs\system-config-report.md'
$ts = Get-Date -Format 'yyyy-MM-dd HH:mm:ss zzz'

$cs = Get-CimInstance Win32_ComputerSystem
$os = Get-CimInstance Win32_OperatingSystem
$cpu = Get-CimInstance Win32_Processor
$bios = Get-CimInstance Win32_BIOS

$sb = [System.Text.StringBuilder]::new()
[void]$sb.AppendLine('# System Configuration Report')
[void]$sb.AppendLine('')
[void]$sb.AppendLine("Generated: $ts")
[void]$sb.AppendLine('')
[void]$sb.AppendLine('## Host')
[void]$sb.AppendLine("- Hostname: $($env:COMPUTERNAME)")
[void]$sb.AppendLine("- Manufacturer/Model: $($cs.Manufacturer) / $($cs.Model)")
[void]$sb.AppendLine("- OS: $($os.Caption) $($os.Version) (Build $($os.BuildNumber), $($os.OSArchitecture))")
[void]$sb.AppendLine("- Last Boot: $($os.LastBootUpTime)")
[void]$sb.AppendLine("- PowerShell: $($PSVersionTable.PSVersion)")
[void]$sb.AppendLine('')
[void]$sb.AppendLine('## CPU & Memory')
[void]$sb.AppendLine("- CPU: $($cpu.Name.Trim())")
[void]$sb.AppendLine("- Cores / Logical: $($cpu.NumberOfCores) / $($cpu.NumberOfLogicalProcessors)")
[void]$sb.AppendLine("- Max Clock: $($cpu.MaxClockSpeed) MHz")
[void]$sb.AppendLine("- Physical Memory: $([math]::Round($cs.TotalPhysicalMemory/1GB,2)) GB")
[void]$sb.AppendLine('')
[void]$sb.AppendLine('## GPU')
$smi = (Get-Command nvidia-smi -ErrorAction SilentlyContinue).Source
if ($smi) {
    [void]$sb.AppendLine("- nvidia-smi path: $smi")
    $header = (& nvidia-smi | Select-Object -First 1).Trim()
    [void]$sb.AppendLine("- NVIDIA-SMI header: $header")
    [void]$sb.AppendLine('- NVIDIA GPU detail (name,driver,memory,power,pstate,temp):')
    $gpus = & nvidia-smi --query-gpu=name,driver_version,memory.total,power.limit,pstate,temperature.gpu --format=csv,noheader
    foreach ($g in $gpus) { [void]$sb.AppendLine("  - $($g.Trim())") }
} else {
    [void]$sb.AppendLine('- nvidia-smi not found')
}
[void]$sb.AppendLine('- WMI Video Controllers:')
foreach ($v in (Get-CimInstance Win32_VideoController)) {
    [void]$sb.AppendLine("  - $($v.Name) | Driver $($v.DriverVersion) | VRAM $([math]::Round($v.AdapterRAM/1GB))GB")
}
[void]$sb.AppendLine('')
[void]$sb.AppendLine('## Storage')
foreach ($d in (Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3')) {
    [void]$sb.AppendLine("- $($d.DeviceID) [$($d.FileSystem)] Total: $([math]::Round($d.Size/1GB,2))GB, Free: $([math]::Round($d.FreeSpace/1GB,2))GB")
}
[void]$sb.AppendLine('')
[void]$sb.AppendLine('## BIOS')
[void]$sb.AppendLine("- Version: $($bios.SMBIOSBIOSVersion)")
[void]$sb.AppendLine("- Release Date: $($bios.ReleaseDate)")
[void]$sb.AppendLine('')
[void]$sb.AppendLine('## Dev Toolchain')
# go 用 version 子命令，其余用 --version
$verCmds = @{ 'go' = @('version'); 'node' = @('--version'); 'pnpm' = @('--version'); 'git' = @('--version') }
foreach ($cmd in @('go','node','pnpm','git')) {
    $p = (Get-Command $cmd -ErrorAction SilentlyContinue).Source
    if ($p) {
        $ver = (& $cmd @($verCmds[$cmd]) 2>&1 | Select-Object -First 1).ToString()
        [void]$sb.AppendLine("- ${cmd}: $ver")
    } else {
        [void]$sb.AppendLine("- ${cmd}: not found")
    }
}
[void]$sb.AppendLine('')
[void]$sb.AppendLine('## Reuse Guidance for Copilot')
[void]$sb.AppendLine('- Purpose: This file is the baseline machine profile used when diagnosing performance, build, GPU/runtime mismatch, and environment-specific bugs.')
[void]$sb.AppendLine('- How to refresh: rerun the same reporting command and overwrite this file.')
[void]$sb.AppendLine('- How to use in future tasks: reference this file first, then only recollect deltas (e.g., driver updates, new SDK versions).')

[System.IO.File]::WriteAllText($out, $sb.ToString(), [System.Text.UTF8Encoding]::new($false))
Get-Content $out
