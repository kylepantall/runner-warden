package winvm

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// psq quotes s as a PowerShell single-quoted literal.
func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// winJoin joins Windows path elements with backslashes.
func winJoin(elem ...string) string {
	out := strings.TrimRight(elem[0], `\/`)
	for _, e := range elem[1:] {
		out += `\` + strings.Trim(e, `\/`)
	}
	return out
}

// TaskName is the Scheduled Task that runs one runner.
func TaskName(runner string) string { return "rw-" + runner }

const psPrelude = "$ErrorActionPreference='Stop'\n$ProgressPreference='SilentlyContinue'\n"

// runnerZipURL is the official release asset for a runner version.
func runnerZipURL(version string) string {
	return "https://github.com/actions/runner/releases/download/v" + version + "/actions-runner-win-x64-" + version + ".zip"
}

// ensureRunnerScript installs or upgrades the runner files in the guest.
func ensureRunnerScript(runnerDir, version string) string {
	return psPrelude + `[Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12
$dir=` + psq(runnerDir) + `; $ver=` + psq(version) + `
$exe=Join-Path $dir 'bin\Runner.Listener.exe'
$have=''
if (Test-Path $exe) { $have=(Get-Item $exe).VersionInfo.FileVersion }
if ((Test-Path (Join-Path $dir 'run.cmd')) -and $have -and ($have -like "$ver*")) { 'runner=current'; exit 0 }
$zip=Join-Path $env:TEMP "actions-runner-$ver.zip"
$url=` + psq(runnerZipURL(version)) + `
for ($i=1; $i -le 3; $i++) { try { Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $zip; break } catch { if ($i -eq 3) { throw }; Start-Sleep 5 } }
New-Item -ItemType Directory -Force -Path $dir | Out-Null
Expand-Archive -LiteralPath $zip -DestinationPath $dir -Force
Remove-Item $zip -Force -ErrorAction SilentlyContinue
'runner=installed'
`
}

// jobPaths are the guest paths of one runner's job.
type jobPaths struct {
	Runner    string // runner name
	RunnerDir string // actions runner install
	WorkRoot  string // windows.work_dir
}

func (j jobPaths) Dir() string        { return winJoin(j.WorkRoot, j.Runner) }
func (j jobPaths) WorkFolder() string { return winJoin(j.Dir(), "_work") }
func (j jobPaths) Cfg() string        { return winJoin(j.Dir(), "jit.cfg") }
func (j jobPaths) Cmd() string        { return winJoin(j.Dir(), "run-task.cmd") }
func (j jobPaths) Log() string        { return winJoin(j.Dir(), "runner.log") }
func (j jobPaths) Diag() string       { return winJoin(j.RunnerDir, "_diag") }
func (j jobPaths) Task() string       { return TaskName(j.Runner) }

// taskCmd is run-task.cmd. The JIT config is read from its file into the
// runner's own input variable (no command-line length limit, nothing in a
// process listing) and the file is deleted before the runner starts.
func (j jobPaths) taskCmd() string {
	return strings.Join([]string{
		"@echo off",
		"setlocal DisableDelayedExpansion",
		`for /f "usebackq delims=" %%a in ("` + j.Cfg() + `") do set "ACTIONS_RUNNER_INPUT_JITCONFIG=%%a"`,
		`del /f /q "` + j.Cfg() + `"`,
		`cd /d "` + j.RunnerDir + `"`,
		`call "` + winJoin(j.RunnerDir, "run.cmd") + `" > "` + j.Log() + `" 2>&1`,
	}, "\n")
}

// killSnippet ends the task and kills the runner's process trees. Matches the
// runner's Listener/Worker under the runner dir, and any process whose command
// line mentions the job dir. $grace seconds are given before the force kill.
func (j jobPaths) killSnippet(graceSecs int) string {
	return `$tn=` + psq(j.Task()) + `; $jd=` + psq(j.Dir()) + `; $rd=` + psq(j.RunnerDir) + `
Stop-ScheduledTask -TaskName $tn -ErrorAction SilentlyContinue
function Get-RwProcs {
  Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne $PID -and (
    ($_.CommandLine -and $_.CommandLine.IndexOf($jd,[StringComparison]::OrdinalIgnoreCase) -ge 0) -or
    ($_.ExecutablePath -and $_.ExecutablePath.StartsWith($rd,[StringComparison]::OrdinalIgnoreCase) -and @('Runner.Listener.exe','Runner.Worker.exe') -contains $_.Name)) }
}
for ($i=0; $i -lt ` + strconv.Itoa(graceSecs) + `; $i++) { if (-not @(Get-RwProcs).Count) { break }; Start-Sleep 1 }
foreach ($p in @(Get-RwProcs)) { & taskkill.exe /T /F /PID $p.ProcessId 2>&1 | Out-Null }
`
}

// startJobScript prepares the job dir, writes the JIT config (from stdin) and
// starts the one-shot Scheduled Task. The runner is not an SSH child.
func startJobScript(j jobPaths, logon string) string {
	return psPrelude + `$name=` + psq(j.Runner) + `; $dir=` + psq(j.Dir()) + `; $root=` + psq(j.WorkRoot) + `; $rd=` + psq(j.RunnerDir) + `
# sweep leftovers of earlier jobs (one job per VM)
Get-ScheduledTask -TaskName 'rw-*' -ErrorAction SilentlyContinue | Where-Object { $_.TaskName -ne (` + psq(j.Task()) + `) } | ForEach-Object { Stop-ScheduledTask -InputObject $_ -ErrorAction SilentlyContinue; Unregister-ScheduledTask -InputObject $_ -Confirm:$false -ErrorAction SilentlyContinue }
foreach ($p in @(Get-Process -Name 'Runner.Listener','Runner.Worker' -ErrorAction SilentlyContinue)) { & taskkill.exe /T /F /PID $p.Id 2>&1 | Out-Null }
if (Test-Path $root) { Get-ChildItem $root -Directory -ErrorAction SilentlyContinue | Where-Object { $_.Name -ne $name -and (Test-Path (Join-Path $_.FullName '.rw-job')) } | ForEach-Object { Remove-Item $_.FullName -Recurse -Force -ErrorAction SilentlyContinue } }
foreach ($f in '.runner','.credentials','.credentials_rsaparams') { Remove-Item (Join-Path $rd $f) -Force -ErrorAction SilentlyContinue }
New-Item -ItemType Directory -Force -Path ` + psq(j.WorkFolder()) + ` | Out-Null
New-Item -ItemType File -Force -Path (Join-Path $dir '.rw-job') | Out-Null
$cfg=[Console]::In.ReadToEnd().Trim()
if (-not $cfg) { throw 'empty jit config' }
$cf=` + psq(j.Cfg()) + `
[IO.File]::WriteAllText($cf, $cfg)
& icacls.exe $cf /inheritance:r /grant:r ("$($env:USERNAME):(F)") 'SYSTEM:(F)' | Out-Null
$cmd=@'
` + j.taskCmd() + `
'@
[IO.File]::WriteAllText(` + psq(j.Cmd()) + `, ([string]::Join([Environment]::NewLine, ($cmd -split "\r?\n")) + [Environment]::NewLine))
$a=New-ScheduledTaskAction -Execute 'cmd.exe' -Argument ('/d /c "' + ` + psq(j.Cmd()) + ` + '"')
$p=New-ScheduledTaskPrincipal -UserId ([System.Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType ` + logon + ` -RunLevel Highest
$s=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -MultipleInstances IgnoreNew -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName ` + psq(j.Task()) + ` -Action $a -Principal $p -Settings $s -Force | Out-Null
Start-ScheduledTask -TaskName ` + psq(j.Task()) + `
'started'
`
}

// stopScript ends the runner's task and process trees.
func stopScript(j jobPaths, graceSecs int) string {
	return "$ErrorActionPreference='Continue'\n" + j.killSnippet(graceSecs) + "'stopped'\n"
}

// cleanupScript is idempotent: it removes everything the runner left.
func cleanupScript(j jobPaths) string {
	return "$ErrorActionPreference='Continue'\n" + j.killSnippet(0) + `Unregister-ScheduledTask -TaskName $tn -Confirm:$false -ErrorAction SilentlyContinue
for ($i=0; $i -lt 5 -and (Test-Path $jd); $i++) { Remove-Item $jd -Recurse -Force -ErrorAction SilentlyContinue; if (Test-Path $jd) { & cmd.exe /d /c ('rd /s /q "' + $jd + '"') 2>&1 | Out-Null }; if (Test-Path $jd) { Start-Sleep 2 } }
Get-ChildItem $env:TEMP -Force -ErrorAction SilentlyContinue | Remove-Item -Recurse -Force -ErrorAction SilentlyContinue
$cut=(Get-Date).AddDays(-7)
Get-ChildItem (Join-Path $rd '_diag') -File -ErrorAction SilentlyContinue | Where-Object { $_.LastWriteTime -lt $cut } | Remove-Item -Force -ErrorAction SilentlyContinue
if (Test-Path $jd) { 'leftover=1' } else { 'leftover=0' }
`
}

// statsScript prints "state=<task state> procs=<n> log=<bytes> diag=<bytes>".
func statsScript(j jobPaths) string {
	return `$ErrorActionPreference='SilentlyContinue'
$t=Get-ScheduledTask -TaskName ` + psq(j.Task()) + `
$state='Missing'; if ($t) { $state=[string]$t.State }
$procs=@(Get-Process -Name 'Runner.Listener','Runner.Worker').Count
$log=0; $i=Get-Item ` + psq(j.Log()) + `; if ($i) { $log=$i.Length }
$diag=0; $m=Get-ChildItem ` + psq(j.Diag()) + ` -File | Measure-Object Length -Sum; if ($m.Sum) { $diag=$m.Sum }
"state=$state procs=$procs log=$log diag=$diag"
`
}

// guestStats is the parsed output of statsScript.
type guestStats struct {
	State string
	Procs int
	Log   int64
	Diag  int64
}

func (g guestStats) Alive() bool { return g.State == "Running" || g.Procs > 0 }

func parseGuestStats(out string) (guestStats, error) {
	var g guestStats
	seen := false
	for _, f := range strings.Fields(out) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "state":
			g.State, seen = v, true
		case "procs":
			if n > 0 && n <= math.MaxInt32 {
				g.Procs = int(n)
			}
		case "log":
			g.Log = n
		case "diag":
			g.Diag = n
		}
	}
	if !seen {
		return g, fmt.Errorf("unparseable guest stats")
	}
	return g, nil
}

// installKeyScript adds a public key to administrators_authorized_keys with
// the ACL sshd requires, and makes PowerShell the default ssh shell.
func installKeyScript(pub string) string {
	return psPrelude + `$f='C:\ProgramData\ssh\administrators_authorized_keys'
$k=` + psq(strings.TrimSpace(pub)) + `
New-Item -ItemType Directory -Force -Path (Split-Path $f) | Out-Null
$have=@(); if (Test-Path $f) { $have=@(Get-Content $f) }
if ($have -notcontains $k) { Add-Content -Path $f -Value $k -Encoding ascii }
& icacls.exe $f /inheritance:r /grant 'Administrators:F' /grant 'SYSTEM:F' | Out-Null
New-ItemProperty -Path 'HKLM:\SOFTWARE\OpenSSH' -Name DefaultShell -Value 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' -PropertyType String -Force | Out-Null
'key=installed'
`
}

// disableRunnerServicesScript stops and disables actions.runner.* services
// without deleting their files, and prints their names (one per line).
const disableRunnerServicesScript = `$ErrorActionPreference='Continue'
foreach ($s in @(Get-Service -Name 'actions.runner.*' -ErrorAction SilentlyContinue)) {
  Stop-Service -InputObject $s -Force -ErrorAction SilentlyContinue
  Set-Service -Name $s.Name -StartupType Disabled
  "service=$($s.Name)"
}
`
