# runner-warden baseline: what the Actions runner and common workflows need on
# Windows (Git + bash, jq, PowerShell 7). Idempotent; every step skips when
# present. Project toolchains (Visual Studio, SDKs, installers) belong in the
# script passed to `runner-warden windows provision --script`.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$tmp = Join-Path $env:TEMP 'rw-baseline'; New-Item -ItemType Directory -Force -Path $tmp | Out-Null
function Get-File($url, $name) {
  if (-not $url) { throw "no download url for $name" }
  $f = Join-Path $tmp $name
  for ($i = 1; $i -le 3; $i++) { try { Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $f; return $f } catch { if ($i -eq 3) { throw }; Start-Sleep 5 } }
}
function Get-Asset($repo, $pattern) {
  $r = Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$repo/releases/latest"
  $a = $r.assets | Where-Object { $_.name -like $pattern } | Select-Object -First 1
  if (-not $a) { throw "no asset matching $pattern in $repo $($r.tag_name)" }
  Get-File $a.browser_download_url $a.name
}
function Add-MachinePath($p) {
  $cur = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  if (($cur -split ';') -notcontains $p) { [Environment]::SetEnvironmentVariable('Path', "$cur;$p", 'Machine') }
}
# Long paths and symlinks (Developer Mode) are needed by many toolchains.
Set-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' LongPathsEnabled 1
New-Item -Force 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock' | Out-Null
Set-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock' AllowDevelopmentWithoutDevLicense 1

$git = 'C:\Program Files\Git'
if (-not (Test-Path "$git\bin\git.exe")) {
  $f = Get-Asset 'git-for-windows/git' 'Git-*-64-bit.exe'
  Start-Process $f -ArgumentList '/VERYSILENT', '/NORESTART', '/NOCANCEL', '/SP-', '/SUPPRESSMSGBOXES' -Wait
}
Add-MachinePath "$git\bin"
if (-not (Test-Path 'C:\tools\bin\jq.exe')) {
  New-Item -ItemType Directory -Force -Path 'C:\tools\bin' | Out-Null
  Copy-Item (Get-File 'https://github.com/jqlang/jq/releases/latest/download/jq-windows-amd64.exe' 'jq.exe') 'C:\tools\bin\jq.exe'
}
Add-MachinePath 'C:\tools\bin'
$pwsh = 'C:\Program Files\PowerShell\7'
if (-not (Test-Path "$pwsh\pwsh.exe")) {
  $f = Get-Asset 'PowerShell/PowerShell' 'PowerShell-*-win-x64.zip'
  New-Item -ItemType Directory -Force -Path $pwsh | Out-Null
  Expand-Archive -Path $f -DestinationPath $pwsh -Force
}
Add-MachinePath $pwsh
Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
'baseline=done'
