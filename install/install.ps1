param(
    [string]$FromUSB = "",
    [string]$DataDir = "$env:ProgramData\Ark\Data",
    [int]$Port = 8080,
    [string]$BinDir = "$env:ProgramFiles\Ark",
    [switch]$Upgrade,
    [switch]$RegisterStartup
)

$ErrorActionPreference = "Stop"
Write-Host "[Ark] Installing Ark Command Center (Data: $DataDir, Port: $Port)..."

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw "Docker is not installed. Please install Docker Desktop before installing Ark."
}
docker info *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Warning "[Ark] Docker daemon is not running yet; start Docker Desktop before starting apps."
}

New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null

$ExeTarget = Join-Path $BinDir "ark.exe"
$SrcExe = Join-Path $PSScriptRoot "..\ark.exe"

if ($FromUSB -ne "") {
    $Manifest = Join-Path $FromUSB "manifest.json"
    if (-not (Test-Path $Manifest)) {
        throw "USB bundle manifest.json not found at $FromUSB"
    }
    Write-Host "[Ark] Found offline USB bundle at $FromUSB"

    $TarPath = Join-Path $FromUSB "images\ark-images.tar"
    if (Test-Path $TarPath) {
        Write-Host "[Ark] Loading offline Docker images..."
        docker load -i $TarPath
    }

    $UsbData = Join-Path $FromUSB "data"
    if (Test-Path $UsbData) {
        Write-Host "[Ark] Copying offline content to $DataDir..."
        Copy-Item -Path "$UsbData\*" -Destination $DataDir -Recurse -Force
    }

    $UsbDB = Join-Path $FromUSB "config\ark.db"
    $DstDB = Join-Path $DataDir "ark.db"
    if ((Test-Path $UsbDB) -and -not (Test-Path $DstDB)) {
        Copy-Item -Path $UsbDB -Destination $DstDB -Force
    }

    $UsbBin = Join-Path $FromUSB "bin\ark.exe"
    if (Test-Path $UsbBin) {
        $SrcExe = $UsbBin
    }
}

$DstDB = Join-Path $DataDir "ark.db"
if ($Upgrade -and (Test-Path $DstDB)) {
    $BackupDir = Join-Path $DataDir "backups"
    New-Item -ItemType Directory -Force -Path $BackupDir | Out-Null
    Copy-Item -Path $DstDB -Destination (Join-Path $BackupDir "ark.db.pre-upgrade") -Force
}

if (Test-Path $SrcExe) {
    if (Test-Path $ExeTarget) {
        Move-Item -Path $ExeTarget -Destination "$ExeTarget.old" -Force
    }
    Copy-Item -Path $SrcExe -Destination $ExeTarget -Force
    Write-Host "[Ark] Installed binary to $ExeTarget"
}

try {
    $RuleName = "Ark Command Center ($Port)"
    Remove-NetFirewallRule -DisplayName $RuleName -ErrorAction SilentlyContinue
    New-NetFirewallRule -DisplayName $RuleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $Port -Profile Private | Out-Null
    Write-Host "[Ark] Added Private network firewall rule for TCP port $Port."
} catch {
    Write-Warning "[Ark] Could not configure Windows Firewall rule (run as Administrator to apply): $_"
}

if ($RegisterStartup) {
    $Action = New-ScheduledTaskAction -Execute $ExeTarget -Argument "run -data-dir `"$DataDir`" -port $Port"
    $Trigger = New-ScheduledTaskTrigger -AtStartup
    $Principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest
    Register-ScheduledTask -TaskName "ArkCommandCenter" -Action $Action -Trigger $Trigger -Principal $Principal -Force | Out-Null
    Write-Host "[Ark] Registered Windows startup task 'ArkCommandCenter'."
} else {
    Write-Host "[Ark] Launch with: `"$ExeTarget`" run -data-dir `"$DataDir`" -port $Port"
}
