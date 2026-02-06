# Сборка медиа-плеера для Orange Pi (Armbian, Linux ARM)
# Использование:
#   .\build-orangepi.ps1           — собрать для arm64 (Orange Pi 3/4/5 и т.д.)
#   .\build-orangepi.ps1 arm64      — то же
#   .\build-orangepi.ps1 arm        — собрать для armv7 (Orange Pi Zero, One, PC)

param(
    [string]$ARCH = "arm64"
)

$VERSION = if ($env:VERSION) { $env:VERSION } else { "dev" }
$BINARY = "setup"

Set-Location $PSScriptRoot

if ($ARCH -eq "arm64" -or $ARCH -eq "aarch64") {
    $env:GOOS = "linux"
    $env:GOARCH = "arm64"
    go build -ldflags "-X main.Version=$VERSION" -o $BINARY .
    Write-Host "Sobran: $BINARY"
}
elseif ($ARCH -eq "arm" -or $ARCH -eq "armv7" -or $ARCH -eq "armhf") {
    $env:GOOS = "linux"
    $env:GOARCH = "arm"
    $env:GOARM = "7"
    go build -ldflags "-X main.Version=$VERSION" -o $BINARY .
    Write-Host "Sobran: $BINARY"
}
else {
    Write-Host "Neizvestnaya arhitektura: $ARCH"
    Write-Host "Dopustimo: arm64, arm"
    exit 1
}