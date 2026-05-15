$ErrorActionPreference = "Stop"

# Force UTF-8 output so Unicode (Braille spinner, ✓ ✗ ⚠) survives in tab titles
# and console output. Default on Windows PS 5.1 is the OEM codepage, which
# replaces unsupported chars with '?'.
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new()
$OutputEncoding           = [System.Text.UTF8Encoding]::new()

# --- Single-instance guard ---
# Named mutex prevents two deploy.ps1 watch loops from racing on rsync / docker /
# port forwarding. The OS releases the mutex automatically when the holding
# process exits, so no stale-lock cleanup is needed.
$script:DeployMutex = New-Object System.Threading.Mutex($false, "Global\redmemo-deploy-ps1")
if (-not $script:DeployMutex.WaitOne(0)) {
    Write-Host "[deploy] Another deploy.ps1 instance is already running — exiting." -ForegroundColor Yellow
    Write-Host "[deploy] Switch to that window (use Ctrl+R there to redeploy)." -ForegroundColor Yellow
    Start-Sleep -Seconds 3
    exit 0
}

$WinDir  = $PSScriptRoot
$WslDist = "Debian"
$WslDir  = "~/redmemo"
$Ports   = @(8080, 8081)

function Log($msg)  { Write-Host "[deploy] $msg" -ForegroundColor Green }
function Warn($msg) { Write-Host "[deploy] $msg" -ForegroundColor Yellow }
function Err($msg)  { Write-Host "[deploy] $msg" -ForegroundColor Red }

# --- Tab title helpers (Windows Terminal / xterm OSC 0) ---
# These write directly to the console's stdout; the terminal consumes the
# escape sequence and updates the tab title — nothing is shown on screen.
function Set-TabTitle {
    param([string]$Title)
    $esc = [char]27; $bel = [char]7
    [Console]::Write("$esc]0;$Title$bel")
}

$script:SpinnerRunspace = $null
$script:SpinnerHandle   = $null
$script:SpinnerStop     = $null

function Start-TitleSpinner {
    param([string]$Label)
    # If a spinner is already running, replace it.
    Stop-TitleSpinner -FinalTitle $null
    $script:SpinnerStop = New-Object System.Threading.ManualResetEventSlim($false)
    $ps = [PowerShell]::Create()
    $null = $ps.AddScript({
        param($label, $stopEvent)
        $frames = @(
            [char]0x280B, [char]0x2819, [char]0x2839, [char]0x2838, [char]0x283C,
            [char]0x2834, [char]0x2826, [char]0x2827, [char]0x2807, [char]0x280F
        )
        $esc = [char]27; $bel = [char]7
        $i = 0
        while (-not $stopEvent.Wait(100)) {
            $f = $frames[$i % $frames.Length]
            [Console]::Write("$esc]0;$f $label$bel")
            $i++
        }
    }).AddArgument($Label).AddArgument($script:SpinnerStop)
    $script:SpinnerRunspace = $ps
    $script:SpinnerHandle   = $ps.BeginInvoke()
}

function Stop-TitleSpinner {
    param([string]$FinalTitle)
    if ($script:SpinnerStop) {
        $script:SpinnerStop.Set()
    }
    if ($script:SpinnerRunspace) {
        try { $null = $script:SpinnerRunspace.EndInvoke($script:SpinnerHandle) } catch {}
        $script:SpinnerRunspace.Dispose()
        $script:SpinnerRunspace = $null
        $script:SpinnerHandle   = $null
    }
    if ($script:SpinnerStop) {
        $script:SpinnerStop.Dispose()
        $script:SpinnerStop = $null
    }
    if ($FinalTitle) { Set-TabTitle $FinalTitle }
}

Set-TabTitle "redmemo: starting"

