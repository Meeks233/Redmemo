$ErrorActionPreference = "Stop"

$WinDir  = $PSScriptRoot
$WslDist = "Debian"
$WslDir  = "~/redmemo"
$Ports   = @(8080, 8081)

function Log($msg)  { Write-Host "[deploy] $msg" -ForegroundColor Green }
function Warn($msg) { Write-Host "[deploy] $msg" -ForegroundColor Yellow }
function Err($msg)  { Write-Host "[deploy] $msg" -ForegroundColor Red }

# --- 0. Clear old portproxy rules to free ports before Docker binds them ---
Log "Clearing old port forwarding rules..."
$clearCmds = ($Ports | ForEach-Object {
    "netsh interface portproxy delete v4tov4 listenport=$_ listenaddress=127.0.0.1 2>`$null"
}) -join "; "
Start-Process powershell -Verb RunAs -ArgumentList "-Command", $clearCmds -Wait
Log "Old port forwarding rules cleared"

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
$wslIp = (wsl -d $WslDist -- hostname -I).Trim().Split(" ")[0]
Log "WSL IP: $wslIp, setting up port forwarding..."

$needsElevation = $false
foreach ($port in $Ports) {
    $existing = netsh interface portproxy show v4tov4 | Select-String ":$port\s"
    if (-not $existing) {
        $needsElevation = $true
        break
    }
}

if ($needsElevation) {
    $cmds = ($Ports | ForEach-Object {
        "netsh interface portproxy delete v4tov4 listenport=$_ listenaddress=127.0.0.1 2>`$null;"
        "netsh interface portproxy add v4tov4 listenport=$_ listenaddress=127.0.0.1 connectport=$_ connectaddress=$wslIp"
    }) -join "; "

    Start-Process powershell -Verb RunAs -ArgumentList "-Command", $cmds -Wait
    Log "Port forwarding configured"
} else {
    # Update connectaddress in case WSL IP changed
    $cmds = ($Ports | ForEach-Object {
        "netsh interface portproxy delete v4tov4 listenport=$_ listenaddress=127.0.0.1 2>`$null;"
        "netsh interface portproxy add v4tov4 listenport=$_ listenaddress=127.0.0.1 connectport=$_ connectaddress=$wslIp"
    }) -join "; "

    Start-Process powershell -Verb RunAs -ArgumentList "-Command", $cmds -Wait
    Log "Port forwarding updated (WSL IP: $wslIp)"
}

Log "Done. Access http://127.0.0.1:8080"

# --- 7. Keep WSL alive in background ---
$wslKeepAlive = Start-Process -FilePath "wsl" -ArgumentList "-d", $WslDist, "--", "bash", "-c", "trap 'exit 0' TERM INT; while true; do sleep 3600; done" -PassThru -WindowStyle Hidden

# --- 8. Watch mode: Ctrl+R manual redeploy, Ctrl+C exit ---
Log "Watch mode active | Ctrl+R to redeploy | Ctrl+C to exit"

function Invoke-Redeploy {
    param([string]$Reason)
    Log "=== Redeploying ($Reason) ==="

    # Rsync
    Log "Syncing files..."
    wsl -d $WslDist -- bash -c $rsyncCmd
    if ($LASTEXITCODE -ne 0) {
        Err "rsync failed, skipping this redeploy"
        return
    }
    Log "Sync complete"

    # Run deploy.sh --redeploy (skip infra, just rebuild + recreate redmemo)
    Log "Rebuilding in WSL..."
    wsl -d $WslDist -- bash -c "cd $WslDir; ./deploy.sh --redeploy"
    if ($LASTEXITCODE -ne 0) {
        Err "Redeploy failed"
        return
    }

    # Health check
    for ($i = 0; $i -lt 10; $i++) {
        Start-Sleep -Seconds 1
        $check = wsl -d $WslDist -- bash -c "curl -sf http://127.0.0.1:8080/settings > /dev/null 2>&1 && echo ok || echo no"
        if ($check.Trim() -eq "ok") {
            Log "Redeploy succeeded!"
            return
        }
    }
    Warn "Health check pending after redeploy"
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
                Log "Watch mode active | Ctrl+R to redeploy | Ctrl+C to exit"
            }
        }
        Start-Sleep -Milliseconds 100
    }
} finally {
    [Console]::TreatControlCAsInput = $false
    if ($wslKeepAlive -and -not $wslKeepAlive.HasExited) {
        $wslKeepAlive.Kill()
    }
    Log "Exiting watch mode"
}
