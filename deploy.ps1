$ErrorActionPreference = "Stop"

$WinDir  = $PSScriptRoot
$WslDist = "Debian"
$WslDir  = "~/redmemo"

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

Log "Done. Access http://127.0.0.1:8080"
