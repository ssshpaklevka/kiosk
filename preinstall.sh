#!/bin/bash
# Pre-install script: проверка зависимостей и загрузка media-player-go
# Запуск: ./preinstall.sh   (при необходимости: sudo ./preinstall.sh)

set -e

REPO_RAW="https://raw.githubusercontent.com/clownlessmode/media-player-go/main"
SETUP_NAME="setup"
INSTALL_DIR="${INSTALL_DIR:-/tmp/media-player-go-install}"

# --- Определение менеджера пакетов ---
detect_pkg_manager() {
	if command -v apt-get &>/dev/null; then
		PKG_MGR="apt"
		PKG_UPDATE="apt-get update -qq"
		PKG_INSTALL="apt-get install -y"
	elif command -v dnf &>/dev/null; then
		PKG_MGR="dnf"
		PKG_UPDATE="dnf check-update -q || true"
		PKG_INSTALL="dnf install -y"
	elif command -v yum &>/dev/null; then
		PKG_MGR="yum"
		PKG_UPDATE="yum check-update -q || true"
		PKG_INSTALL="yum install -y"
	elif command -v pacman &>/dev/null; then
		PKG_MGR="pacman"
		PKG_UPDATE="pacman -Sy --noconfirm"
		PKG_INSTALL="pacman -S --noconfirm"
	elif command -v apk &>/dev/null; then
		PKG_MGR="apk"
		PKG_UPDATE="apk update"
		PKG_INSTALL="apk add"
	else
		echo "Не найден подходящий менеджер пакетов (apt/dnf/yum/pacman/apk)."
		exit 1
	fi
}

# --- Установка пакета при отсутствии ---
ensure_installed() {
	local name="$1"
	local pkg="${2:-$1}"
	if command -v "$name" &>/dev/null; then
		echo "[OK] $name уже установлен: $(command -v "$name")"
		return 0
	fi
	echo "[...] Устанавливаю $name..."
	case "$PKG_MGR" in
		apt|dnf|yum)
			$PKG_UPDATE
			$PKG_INSTALL $pkg
			;;
		pacman)
			$PKG_UPDATE
			$PKG_INSTALL $pkg
			;;
		apk)
			$PKG_INSTALL $pkg
			;;
		*)
			echo "Установите $name вручную."
			exit 1
			;;
	esac
	echo "[OK] $name установлен."
}

# --- Установка пакета при отсутствии (при ошибке — предупреждение, скрипт не падает) ---
ensure_installed_optional() {
	local name="$1"
	local pkg="${2:-$1}"
	if command -v "$name" &>/dev/null; then
		echo "[OK] $name уже установлен: $(command -v "$name")"
		return 0
	fi
	echo "[...] Устанавливаю $name (опционально)..."
	set +e
	case "$PKG_MGR" in
		apt|dnf|yum)
			$PKG_UPDATE
			$PKG_INSTALL $pkg
			;;
		pacman)
			$PKG_UPDATE
			$PKG_INSTALL $pkg
			;;
		apk)
			$PKG_INSTALL $pkg
			;;
		*)
			set -e
			echo "Установите $name вручную при необходимости."
			return 1
			;;
	esac
	local ret=$?
	set -e
	if [ $ret -ne 0 ]; then
		echo "[!] Не удалось установить $name (возможен конфликт зависимостей). Продолжаю без него."
		return 1
	fi
	echo "[OK] $name установлен."
	return 0
}

