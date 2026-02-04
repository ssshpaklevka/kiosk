// Медиа-плеер для Orange Pi: чек-ин по MAC, JWT, GET /api/device/me/media, загрузка по id, воспроизведение; синхронизация при получении токена и в 4:00.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Версия (подставляется при сборке: -ldflags "-X main.Version=...")
var Version = "dev"

// videoPlayerCmd — имя плеера после runStartupChecks: "mplayer" или "mpv"
var videoPlayerCmd string

// HTTP-клиент без проверки TLS (для загрузки с любых источников).
var httpClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

const (
	checkInPath = "/api/device/check-in"
	mediaPath   = "/api/device/me/media"
	jwtFile     = ".jwt"
	mediaDir    = "./media"
)

type config struct {
	ServerURL string
	MediaDir  string
}

// MediaItem — элемент ответа GET /api/device/me/media
type MediaItem struct {
	ID   string `json:"id"`
	URL  string `json:"url"`
	Name string `json:"name"`
}

func main() {
	cfg := config{
		ServerURL: getEnv("SERVER_URL", "http://192.168.0.4:3000"),
		MediaDir:  getEnv("MEDIA_DIR", mediaDir),
	}
	if err := os.MkdirAll(cfg.MediaDir, 0755); err != nil {
		exit(err)
	}
	// Абсолютный путь, чтобы плейлист и пути не зависели от cwd (иначе media/media/... при запуске из media/)
	absMediaDir, err := filepath.Abs(cfg.MediaDir)
	if err != nil {
		exit(err)
	}
	cfg.MediaDir = absMediaDir

	mac := macAddressString()
	fmt.Printf("[mediaplayer] запуск, MAC=%s, SERVER=%s, MEDIA_DIR=%s\n", mac, cfg.ServerURL, cfg.MediaDir)

	runStartupChecks()

	// 1. Чек-ин каждые 10 минут (при 401 не выходим, продолжаем ждать)
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		doCheckIn := func() {
			jwt, err := checkIn(cfg.ServerURL, mac)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[mediaplayer] check-in: %v\n", err)
				return
			}
			if jwt != "" {
				if err := saveJWT(jwt); err != nil {
					fmt.Fprintf(os.Stderr, "[mediaplayer] save JWT: %v\n", err)
				} else {
					fmt.Println("[mediaplayer] check-in OK, token saved")
				}
			} else {
				fmt.Println("[mediaplayer] устройство ожидает назначения группы (401)")
			}
		}
		doCheckIn()
		for range ticker.C {
			doCheckIn()
		}
	}()

	// 2. Синхронизация медиа при первом JWT и в 4:00; воспроизведение после загрузки
	var mplayerMu sync.Mutex
	var mplayerCmd *exec.Cmd
	var ffmpegCmd *exec.Cmd
	var playCancel context.CancelFunc
	initialSyncDone := false
	lastRunDate := ""

	stopPlayback := func() {
		if playCancel != nil {
			playCancel()
			playCancel = nil
		}
		mplayerMu.Lock()
		if mplayerCmd != nil && mplayerCmd.Process != nil {
			_ = mplayerCmd.Process.Kill()
			mplayerCmd = nil
		}
		if ffmpegCmd != nil && ffmpegCmd.Process != nil {
			_ = ffmpegCmd.Process.Kill()
			ffmpegCmd = nil
		}
		mplayerMu.Unlock()
		clearDisplayBlack() // сразу чёрный экран, чтобы не мелькала консоль
	}

	syncAndPlay := func() {
		jwt, _ := loadJWT()
		if jwt == "" {
			return
		}
		fmt.Println("[mediaplayer] JWT есть, запрашиваю медиа...")
		stopPlayback()
		items, err := fetchMedia(cfg.ServerURL, jwt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[mediaplayer] fetch media: %v\n", err)
			return
		}
		fmt.Printf("[mediaplayer] медиа с сервера: %d шт.\n", len(items))
		if len(items) == 0 {
			fmt.Println("[mediaplayer] список пуст, воспроизведение не запускаю")
			return
		}
		keepIDs := make(map[string]bool)
		for _, it := range items {
			keepIDs[fileID(it.ID)] = true
		}
		cleanupByIDs(cfg.MediaDir, keepIDs)
		fmt.Println("[mediaplayer] скачиваю файлы...")
		downloaded, err := downloadMedia(cfg.MediaDir, items)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[mediaplayer] download: %v\n", err)
		}
		fmt.Printf("[mediaplayer] скачано: %d из %d\n", len(downloaded), len(items))
		if len(downloaded) == 0 {
			fmt.Println("[mediaplayer] ни одного файла не загрузилось, воспроизведение не запускаю")
			return
		}
		fmt.Printf("[mediaplayer] запускаю воспроизведение (ffmpeg concat → %s, vo=%s)\n", videoPlayerCmd, mplayerVideoOutput())
		setDisplayResolution1280x720() // 1280x720 перед воспроизведением (X11)
		clearDisplayBlack()            // чёрный до первого кадра
		ctx, cancel := context.WithCancel(context.Background())
		playCancel = cancel
		go func() {
			defer cancel()
			select {
			case <-ctx.Done():
				return
			default:
			}
			mplayerMu.Lock()
			ffmpegCmd, mplayerCmd = runConcatPlayback(cfg.MediaDir)
			mplayerMu.Unlock()
			if mplayerCmd == nil {
				return
			}
			_ = mplayerCmd.Wait()
			mplayerMu.Lock()
			if ffmpegCmd != nil && ffmpegCmd.Process != nil {
				_ = ffmpegCmd.Process.Kill()
			}
			ffmpegCmd, mplayerCmd = nil, nil
			mplayerMu.Unlock()
		}()
	}

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Первую проверку делаем сразу (чтобы не ждать минуту после check-in), дальше — по тикеру
	for first := true; ; first = false {
		if !first {
			<-ticker.C
		}
		jwt, _ := loadJWT()
		if jwt == "" {
			if first {
				time.Sleep(2 * time.Second) // дать чек-ину успеть сохранить токен
				jwt, _ = loadJWT()
			}
			if jwt == "" {
				continue
			}
		}
		now := time.Now()
		is4AM := now.Hour() == 4 && now.Minute() == 0
		today := now.Format("2006-01-02")

		if !initialSyncDone {
			initialSyncDone = true
			fmt.Println("[mediaplayer] первый запуск с токеном — синхронизация медиа")
			syncAndPlay()
			continue
		}
		if is4AM && today != lastRunDate {
			lastRunDate = today
			fmt.Println("[mediaplayer] 4:00 — синхронизация медиа")
			syncAndPlay()
		}
	}
}