# --- 0. Spawn a single elevated helper that handles BOTH portproxy phases ---
# Why one helper instead of two elevated calls:
#   1) We must CLEAR stale portproxy before deploy.sh, so Docker (in WSL2's net
#      namespace under mirrored networking, or because of localhostForwarding)
#      can bind 8080/8081 without hitting "address already in use".
#   2) We must ADD the new portproxy AFTER deploy.sh succeeds, because we need
#      the (possibly new) WSL IP, and adding before Docker is up can race the
#      bind under mirrored mode.
# Doing both phases in one long-running elevated process means only ONE UAC.
# The helper clears immediately, blocks on a signal file, then reads the WSL IP
# from a file and adds the new rules. -EncodedCommand sidesteps all argument
# quoting headaches.

$ipcDir     = Join-Path $env:TEMP "redmemo-portproxy-$PID"
$signalFile = Join-Path $ipcDir "go"
$ipFile     = Join-Path $ipcDir "wslip"
$doneFile   = Join-Path $ipcDir "done"
Remove-Item $ipcDir -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Path $ipcDir -Force | Out-Null

$portsCsv = ($Ports -join ',')
$helperSrc = @"
`$ErrorActionPreference = 'Continue'
`$ports = '$portsCsv' -split ',' | ForEach-Object { [int]`$_ }
# Phase 1: clear stale rules so Docker can bind cleanly.
foreach (`$p in `$ports) {
    netsh interface portproxy delete v4tov4 listenport=`$p listenaddress=127.0.0.1 2>`$null | Out-Null
}
# Phase 2: wait for main script to write WSL IP and drop the go-signal.
`$deadline = (Get-Date).AddMinutes(10)
while (-not (Test-Path '$signalFile')) {
    if ((Get-Date) -gt `$deadline) { exit 2 }
    Start-Sleep -Milliseconds 200
}
`$wslIp = (Get-Content '$ipFile' -Raw).Trim()
# Phase 3: install new forwarding rules.
foreach (`$p in `$ports) {
    netsh interface portproxy delete v4tov4 listenport=`$p listenaddress=127.0.0.1 2>`$null | Out-Null
    netsh interface portproxy add v4tov4 listenport=`$p listenaddress=127.0.0.1 connectport=`$p connectaddress=`$wslIp | Out-Null
}
New-Item -ItemType File -Path '$doneFile' -Force | Out-Null
"@

$helperEncoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($helperSrc))

Log "Requesting elevation (single UAC for clear+forward)..."
$helperProc = Start-Process powershell `
    -Verb RunAs `
    -ArgumentList '-NoProfile','-WindowStyle','Hidden','-EncodedCommand',$helperEncoded `
    -PassThru
Log "Elevated portproxy helper started (PID $($helperProc.Id)); stale rules clearing now"

function Stop-PortproxyHelper {
    if ($script:helperProc -and -not $script:helperProc.HasExited) {
        try { $script:helperProc.Kill() } catch {}
    }
    Remove-Item $ipcDir -Recurse -Force -ErrorAction SilentlyContinue
}
$script:helperProc = $helperProc

# --- 1. Convert Windows path to WSL mount path ---
$drive = $WinDir.Substring(0, 1).ToLower()
$rest  = $WinDir.Substring(2) -replace '\\', '/'
$WslSrc = "/mnt/$drive$rest"

Log "Syncing: $WinDir -> $WslDist`:$WslDir"

# --- 2. Ensure target directory exists ---
wsl -d $WslDist -- mkdir -p $WslDir

# --- 3. Rsync from Windows mount to WSL home ---
$rsyncCmd = "rsync -a --delete --exclude='_redlib_ref/' --exclude='.git/' --exclude='bin/' --exclude='redmemo.log' --exclude='*.exe' --exclude='config.yaml' $WslSrc/ $WslDir/"

wsl -d $WslDist -- bash -c $rsyncCmd
if ($LASTEXITCODE -ne 0) {
    Err "rsync failed"
    Stop-PortproxyHelper
    exit 1
}
Log "Sync complete"

# --- 4. Ensure config.yaml exists in WSL ---
$hasConfig = wsl -d $WslDist -- bash -c "test -f $WslDir/config.yaml && echo yes || echo no"
if ($hasConfig.Trim() -eq "no") {
    Log "No config.yaml in WSL, copying from config.example.yaml..."
    wsl -d $WslDist -- bash -c "cp $WslDir/config.example.yaml $WslDir/config.yaml"
    Warn "Edit $WslDir/config.yaml in WSL if needed"
}

# --- 5. Run deploy.sh in WSL (first deploy, synchronous) ---
Log "Starting deployment in WSL..."
wsl -d $WslDist -- bash -c "chmod +x $WslDir/deploy.sh; cd $WslDir; ./deploy.sh --watch"
if ($LASTEXITCODE -ne 0) {
    Err "Deployment failed"
    Stop-PortproxyHelper
    exit 1
}

# Health check
$deployed = $false
for ($i = 0; $i -lt 30; $i++) {
    Start-Sleep -Seconds 2
    $check = wsl -d $WslDist -- bash -c "curl -sf http://127.0.0.1:8080/settings > /dev/null 2>&1 && echo ok || echo no"
    if ($check.Trim() -eq "ok") {
        Log "Deployment succeeded"
        $deployed = $true
        break
    }
}
if (-not $deployed) {
    Warn "Deployment health check timed out, continuing anyway"
}

# --- 6. Signal elevated helper to install portproxy rules ---
$wslIp = (wsl -d $WslDist -- hostname -I).Trim().Split(" ")[0]
Log "WSL IP: $wslIp, signaling elevated helper to install portproxy rules..."

Set-Content -Path $ipFile -Value $wslIp -NoNewline -Encoding ASCII
New-Item -ItemType File -Path $signalFile -Force | Out-Null

# Wait up to 30s for helper to finish; bail loudly if it hangs.
if (-not $helperProc.WaitForExit(30000)) {
    Warn "Portproxy helper still running after 30s — continuing anyway"
} elseif ($helperProc.ExitCode -ne 0) {
    Warn "Portproxy helper exited with code $($helperProc.ExitCode); port forwarding may be broken"
} elseif (Test-Path $doneFile) {
    Log "Port forwarding configured (WSL IP: $wslIp)"
} else {
    Warn "Portproxy helper finished without writing done-marker; verify with: netsh interface portproxy show v4tov4"
}

Remove-Item $ipcDir -Recurse -Force -ErrorAction SilentlyContinue

Log "Done. Access http://127.0.0.1:8080"

# --- Open default browser on first launch only ---
# Only fire if the initial deploy passed health check; skip on health timeout
# so we don't open a tab pointing at a broken instance.
if ($deployed) {
    try {
        Start-Process "http://127.0.0.1:8080"
        Log "Opened http://127.0.0.1:8080 in default browser"
    } catch {
        Warn "Could not open browser automatically: $_"
    }
}

# --- 7. Keep WSL alive in background ---
$wslKeepAlive = Start-Process -FilePath "wsl" -ArgumentList "-d", $WslDist, "--", "bash", "-c", "trap 'exit 0' TERM INT; while true; do sleep 3600; done" -PassThru -WindowStyle Hidden

# --- 8. Watch mode: Ctrl+R manual redeploy, Ctrl+C exit, CC turn-end auto-redeploy ---
$TurnMarker = Join-Path $WinDir ".cc-turn-done"
# Clear any stale marker from a previous run so we don't trigger immediately
if (Test-Path $TurnMarker) { Remove-Item $TurnMarker -Force -ErrorAction SilentlyContinue }

# Snapshot of newest mtime across build-relevant files. Used to decide whether
# a Claude Code turn end warrants a rebuild (skip rebuilds for doc/log-only changes).
$RelevantIncludes = @("*.go", "go.mod", "go.sum", "*.html", "*.tmpl", "*.css", "*.js", "*.svg", "Dockerfile*", "docker-compose.yml", "config.example.yaml")
$ExcludeRegex = '\\(\.git|_redlib_ref|bin|node_modules|\.claude)\\'

function Get-CodeMaxMtime {
    $files = Get-ChildItem -Path $WinDir -Recurse -File -Include $RelevantIncludes -ErrorAction SilentlyContinue |
             Where-Object { $_.FullName -notmatch $ExcludeRegex }
    if (-not $files) { return [DateTime]::MinValue }
    return ($files | Measure-Object -Property LastWriteTimeUtc -Maximum).Maximum
}

$LastBuiltMtime = Get-CodeMaxMtime

Set-TabTitle "redmemo: idle"
Log "Watch mode active | Ctrl+R to redeploy | Ctrl+C to exit | CC turn-end auto-redeploy armed"

function Invoke-Redeploy {
    param([string]$Reason)
    Log "=== Redeploying ($Reason) ==="
    Start-TitleSpinner "redmemo: rebuilding"

    # Rsync
    Log "Syncing files..."
    wsl -d $WslDist -- bash -c $rsyncCmd
    if ($LASTEXITCODE -ne 0) {
        Err "rsync failed, skipping this redeploy"
        Stop-TitleSpinner -FinalTitle "redmemo: $([char]0x2717) sync failed"
        return
    }
    Log "Sync complete"

    # Run deploy.sh --redeploy (skip infra, just rebuild + recreate redmemo)
    Log "Rebuilding in WSL..."
    wsl -d $WslDist -- bash -c "cd $WslDir; ./deploy.sh --redeploy"
    if ($LASTEXITCODE -ne 0) {
        Err "Redeploy failed"
        Stop-TitleSpinner -FinalTitle "redmemo: $([char]0x2717) rebuild failed"
        return
    }

    # Health check
    for ($i = 0; $i -lt 10; $i++) {
        Start-Sleep -Seconds 1
        $check = wsl -d $WslDist -- bash -c "curl -sf http://127.0.0.1:8080/settings > /dev/null 2>&1 && echo ok || echo no"
        if ($check.Trim() -eq "ok") {
            Log "Redeploy succeeded!"
            Stop-TitleSpinner -FinalTitle "redmemo: $([char]0x2713) idle"
            return
        }
    }
    Warn "Health check pending after redeploy"
    Stop-TitleSpinner -FinalTitle "redmemo: $([char]0x26A0) unhealthy"
}

[Console]::TreatControlCAsInput = $true

try {
    while ($true) {
        if ([Console]::KeyAvailable) {
            $key = [Console]::ReadKey($true)
            if ($key.Modifiers -band [ConsoleModifiers]::Control -and $key.Key -eq 'C') {
                break
            }
            if ($key.Modifiers -band [ConsoleModifiers]::Control -and $key.Key -eq 'R') {
                Invoke-Redeploy -Reason "manual Ctrl+R"
                $LastBuiltMtime = Get-CodeMaxMtime
                Log "Watch mode active | Ctrl+R to redeploy | Ctrl+C to exit | CC turn-end auto-redeploy armed"
            }
        }

        # CC turn-end auto-redeploy: marker file is touched by the project Stop hook.
        if (Test-Path $TurnMarker) {
            # Debounce: wait a moment for any trailing file writes from the last tool calls
            Start-Sleep -Milliseconds 1500
            Remove-Item $TurnMarker -Force -ErrorAction SilentlyContinue
            $currentMtime = Get-CodeMaxMtime
            if ($currentMtime -gt $LastBuiltMtime) {
                Invoke-Redeploy -Reason "CC turn end + code change detected"
                $LastBuiltMtime = Get-CodeMaxMtime
            } else {
                Log "CC turn ended; no build-relevant changes since last deploy, skipping"
            }
            Log "Watch mode active | Ctrl+R to redeploy | Ctrl+C to exit | CC turn-end auto-redeploy armed"
        }

        Start-Sleep -Milliseconds 200
    }
} finally {
    [Console]::TreatControlCAsInput = $false
    Stop-TitleSpinner -FinalTitle "redmemo: exited"
    Stop-PortproxyHelper
    if ($wslKeepAlive -and -not $wslKeepAlive.HasExited) {
        $wslKeepAlive.Kill()
    }
    Log "Exiting watch mode"
}