# --- Отключение скринсейвера и гашения экрана (XFCE / X11) ---
disable_screensaver_and_dpms() {
	echo "[...] Отключаю скринсейвер и гашение экрана..."
	local desk_user=""
	local pid
	pid=$(pgrep xfce4-screensaver 2>/dev/null | head -1)
	[ -n "$pid" ] && desk_user=$(ps -o user= -p "$pid" 2>/dev/null | tr -d ' ')
	# иначе первый пользователь с домашней папкой в /home (uid >= 1000)
	if [ -z "$desk_user" ]; then
		for u in $(ls /home 2>/dev/null); do
			uid=$(id -u "$u" 2>/dev/null)
			if [ -n "$uid" ] && [ "$uid" -ge 1000 ] 2>/dev/null; then
				desk_user="$u"
				break
			fi
		done
	fi
	[ -z "$desk_user" ] && echo "[!] Пользователь рабочего стола не найден, пропускаю отключение скринсейвера." && return 0

	local disp=":0"
	set +e
	# xfconf: отключить скринсейвер и блокировку
	if command -v xfconf-query &>/dev/null; then
		sudo -u "$desk_user" DISPLAY="$disp" xfconf-query -c xfce4-screensaver -p /saver/enabled -s false 2>/dev/null
		sudo -u "$desk_user" DISPLAY="$disp" xfconf-query -c xfce4-screensaver -p /lock/enabled -s false 2>/dev/null
		echo "[OK] xfce4-screensaver отключён в настройках (xfconf)"
	fi
	# убрать из автозапуска
	local autostart="/home/$desk_user/.config/autostart"
	local off="/home/$desk_user/.config/autostart-off"
	if [ -f "$autostart/xfce4-screensaver.desktop" ]; then
		mkdir -p "$off"
		mv "$autostart/xfce4-screensaver.desktop" "$off/" 2>/dev/null && echo "[OK] xfce4-screensaver убран из автозапуска"
	fi
	# xset: не гасить экран, не включать DPMS
	if command -v xset &>/dev/null; then
		sudo -u "$desk_user" DISPLAY="$disp" xset s off -dpms s noblank 2>/dev/null && echo "[OK] xset: экран не будет гаснуть (DPMS/blank off)"
	fi
	# разрешить root подключаться к X (чтобы mediaplayer из-под root мог выводить на экран)
	if command -v xhost &>/dev/null; then
		sudo -u "$desk_user" DISPLAY="$disp" xhost +SI:localuser:root 2>/dev/null && echo "[OK] xhost: root разрешён доступ к X"
	fi
	# экран 1280x720 (xrandr)
	if command -v xrandr &>/dev/null; then
		outname=""
		while read -r line; do
			if [[ "$line" == *" connected"* ]]; then
				outname=$(echo "$line" | awk '{print $1}')
				break
			fi
		done < <(sudo -u "$desk_user" DISPLAY="$disp" xrandr -q 2>/dev/null)
		if [ -n "$outname" ]; then
			if sudo -u "$desk_user" DISPLAY="$disp" xrandr --output "$outname" --mode 1280x720 2>/dev/null || \
			   sudo -u "$desk_user" DISPLAY="$disp" xrandr --output "$outname" --mode 1280x720_60.00 2>/dev/null || \
			   sudo -u "$desk_user" DISPLAY="$disp" xrandr --output "$outname" --mode 1280x720_60 2>/dev/null; then
				echo "[OK] xrandr: разрешение 1280x720"
			fi
		fi
	fi
	# консоль: не гасить (на всякий случай; ошибки не выводим)
	( [ -w /sys/module/kernel/parameters/consoleblank ] && echo 0 > /sys/module/kernel/parameters/consoleblank ) 2>/dev/null && echo "[OK] consoleblank=0" || true
	[ -w /dev/tty1 ] && printf '\033[9;0]' > /dev/tty1 2>/dev/null || true
	# завершить уже запущенный скринсейвер/слайдшоу
	pkill -u "$desk_user" -f xfce4-screensaver 2>/dev/null
	pkill -u "$desk_user" -f 'slideshow --location' 2>/dev/null
	set -e
	echo "[OK] Скринсейвер и гашение экрана отключены для пользователя $desk_user"
}

# --- Скачивание файла ---
download() {
	local url="$1"
	local out="$2"
	if command -v curl &>/dev/null; then
		curl -fsSL "$url" -o "$out" || { rm -f "$out"; return 1; }
	elif command -v wget &>/dev/null; then
		wget -q -O "$out" "$url" || { rm -f "$out"; return 1; }
	else
		echo "Нужен curl или wget для загрузки."
		exit 1
	fi
}

# --- main ---
echo "=== Pre-install: media-player-go ==="
detect_pkg_manager

ensure_installed "git" "git"
ensure_installed "ffmpeg" "ffmpeg"
ensure_installed_optional "mplayer" "mplayer" || true

mkdir -p "$INSTALL_DIR"
cd "$INSTALL_DIR"

echo "[...] Загружаю $SETUP_NAME..."
if download "$REPO_RAW/$SETUP_NAME" "$SETUP_NAME"; then
	chmod +x "$SETUP_NAME"
	echo "[OK] $SETUP_NAME загружен."
else
	echo "[FAIL] Не удалось загрузить $SETUP_NAME с $REPO_RAW/$SETUP_NAME"
	exit 1
fi

disable_screensaver_and_dpms

echo "[...] Запускаю setup с sudo..."
sudo ./"$SETUP_NAME"
echo "=== Готово ==="