// macAddressString возвращает MAC первого не-loopback интерфейса в формате "AA:BB:CC:DD:EE:FF".
func macAddressString() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || len(iface.HardwareAddr) < 6 {
			continue
		}
		a := iface.HardwareAddr
		return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", a[0], a[1], a[2], a[3], a[4], a[5])
	}
	return ""
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimRight(v, "/")
	}
	return def
}

func loadJWT() (string, error) {
	b, err := os.ReadFile(jwtFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func saveJWT(token string) error {
	return os.WriteFile(jwtFile, []byte(token), 0600)
}

type checkInReq struct {
	MACAddress string `json:"macAddress"`
}

type checkInResp struct {
	AccessToken string `json:"accessToken"`
}

func checkIn(serverURL, macAddress string) (jwt string, err error) {
	if macAddress == "" {
		return "", fmt.Errorf("mac address not found")
	}
	body, _ := json.Marshal(checkInReq{MACAddress: macAddress})
	req, err := http.NewRequest(http.MethodPost, serverURL+checkInPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bs, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("check-in %d: %s", resp.StatusCode, string(bs))
	}
	var out checkInResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

func fetchMedia(serverURL, jwt string) ([]MediaItem, error) {
	req, err := http.NewRequest(http.MethodGet, serverURL+mediaPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("401: токен невалиден")
	}
	if resp.StatusCode != http.StatusOK {
		bs, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("media %d: %s", resp.StatusCode, string(bs))
	}
	var items []MediaItem
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		return nil, err
	}
	return items, nil
}

// fileID делает безопасное имя файла из id (подписываем как в ссылках).
func fileID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	s := b.String()
	if s == "" {
		return "media"
	}
	return s
}

func downloadMedia(dir string, items []MediaItem) (downloaded []string, err error) {
	for _, it := range items {
		if it.URL == "" {
			continue
		}
		ext := extFromURL(it.URL)
		name := filepath.Join(dir, fileID(it.ID)+ext)
		if err := downloadFile(it.URL, name); err != nil {
			fmt.Fprintf(os.Stderr, "[mediaplayer] download %s: %v\n", it.URL, err)
			continue
		}
		fmt.Printf("[mediaplayer] загружен: %s -> %s\n", it.Name, filepath.Base(name))
		downloaded = append(downloaded, name)
	}
	return downloaded, nil
}

func extFromURL(url string) string {
	u := strings.ToLower(url)
	for _, e := range []string{".mkv", ".mp4", ".avi", ".webm"} {
		if strings.Contains(u, e) {
			return e
		}
	}
	return ".mp4"
}

func downloadFile(url, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// runStartupChecks проверяет зависимости и окружение перед работой; при отсутствии mplayer/mpv и ffmpeg — выход.
func runStartupChecks() {
	// 1) Нужен хотя бы один плеер (mplayer или mpv) и ffmpeg
	videoPlayerCmd = ""
	if _, err := exec.LookPath("mplayer"); err == nil {
		videoPlayerCmd = "mplayer"
	}
	if videoPlayerCmd == "" {
		if _, err := exec.LookPath("mpv"); err == nil {
			videoPlayerCmd = "mpv"
		}
	}
	if videoPlayerCmd == "" {
		fmt.Fprintln(os.Stderr, "[mediaplayer] ошибка: не найден mplayer или mpv. Установите: apt install mplayer  или  pacman -S mpv")
		os.Exit(1)
	}
	fmt.Printf("[mediaplayer] проверка: %s найден\n", videoPlayerCmd)
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fmt.Fprintln(os.Stderr, "[mediaplayer] ошибка: ffmpeg не найден. Установите: apt install ffmpeg  или  pacman -S ffmpeg")
		os.Exit(1)
	}
	fmt.Println("[mediaplayer] проверка: ffmpeg найден")

	// 2) аудио: есть ли хотя бы одна звуковая карта
	if data, err := os.ReadFile("/proc/asound/cards"); err != nil || strings.TrimSpace(string(data)) == "" || strings.Contains(string(data), "no soundcards") {
		fmt.Fprintln(os.Stderr, "[mediaplayer] предупреждение: звуковые карты не найдены (aplay -l). Звук может не работать.")
	} else {
		fmt.Println("[mediaplayer] проверка: звуковые карты обнаружены")
	}

	// 3) экран 1280x720: при X11 выставляем разрешение сразу
	if mplayerDisplay() != "" {
		setDisplayResolution1280x720()
	} else {
		fmt.Println("[mediaplayer] проверка: X11 не активен, вывод будет в fbdev2 (разрешение из загрузки)")
	}
}

// setDisplayResolution1280x720 переключает экран в 1280x720 перед воспроизведением (X11 через xrandr).
func setDisplayResolution1280x720() {
	disp := mplayerDisplay()
	if disp == "" {
		return
	}
	env := os.Environ()
	if os.Getenv("DISPLAY") == "" {
		var filtered []string
		for _, e := range env {
			if !strings.HasPrefix(e, "DISPLAY=") {
				filtered = append(filtered, e)
			}
		}
		env = append(filtered, "DISPLAY="+disp)
	}
	// Узнаём имя подключённого вывода (HDMI-1, HDMI-A-1 и т.п.)
	cmd := exec.Command("xrandr", "-q")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return
	}
	var outputName string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, " connected") {
			fields := strings.Fields(line)
			if len(fields) >= 1 {
				outputName = fields[0]
				break
			}
		}
	}
	if outputName == "" {
		return
	}
	tryMode := func(mode string) bool {
		cmd := exec.Command("xrandr", "--output", outputName, "--mode", mode)
		cmd.Env = env
		return cmd.Run() == nil
	}
	if tryMode("1280x720") || tryMode("1280x720_60.00") || tryMode("1280x720_60") {
		fmt.Println("[mediaplayer] разрешение экрана: 1280x720")
	}
}

