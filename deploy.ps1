$ErrorActionPreference = "Stop"

$WinDir  = $PSScriptRoot
$WslDist = "Debian"
$WslDir  = "~/redmemo"
$Ports   = @(8080, 8081)

function Log($msg)  { Write-Host "[deploy] $msg" -ForegroundColor Green }
function Warn($msg) { Write-Host "[deploy] $msg" -ForegroundColor Yellow }
function Err($msg)  { Write-Host "[deploy] $msg" -ForegroundColor Red }

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

# --- 5. Run deploy.sh in WSL ---
Log "Starting deployment in WSL..."
wsl -d $WslDist -- bash -c "chmod +x $WslDir/deploy.sh; cd $WslDir; ./deploy.sh"

if ($LASTEXITCODE -ne 0) {
    Err "Deployment failed"
    exit 1
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
