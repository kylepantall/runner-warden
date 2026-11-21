# runner-warden guest setup: sshd, key auth, update/sleep hardening.
$ErrorActionPreference = 'Continue'
$log = 'C:\rw-install.log'
function Step($n, [scriptblock]$b) { try { & $b; "ok: $n" | Add-Content $log } catch { "fail: $n $_" | Add-Content $log } }
$pub = (Get-Content (Join-Path $PSScriptRoot 'authorized_key.pub') -Raw).Trim()

# OpenSSH server (Feature on Demand needs network; retry).
Step 'openssh-capability' {
  for ($i = 1; $i -le 5; $i++) {
    Add-WindowsCapability -Online -Name 'OpenSSH.Server~~~~0.0.1.0' -ErrorAction Stop | Out-Null
    if (Get-Service sshd -ErrorAction SilentlyContinue) { break }
    Start-Sleep 20
  }
}
Step 'sshd-service' { Set-Service -Name sshd -StartupType Automatic; Start-Service sshd }
Step 'firewall-22' {
  if (-not (Get-NetFirewallRule -Name 'rw-sshd' -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name 'rw-sshd' -DisplayName 'OpenSSH (runner-warden)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null
  }
}
Step 'authorized-keys' {
  $f = 'C:\ProgramData\ssh\administrators_authorized_keys'
  New-Item -ItemType Directory -Force -Path (Split-Path $f) | Out-Null
  Set-Content -Path $f -Value $pub -Encoding ascii
  icacls.exe $f /inheritance:r /grant 'Administrators:F' /grant 'SYSTEM:F' | Out-Null
}
Step 'default-shell' {
  New-ItemProperty -Path 'HKLM:\SOFTWARE\OpenSSH' -Name DefaultShell -Value 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' -PropertyType String -Force | Out-Null
}
Restart-Service sshd -ErrorAction SilentlyContinue

# Windows Update off (services + policy).
Step 'windows-update' {
  foreach ($s in 'wuauserv', 'UsoSvc', 'WaaSMedicSvc', 'DoSvc') {
    Stop-Service $s -Force -ErrorAction SilentlyContinue
    Set-Service $s -StartupType Disabled -ErrorAction SilentlyContinue
    Set-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Services\$s" -Name Start -Value 4 -ErrorAction SilentlyContinue
  }
  $k = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU'
  New-Item -Path $k -Force | Out-Null
  Set-ItemProperty $k -Name NoAutoUpdate -Value 1 -Type DWord
  Set-ItemProperty $k -Name AUOptions -Value 1 -Type DWord
}
# Defender scheduled scans off.
Step 'defender-scans' {
  Set-MpPreference -DisableCatchupFullScan $true -DisableCatchupQuickScan $true -ScanScheduleDay 8
  Get-ScheduledTask -TaskPath '\Microsoft\Windows\Windows Defender\' -ErrorAction SilentlyContinue | Disable-ScheduledTask | Out-Null
}
# No sleep or hibernate.
Step 'power' {
  powercfg.exe /hibernate off
  foreach ($o in 'standby-timeout-ac', 'standby-timeout-dc', 'monitor-timeout-ac', 'disk-timeout-ac', 'hibernate-timeout-ac') { powercfg.exe /change $o 0 }
}
# Long paths and Developer Mode.
Step 'long-paths' { Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' -Name LongPathsEnabled -Value 1 -Type DWord }
Step 'developer-mode' {
  $k = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock'
  New-Item -Path $k -Force | Out-Null
  Set-ItemProperty $k -Name AllowDevelopmentWithoutDevLicense -Value 1 -Type DWord
  Set-ItemProperty $k -Name AllowAllTrustedApps -Value 1 -Type DWord
}
'done' | Set-Content 'C:\rw-install.done'