// clearDisplayBlack заливает экран чёрным, чтобы между остановкой и запуском mplayer не мелькала консоль.
func clearDisplayBlack() {
	// 1) Очистка консоли на дисплее (tty1) и скрытие курсора
	for _, dev := range []string{"/dev/tty1", "/dev/tty0", "/dev/console"} {
		f, err := os.OpenFile(dev, os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		_, _ = f.WriteString("\033[2J\033[H\033[?25l") // clear, home, hide cursor
		f.Close()
		break
	}
	// 2) Заливка фреймбуфера нулями (чёрный) — не зависит от консоли
	fbClear()
}

// fbClear пишет нули в /dev/fb0. Размер берётся из sysfs.
func fbClear() {
	const (
		fbDev    = "/dev/fb0"
		sysSize  = "/sys/class/graphics/fb0/virtual_size"
		sysBpp   = "/sys/class/graphics/fb0/bits_per_pixel"
		sysWidth = "/sys/class/graphics/fb0/width"
		sysHeight = "/sys/class/graphics/fb0/height"
	)
	var w, h, bpp int
	if b, err := os.ReadFile(sysSize); err == nil {
		parts := strings.Split(strings.TrimSpace(string(b)), ",")
		if len(parts) >= 2 {
			fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &w)
			fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &h)
		}
	}
	if w <= 0 || h <= 0 {
		if bw, _ := os.ReadFile(sysWidth); len(bw) > 0 {
			fmt.Sscanf(strings.TrimSpace(string(bw)), "%d", &w)
		}
		if bh, _ := os.ReadFile(sysHeight); len(bh) > 0 {
			fmt.Sscanf(strings.TrimSpace(string(bh)), "%d", &h)
		}
	}
	if b, err := os.ReadFile(sysBpp); err == nil {
		fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &bpp)
	}
	if w <= 0 || h <= 0 || bpp <= 0 {
		return
	}
	size := int64(w) * int64(h) * int64(bpp/8)
	if size <= 0 || size > 64*1024*1024 {
		return
	}
	f, err := os.OpenFile(fbDev, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	const chunk = 256 * 1024
	var buf [chunk]byte
	for written := int64(0); written < size; written += chunk {
		n := chunk
		if size-written < int64(chunk) {
			n = int(size - written)
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return
		}
	}
}

