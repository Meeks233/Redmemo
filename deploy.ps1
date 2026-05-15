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

# --- 6. Set up Windows portproxy (WSL2 Docker ports -> Windows localhost) ---
# Single elevated invocation: delete any stale rule for each port, then add a
# fresh rule pointing at the current WSL IP. The delete is idempotent (errors
# swallowed by `2>$null`), so we don't need a separate pre-check pass.
$wslIp = (wsl -d $WslDist -- hostname -I).Trim().Split(" ")[0]
Log "WSL IP: $wslIp, configuring port forwarding (UAC prompt may appear)..."

$cmds = ($Ports | ForEach-Object {
    "netsh interface portproxy delete v4tov4 listenport=$_ listenaddress=127.0.0.1 2>`$null;"
    "netsh interface portproxy add v4tov4 listenport=$_ listenaddress=127.0.0.1 connectport=$_ connectaddress=$wslIp"
}) -join "; "

Start-Process powershell -Verb RunAs -ArgumentList "-Command", $cmds -Wait
Log "Port forwarding configured (WSL IP: $wslIp)"

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
    if ($wslKeepAlive -and -not $wslKeepAlive.HasExited) {
        $wslKeepAlive.Kill()
    }
    Log "Exiting watch mode"
}
