# runner-warden baseline toolchain. Idempotent; every step skips when present.
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$tmp = Join-Path $env:TEMP 'rw-baseline'; New-Item -ItemType Directory -Force -Path $tmp | Out-Null
function Get-File($url, $name) {
  $f = Join-Path $tmp $name
  for ($i = 1; $i -le 3; $i++) { try { Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $f; return $f } catch { if ($i -eq 3) { throw }; Start-Sleep 5 } }
}
function Get-Asset($repo, $pattern) {
  $r = Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$repo/releases/latest"
  $a = $r.assets | Where-Object { $_.name -like $pattern } | Select-Object -First 1
  Get-File $a.browser_download_url $a.name
}
function Add-MachinePath($p) {
  $cur = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  if (($cur -split ';') -notcontains $p) { [Environment]::SetEnvironmentVariable('Path', "$cur;$p", 'Machine') }
}
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
if (-not (Test-Path 'C:\Program Files\PowerShell\7\pwsh.exe')) {
  $f = Get-Asset 'PowerShell/PowerShell' 'PowerShell-*-win-x64.msi'
  Start-Process msiexec.exe -ArgumentList '/i', "`"$f`"", '/qn', '/norestart' -Wait
}
if (-not (Test-Path 'C:\Program Files (x86)\Inno Setup 6\ISCC.exe')) {
  $f = Get-File 'https://jrsoftware.org/download.php/is.exe' 'is.exe'
  Start-Process $f -ArgumentList '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/SP-' -Wait
}
if (-not (Test-Path 'C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools')) {
  $f = Get-File 'https://aka.ms/vs/17/release/vs_BuildTools.exe' 'vs_BuildTools.exe'
  $p = Start-Process $f -ArgumentList '--quiet', '--wait', '--norestart', '--nocache', '--includeRecommended', '--add', 'Microsoft.VisualStudio.Workload.VCTools', '--add', 'Microsoft.VisualStudio.Component.VC.ATL' -Wait -PassThru
  if ($p.ExitCode -notin 0, 3010) { throw "vs_BuildTools exit $($p.ExitCode)" }
}
Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
'baseline=done'