// cleanupByIDs удаляет файлы в dir, чей id (имя без расширения) не в keepIDs.
func cleanupByIDs(dir string, keepIDs map[string]bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := e.Name()
		ext := filepath.Ext(base)
		id := strings.TrimSuffix(base, ext)
		if id == "" {
			continue
		}
		if !keepIDs[id] {
			_ = os.Remove(filepath.Join(dir, base))
		}
	}
}

// mplayerVideoOutput выбирает вывод видео: в графической среде (десктоп) — x11, иначе fbdev2.
// Если DISPLAY пустой (запуск из SSH/консоли), но X11 запущен (LightDM/XFCE), используем :0.
// Переменная MPLAYER_VO переопределяет выбор (например MPLAYER_VO=x11 или MPLAYER_VO=fbdev2).
func mplayerVideoOutput() string {
	if v := os.Getenv("MPLAYER_VO"); v != "" {
		return v
	}
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		return "x11"
	}
	// Запуск из SSH/консоли без DISPLAY — если X слушает на :0, выводим туда
	if _, err := os.Stat("/tmp/.X11-unix/X0"); err == nil {
		return "x11"
	}
	return "fbdev2"
}

// mplayerDisplay возвращает DISPLAY для процесса mplayer (при vo=x11). Если в окружении пусто — :0.
func mplayerDisplay() string {
	if d := os.Getenv("DISPLAY"); d != "" {
		return d
	}
	if _, err := os.Stat("/tmp/.X11-unix/X0"); err == nil {
		return ":0"
	}
	return ""
}

// xauthPath возвращает путь к .Xauthority для доступа к X11 (при запуске из-под root/sudo).
func xauthPath() string {
	if p := os.Getenv("XAUTHORITY"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Классические пути
	for _, name := range []string{os.Getenv("SUDO_USER"), "user"} {
		if name == "" {
			continue
		}
		p := "/home/" + name + "/.Xauthority"
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// Владелец сокета X0 — его /run/user/UID/.Xauthority (systemd logind)
	var st syscall.Stat_t
	if err := syscall.Stat("/tmp/.X11-unix/X0", &st); err == nil {
		p := filepath.Join("/run/user", fmt.Sprint(st.Uid), ".Xauthority")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// LightDM и др.
	for _, p := range []string{"/var/run/lightdm/.Xauthority", "/var/lib/gdm/.Xauthority"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// runConcatPlayback запускает ffmpeg concat (бесконечный цикл по файлам) в пайп в mplayer.
// Один непрерывный поток — ни чёрного экрана, ни пауз между роликами.
func runConcatPlayback(mediaDir string) (ffmpeg *exec.Cmd, mplayer *exec.Cmd) {
	files := listVideoFiles(mediaDir)
	if len(files) == 0 {
		return nil, nil
	}
	concatPath := filepath.Join(mediaDir, ".concat.txt")
	var b strings.Builder
	for _, f := range files {
		// ffmpeg concat: file 'path'; одинарную кавычку в path — удвоением ''
		escaped := strings.ReplaceAll(f, "'", "''")
		b.WriteString("file '")
		b.WriteString(escaped)
		b.WriteString("'\n")
	}
	if err := os.WriteFile(concatPath, []byte(b.String()), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "concat list write: %v\n", err)
		return nil, nil
	}
	// ffmpeg: бесконечный цикл по списку; genpts — ровные таймстемпы на стыках файлов, меньше зависаний
	ffmpeg = exec.Command("ffmpeg",
		"-stream_loop", "-1",
		"-f", "concat", "-safe", "0", "-i", concatPath,
		"-fflags", "+genpts",
		"-c", "copy", "-f", "matroska", "-",
	)
	ffmpeg.Stderr = nil // не смешивать с выводом mplayer
	pipe, err := ffmpeg.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ffmpeg stdout pipe: %v\n", err)
		return nil, nil
	}
	if err := ffmpeg.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "ffmpeg start: %v\n", err)
		return nil, nil
	}
	// Видеовыход: в графической среде (десктоп) — x11, иначе fbdev2/drm (консоль/без дисплея)
	vo := mplayerVideoOutput()
	audioDevice := getEnv("MPLAYER_AUDIO_DEVICE", "plughw:1,0")

	if videoPlayerCmd == "mpv" {
		// mpv: x11 (gpu/drm при запуске от root часто дают Permission denied / DRM busy)
		mpvVo := vo
		if vo == "fbdev2" {
			mpvVo = "drm"
		}
		args := []string{
			"-",
			"--vo=" + mpvVo,
			"--ao=alsa",
			"--vf=scale=1280:720",
			"--cache=yes", "--demuxer-max-bytes=150M",
		}
		if vo == "x11" {
			args = append(args, "--fs")
		}
		mplayer = exec.Command("mpv", args...)
		mplayer.Stdin = pipe
		mplayer.Stdout = os.Stdout
		mplayer.Stderr = os.Stderr
		if vo == "x11" && mplayerDisplay() != "" {
			env := os.Environ()
			var filtered []string
			for _, e := range env {
				if !strings.HasPrefix(e, "DISPLAY=") && !strings.HasPrefix(e, "XAUTHORITY=") {
					filtered = append(filtered, e)
				}
			}
			filtered = append(filtered, "DISPLAY="+mplayerDisplay())
			if xauth := xauthPath(); xauth != "" {
				filtered = append(filtered, "XAUTHORITY="+xauth)
			}
			mplayer.Env = filtered
		}
		if err := mplayer.Start(); err != nil {
			_ = ffmpeg.Process.Kill()
			fmt.Fprintf(os.Stderr, "mpv start: %v\n", err)
			return nil, nil
		}
		return ffmpeg, mplayer
	}

	// mplayer
	args := []string{
		"-ao", "alsa:device=" + audioDevice,
		"-vo", vo,
		"-vf", "scale=1280:720",
		"-lavdopts", "lowres=0:fast",
		"-cache", "32768",
	}
	if vo == "x11" {
		args = append(args, "-fs")
	}
	args = append(args, "-")
	mplayer = exec.Command("mplayer", args...)
	mplayer.Stdin = pipe
	mplayer.Stdout = os.Stdout
	mplayer.Stderr = os.Stderr
	if vo == "x11" && mplayerDisplay() != "" {
		env := os.Environ()
		var filtered []string
		for _, e := range env {
			if !strings.HasPrefix(e, "DISPLAY=") && !strings.HasPrefix(e, "XAUTHORITY=") {
				filtered = append(filtered, e)
			}
		}
		filtered = append(filtered, "DISPLAY="+mplayerDisplay())
		if xauth := xauthPath(); xauth != "" {
			filtered = append(filtered, "XAUTHORITY="+xauth)
		}
		mplayer.Env = filtered
	}
	if err := mplayer.Start(); err != nil {
		_ = ffmpeg.Process.Kill()
		fmt.Fprintf(os.Stderr, "mplayer start: %v\n", err)
		return nil, nil
	}
	return ffmpeg, mplayer
}

func listVideoFiles(mediaDir string) []string {
	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		if strings.HasSuffix(name, ".mkv") || strings.HasSuffix(name, ".mp4") ||
			strings.HasSuffix(name, ".avi") || strings.HasSuffix(name, ".webm") {
			files = append(files, filepath.Join(mediaDir, e.Name()))
		}
	}
	return files
}

func exit(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
