package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

type Config struct {
	AdminID        int64  `yaml:"admin_id"`
	HTTPHost       string `yaml:"http_host"`
	HTTPPort       int    `yaml:"http_port"`
	WebsocketURL   string `yaml:"websocket_url"`
	WebsocketToken string `yaml:"websocket_token"`

	FileDir           string `yaml:"file_dir"`
	MangaDir          string `yaml:"manga_dir"`
	CBZDir            string `yaml:"cbz_dir"`
	CBZChapterEnabled bool   `yaml:"cbz_chapter_enabled"`
	CBZSeriesEnabled  bool   `yaml:"cbz_series_enabled"`
	LogDir            string `yaml:"log_dir"`
	JMOptionPath      string `yaml:"jm_option_path"`
	TransferMode      string `yaml:"transfer_mode"`
	RemoteUser        string `yaml:"remote_user"`
	RemoteHost        string `yaml:"remote_host"`
	RemoteTempDir     string `yaml:"remote_temp_dir"`
	LocalSSHKey       string `yaml:"local_ssh_key"`
	DockerPath        string `yaml:"docker_internal_path"`

	DownloadTimeout      int     `yaml:"download_timeout"`
	SearchTimeout        int     `yaml:"search_timeout"`
	MaxEpisodes          int     `yaml:"max_episodes"`
	DedupWindow          int     `yaml:"dedup_window_seconds"`
	RandomPasswordLength int     `yaml:"random_password_length"`
	SoutuTriggerWindow   int     `yaml:"soutu_trigger_window_seconds"`
	SoutuGlobalM         int64   `yaml:"soutu_global_m"`
	SoutuFactor          float64 `yaml:"soutu_factor"`
	SoutuURL             string  `yaml:"soutu_url"`
	SoutuAPI             string  `yaml:"soutu_api"`
	SoutuUserAgent       string  `yaml:"soutu_user_agent"`
	CFBypassAPIURL       string  `yaml:"cf_bypass_api_url"`
	CFBypassPollInterval float64 `yaml:"cf_bypass_poll_interval_sec"`
	CFBypassPollTimeout  float64 `yaml:"cf_bypass_poll_timeout_sec"`
	EmbeddedBypassEnable bool    `yaml:"embedded_bypass_enabled"`
	EmbeddedBypassHost   string  `yaml:"embedded_bypass_host"`
	EmbeddedBypassPort   int     `yaml:"embedded_bypass_port"`
	HTTPPortFallback     bool    `yaml:"http_port_fallback"`
	PortFallbackTries    int     `yaml:"port_fallback_tries"`

	SendModeGlobal     string            `yaml:"send_mode_global"`
	SendModeGroup      map[string]string `yaml:"send_mode_group"`
	SendNameModeGlobal string            `yaml:"send_name_mode_global"`
	SendNameModeGroup  map[string]string `yaml:"send_name_mode_group"`

	EncEnabledGlobal  bool              `yaml:"enc_enabled_global"`
	EncEnabledGroup   map[string]bool   `yaml:"enc_enabled_group"`
	EncPasswordGlobal string            `yaml:"enc_password_global"`
	EncPasswordGroup  map[string]string `yaml:"enc_password_group"`

	RandomPasswordEnabledGlobal bool            `yaml:"random_password_enabled_global"`
	RandomPasswordEnabledGroup  map[string]bool `yaml:"random_password_enabled_group"`
	RegexEnabledGlobal          bool            `yaml:"regex_enabled_global"`
	RegexEnabledGroup           map[string]bool `yaml:"regex_enabled_group"`

	BannedID    []string `yaml:"banned_id"`
	BannedUser  []string `yaml:"banned_user"`
	BannedGroup []string `yaml:"banned_group"`

	ReplyAsCard  bool   `yaml:"reply_as_card"`
	CardNickname string `yaml:"card_nickname"`
	CardUserID   int64  `yaml:"card_user_id"`

	LocalTestMode              bool `yaml:"local_test_mode"`
	LocalTestExitAfterSelftest bool `yaml:"local_test_exit_after_selftest"`
}

type App struct {
	cfgPath string
	cfgMu   sync.RWMutex
	cfg     *Config

	bot *NapcatClient
	jm  *JMBridge

	queue chan DownloadTask

	recentMu sync.Mutex
	recent   map[string]map[string]time.Time

	searchMu sync.Mutex
	search   map[string]PendingSearch

	jmEnabledMu sync.RWMutex
	jmEnabled   bool

	soutuMu    sync.Mutex
	soutuArmed map[string]time.Time
}

type PendingSearch struct {
	AlbumID string
	Title   string
	At      time.Time
}

type DownloadTask struct {
	Number      string
	MessageType string
	GroupID     int64
	UserID      int64
	Scope       string
	Uploader    string
}

type Album struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Episodes    int      `json:"episodes"`
	Views       string   `json:"views"`
}

type bypassResponse struct {
	Message   string         `json:"message"`
	UserAgent string         `json:"user_agent"`
	Cookies   []bypassCookie `json:"cookies"`
}

type bypassCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type bypassQuery struct {
	URL         string `json:"url"`
	UserAgent   string `json:"user_agent"`
	ProxyServer string `json:"proxy_server"`
}

type embeddedBypassService struct {
	mu      sync.Mutex
	cache   map[string]embeddedCacheItem
	running map[string]bool
}

type embeddedCacheItem struct {
	Data      bypassResponse
	ExpiresAt time.Time
}

var (
	soutuCFMu            sync.RWMutex
	soutuCFCookies       = map[string]string{}
	soutuCFCookieExpires time.Time
)

func main() {
	var installService bool
	var uninstallService bool
	var serviceName string
	var serviceUser string
	var serviceGroup string
	flag.BoolVar(&installService, "install", false, "install and enable systemd service")
	flag.BoolVar(&uninstallService, "uninstall", false, "disable and remove systemd service")
	flag.StringVar(&serviceName, "service-name", "napcat-jm-go", "systemd service name")
	flag.StringVar(&serviceUser, "service-user", "", "systemd service user, default current login user")
	flag.StringVar(&serviceGroup, "service-group", "", "systemd service group, default primary group of service user")
	flag.Parse()

	if installService && uninstallService {
		log.Fatal("cannot use --install and --uninstall together")
	}
	if installService {
		if err := installSystemdService(serviceName, serviceUser, serviceGroup); err != nil {
			log.Fatalf("install service failed: %v", err)
		}
		log.Printf("service installed and started: %s", serviceName)
		return
	}
	if uninstallService {
		if err := uninstallSystemdService(serviceName); err != nil {
			log.Fatalf("uninstall service failed: %v", err)
		}
		log.Printf("service removed: %s", serviceName)
		return
	}

	app, err := NewApp("config.yml", "config.example.yml")
	if err != nil {
		log.Fatalf("init failed: %v", err)
	}

	if app.cfg.LocalTestMode {
		log.Printf("local test mode enabled")
		if err := app.runLocalSelfTest(); err != nil {
			log.Printf("local selftest failed: %v", err)
		} else {
			log.Printf("local selftest succeeded")
		}
		if app.cfg.LocalTestExitAfterSelftest {
			return
		}
	}

	go app.worker()

	cfg := app.currentConfig()
	mainMux := http.NewServeMux()
	mainMux.HandleFunc("/", app.handleHTTPEvent)
	mainServer := &http.Server{Handler: mainMux}

	mainTries := 1
	if cfg.HTTPPortFallback {
		mainTries = cfg.PortFallbackTries
	}
	mainListener, mainPort, err := listenWithFallback(cfg.HTTPHost, cfg.HTTPPort, mainTries)
	if err != nil {
		log.Fatalf("listen main http failed: %v", err)
	}
	if mainPort != cfg.HTTPPort {
		log.Printf("main port %d unavailable, fallback to %d", cfg.HTTPPort, mainPort)
	}

	var bypassServer *http.Server
	if cfg.EmbeddedBypassEnable {
		bypassSvc := newEmbeddedBypassService()
		bypassMux := http.NewServeMux()
		bypassMux.HandleFunc("/api/v1/bypass", bypassSvc.handleBypassV1)
		bypassMux.HandleFunc("/cloudflare5s/bypass-v1", bypassSvc.handleBypassV1)
		bypassServer = &http.Server{Handler: bypassMux}

		bypassListener, bypassPort, listenErr := listenWithFallback(cfg.EmbeddedBypassHost, cfg.EmbeddedBypassPort, cfg.PortFallbackTries)
		if listenErr != nil {
			log.Fatalf("listen embedded bypass failed: %v", listenErr)
		}
		if bypassPort != cfg.EmbeddedBypassPort {
			log.Printf("bypass port %d unavailable, fallback to %d", cfg.EmbeddedBypassPort, bypassPort)
		}
		if shouldUseEmbeddedBypassURL(cfg.CFBypassAPIURL) {
			u := fmt.Sprintf("http://%s:%d/api/v1/bypass", clientHost(cfg.EmbeddedBypassHost), bypassPort)
			app.cfgMu.Lock()
			app.cfg.CFBypassAPIURL = u
			app.cfgMu.Unlock()
			log.Printf("using embedded bypass api: %s", u)
		}
		go serveHTTP("embedded bypass", bypassServer, bypassListener)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("go bot listening at %s", mainListener.Addr().String())
		if serveErr := mainServer.Serve(mainListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("received shutdown signal")
	case serveErr := <-errCh:
		log.Printf("server error: %v", serveErr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := mainServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("main server shutdown error: %v", err)
	}
	if bypassServer != nil {
		if err := bypassServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("bypass server shutdown error: %v", err)
		}
	}
	log.Printf("shutdown complete")
}

func NewApp(configPath, configExamplePath string) (*App, error) {
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		raw, readErr := os.ReadFile(configExamplePath)
		if readErr != nil {
			return nil, fmt.Errorf("missing config.yml and unreadable config.example.yml: %w", readErr)
		}
		if writeErr := os.WriteFile(configPath, raw, 0o644); writeErr != nil {
			return nil, writeErr
		}
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, err
	}
	fillDefaults(cfg)

	app := &App{
		cfgPath:    configPath,
		cfg:        cfg,
		bot:        NewNapcatClient(cfg.WebsocketURL, cfg.WebsocketToken, cfg.LocalTestMode),
		jm:         NewJMBridge(cfg.JMOptionPath, cfg.FileDir, cfg.MangaDir, cfg.CBZDir, cfg.DownloadTimeout, cfg.LocalTestMode),
		queue:      make(chan DownloadTask, 1024),
		recent:     map[string]map[string]time.Time{},
		search:     map[string]PendingSearch{},
		jmEnabled:  true,
		soutuArmed: map[string]time.Time{},
	}
	app.jm.SetCBZOptions(cfg.CBZChapterEnabled, cfg.CBZSeriesEnabled)
	return app, nil
}

func fillDefaults(cfg *Config) {
	if cfg.HTTPHost == "" {
		cfg.HTTPHost = "0.0.0.0"
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 8071
	}
	if cfg.WebsocketURL == "" {
		cfg.WebsocketURL = "ws://127.0.0.1:13001"
	}
	if cfg.FileDir == "" {
		cfg.FileDir = "./pdf/"
	}
	if cfg.MangaDir == "" {
		cfg.MangaDir = "./manga/"
	}
	if cfg.CBZDir == "" {
		cfg.CBZDir = "./cbz/"
	}
	if cfg.LogDir == "" {
		cfg.LogDir = "./logs"
	}
	if cfg.TransferMode == "" {
		cfg.TransferMode = "scp"
	}
	if cfg.DownloadTimeout == 0 {
		cfg.DownloadTimeout = 1800
	}
	if cfg.SearchTimeout == 0 {
		cfg.SearchTimeout = 600
	}
	if cfg.MaxEpisodes == 0 {
		cfg.MaxEpisodes = 20
	}
	if cfg.DedupWindow == 0 {
		cfg.DedupWindow = 12 * 60 * 60
	}
	if cfg.RandomPasswordLength <= 0 {
		cfg.RandomPasswordLength = 10
	}
	if cfg.SoutuTriggerWindow <= 0 {
		cfg.SoutuTriggerWindow = 120
	}
	if cfg.SoutuGlobalM <= 0 {
		cfg.SoutuGlobalM = 3331358690401
	}
	if cfg.SoutuFactor <= 0 {
		cfg.SoutuFactor = 1.2
	}
	if strings.TrimSpace(cfg.SoutuURL) == "" {
		cfg.SoutuURL = "https://soutubot.moe"
	}
	cfg.SoutuURL = strings.TrimRight(strings.TrimSpace(cfg.SoutuURL), "/")
	if strings.TrimSpace(cfg.SoutuAPI) == "" {
		cfg.SoutuAPI = cfg.SoutuURL + "/api/search"
	}
	if strings.TrimSpace(cfg.SoutuUserAgent) == "" {
		cfg.SoutuUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	}
	if strings.TrimSpace(cfg.CFBypassAPIURL) == "" {
		cfg.CFBypassAPIURL = "http://127.0.0.1:8000/api/v1/bypass"
	}
	if cfg.CFBypassPollInterval <= 0 {
		cfg.CFBypassPollInterval = 2
	}
	if cfg.CFBypassPollTimeout <= 0 {
		cfg.CFBypassPollTimeout = 120
	}
	if cfg.EmbeddedBypassHost == "" {
		cfg.EmbeddedBypassHost = "127.0.0.1"
	}
	if cfg.EmbeddedBypassPort <= 0 {
		cfg.EmbeddedBypassPort = 18000
	}
	if cfg.PortFallbackTries <= 0 {
		cfg.PortFallbackTries = 20
	}
	if !cfg.EmbeddedBypassEnable && shouldUseEmbeddedBypassURL(cfg.CFBypassAPIURL) {
		cfg.EmbeddedBypassEnable = true
	}
	if cfg.SendModeGlobal == "" {
		cfg.SendModeGlobal = "pdf"
	}
	if cfg.SendNameModeGlobal == "" {
		cfg.SendNameModeGlobal = "full"
	}
	if cfg.SendModeGroup == nil {
		cfg.SendModeGroup = map[string]string{}
	}
	if cfg.SendNameModeGroup == nil {
		cfg.SendNameModeGroup = map[string]string{}
	}
	if cfg.EncEnabledGroup == nil {
		cfg.EncEnabledGroup = map[string]bool{}
	}
	if cfg.EncPasswordGroup == nil {
		cfg.EncPasswordGroup = map[string]string{}
	}
	if cfg.RandomPasswordEnabledGroup == nil {
		cfg.RandomPasswordEnabledGroup = map[string]bool{}
	}
	if cfg.RegexEnabledGroup == nil {
		cfg.RegexEnabledGroup = map[string]bool{}
	}
	if strings.TrimSpace(cfg.CardNickname) == "" {
		cfg.CardNickname = "文件助手"
	}
	if cfg.CardUserID == 0 {
		if cfg.AdminID > 0 {
			cfg.CardUserID = cfg.AdminID
		} else {
			cfg.CardUserID = 10000
		}
	}
}

func serveHTTP(name string, srv *http.Server, ln net.Listener) {
	log.Printf("%s listening at %s", name, ln.Addr().String())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("%s serve error: %v", name, err)
	}
}

func listenWithFallback(host string, preferredPort, tries int) (net.Listener, int, error) {
	if tries <= 0 {
		tries = 1
	}
	lastErr := error(nil)
	for i := 0; i < tries; i++ {
		p := preferredPort + i
		addr := fmt.Sprintf("%s:%d", host, p)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, p, nil
		}
		lastErr = err
		if !isAddrInUseErr(err) {
			return nil, 0, err
		}
	}
	return nil, 0, fmt.Errorf("failed to bind %s:%d after %d tries: %w", host, preferredPort, tries, lastErr)
}

func isAddrInUseErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use")
}

func shouldUseEmbeddedBypassURL(current string) bool {
	v := strings.TrimSpace(current)
	if v == "" || strings.EqualFold(v, "auto") {
		return true
	}
	return v == "http://127.0.0.1:8000/api/v1/bypass"
}

func clientHost(host string) string {
	h := strings.TrimSpace(host)
	if h == "" || h == "0.0.0.0" || h == "::" {
		return "127.0.0.1"
	}
	return h
}

func installSystemdService(serviceName, serviceUser, serviceGroup string) error {
	if os.Geteuid() != 0 {
		return errors.New("install requires root, run with sudo")
	}
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" || strings.Contains(serviceName, " ") || strings.Contains(serviceName, "/") {
		return fmt.Errorf("invalid service name: %q", serviceName)
	}

	userName, groupName, err := resolveServiceUserGroup(serviceUser, serviceGroup)
	if err != nil {
		return err
	}
	workDir, err := os.Getwd()
	if err != nil {
		return err
	}
	execPath, err := detectServiceExecPath(workDir)
	if err != nil {
		return err
	}

	servicePath := filepath.Join("/etc/systemd/system", serviceName+".service")
	content := renderSystemdService(serviceName, userName, groupName, workDir, execPath)
	if err := os.WriteFile(servicePath, []byte(content), 0o644); err != nil {
		return err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", serviceName); err != nil {
		return err
	}
	return nil
}

func uninstallSystemdService(serviceName string) error {
	if os.Geteuid() != 0 {
		return errors.New("uninstall requires root, run with sudo")
	}
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return errors.New("service name is required")
	}
	_ = runSystemctl("disable", "--now", serviceName)
	servicePath := filepath.Join("/etc/systemd/system", serviceName+".service")
	if err := os.Remove(servicePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	return nil
}

func resolveServiceUserGroup(serviceUser, serviceGroup string) (string, string, error) {
	userName := strings.TrimSpace(serviceUser)
	if userName == "" {
		userName = strings.TrimSpace(os.Getenv("SUDO_USER"))
	}
	if userName == "" {
		cur, err := user.Current()
		if err != nil {
			return "", "", err
		}
		userName = cur.Username
	}

	u, err := user.Lookup(userName)
	if err != nil {
		return "", "", fmt.Errorf("lookup user %s: %w", userName, err)
	}
	groupName := strings.TrimSpace(serviceGroup)
	if groupName == "" {
		g, gErr := user.LookupGroupId(u.Gid)
		if gErr != nil {
			return "", "", fmt.Errorf("lookup group id %s: %w", u.Gid, gErr)
		}
		groupName = g.Name
	}
	return userName, groupName, nil
}

func detectServiceExecPath(workDir string) (string, error) {
	preferred := filepath.Join(workDir, "napcat-jm-go")
	if st, err := os.Stat(preferred); err == nil && !st.IsDir() {
		return preferred, nil
	}
	execPath, err := os.Executable()
	if err != nil {
		return "", err
	}
	if strings.Contains(execPath, "/go-build") {
		return "", errors.New("running via go run without ./napcat-jm-go binary; build first with: go build -o napcat-jm-go .")
	}
	return execPath, nil
}

func renderSystemdService(serviceName, userName, groupName, workDir, execPath string) string {
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s
ExecStart=%s
Restart=always
RestartSec=3
KillSignal=SIGTERM
TimeoutStopSec=20
StandardOutput=journal
StandardError=journal
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`, serviceName, userName, groupName, workDir, execPath)
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func newEmbeddedBypassService() *embeddedBypassService {
	return &embeddedBypassService{
		cache:   map[string]embeddedCacheItem{},
		running: map[string]bool{},
	}
}

func (s *embeddedBypassService) handleBypassV1(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var q bypassQuery
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": err.Error()})
		return
	}
	q.URL = strings.TrimSpace(q.URL)
	if q.URL == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "url is required"})
		return
	}
	parsedURL, err := url.Parse(q.URL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "url is invalid"})
		return
	}

	cacheKey := fmt.Sprintf("%s|%s|%s", q.URL, q.UserAgent, q.ProxyServer)
	now := time.Now()

	s.mu.Lock()
	if cached, ok := s.cache[cacheKey]; ok && now.Before(cached.ExpiresAt) {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, cached.Data)
		return
	}
	if !s.running[cacheKey] {
		s.running[cacheKey] = true
		polling := bypassResponse{Message: "正在解密 cloudflare 5s 盾，请继续轮询"}
		s.cache[cacheKey] = embeddedCacheItem{Data: polling, ExpiresAt: now.Add(60 * time.Second)}
		go s.solve(cacheKey, q)
	}
	out := s.cache[cacheKey].Data
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, out)
}

func (s *embeddedBypassService) solve(cacheKey string, q bypassQuery) {
	result, err := runCloudflareBypass(q)

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, cacheKey)

	if err != nil {
		s.cache[cacheKey] = embeddedCacheItem{
			Data:      bypassResponse{Message: "查询失败，请1分钟后再次尝试"},
			ExpiresAt: time.Now().Add(60 * time.Second),
		}
		log.Printf("embedded bypass failed: %v", err)
		return
	}
	result.Message = "ok"
	s.cache[cacheKey] = embeddedCacheItem{
		Data:      result,
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}
}

func runCloudflareBypass(q bypassQuery) (bypassResponse, error) {
	browserPath, err := resolveChromeExecPath()
	if err != nil {
		return bypassResponse{}, err
	}

	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(browserPath),
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("export-tagged-pdf", true),
		chromedp.Flag("disable-background-mode", true),
		chromedp.Flag("enable-features", "NetworkService,NetworkServiceInProcess,LoadCryptoTokenExtension,PermuteTLSExtensions"),
		chromedp.Flag("disable-features", "FlashDeprecationWarning,EnablePasswordsAccountStorage"),
		chromedp.Flag("deny-permission-prompts", true),
		chromedp.DisableGPU,
		chromedp.Flag("accept-lang", "en-US"),
	}
	if runtime.GOOS == "linux" {
		opts = append(opts, chromedp.Headless)
		opts = append(opts, chromedp.Flag("no-sandbox", true))
	}
	if strings.TrimSpace(q.UserAgent) != "" {
		opts = append(opts, chromedp.UserAgent(strings.TrimSpace(q.UserAgent)))
	}
	if strings.TrimSpace(q.ProxyServer) != "" {
		opts = append(opts, chromedp.ProxyServer(strings.TrimSpace(q.ProxyServer)))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(func(string, ...any) {}))
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, 180*time.Second)
	defer timeoutCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(q.URL)); err != nil {
		return bypassResponse{}, fmt.Errorf("navigate: %w", err)
	}
	// Cloudflare 5s challenge usually resolves automatically without clicks.
	time.Sleep(6 * time.Second)

	var userAgent string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`navigator.userAgent`, &userAgent)); err != nil {
		return bypassResponse{}, fmt.Errorf("get user agent: %w", err)
	}

	for i := 0; i < 75; i++ {
		// Poll cookies; for default 5s shield cf_clearance appears automatically.
		if i > 0 {
			time.Sleep(2 * time.Second)
		}

		var cookies []*network.Cookie
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			cookies, err = network.GetCookies().Do(ctx)
			return err
		})); err != nil {
			continue
		}

		found := false
		out := bypassResponse{UserAgent: userAgent, Cookies: make([]bypassCookie, 0, len(cookies))}
		for _, ck := range cookies {
			if ck.Name == "cf_clearance" {
				found = true
			}
			out.Cookies = append(out.Cookies, bypassCookie{Name: ck.Name, Value: ck.Value})
		}
		if found {
			return out, nil
		}
	}

	return bypassResponse{}, errors.New("no cf_clearance cookie acquired after waiting challenge window (~150s)")
}

func resolveChromeExecPath() (string, error) {
	candidates := []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"chrome",
	}
	for _, name := range candidates {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}

	absoluteCandidates := []string{
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/opt/google/chrome/chrome",
	}
	for _, p := range absoluteCandidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("no chrome/chromium executable found for embedded bypass; install chromium or set cf_bypass_api_url to an external bypass service")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) runLocalSelfTest() error {
	cfg := a.currentConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	testID := "123456"
	album, err := a.jm.GetAlbum(ctx, testID)
	if err != nil {
		return fmt.Errorf("get album failed: %w", err)
	}

	testOut := filepath.Join(cfg.FileDir, fmt.Sprintf("%s_selftest.pdf", album.ID))
	if err := a.jm.DownloadTo(ctx, testID, testOut, "1234"); err != nil {
		return fmt.Errorf("download test pdf failed: %w", err)
	}
	defer os.Remove(testOut)

	if !a.bot.SendPrivateMessage(cfg.AdminID, "本地自测：文本发送通过") {
		return errors.New("send text failed")
	}
	if !a.bot.SendPrivateFile(cfg, cfg.AdminID, testOut) {
		return errors.New("send file failed")
	}
	return nil
}

func (a *App) saveConfig() {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	raw, err := yaml.Marshal(a.cfg)
	if err != nil {
		log.Printf("marshal config failed: %v", err)
		return
	}
	if err := os.WriteFile(a.cfgPath, raw, 0o644); err != nil {
		log.Printf("write config failed: %v", err)
	}
}

func (a *App) handleHTTPEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	go a.handleMessageEvent(data)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"success"}`))
}

func (a *App) handleMessageEvent(data map[string]any) {
	if toString(data["post_type"]) != "message" {
		return
	}
	messageType := toString(data["message_type"])
	rawMessage := strings.TrimSpace(toString(data["raw_message"]))
	userID := toInt64(data["user_id"])
	groupID := toInt64(data["group_id"])
	scope := requestScope(messageType, groupID, userID)
	soutuScopeKey := requestSoutuScope(messageType, groupID, userID)

	if a.handleSoutuArmingCommand(rawMessage, messageType, groupID, userID, soutuScopeKey) {
		return
	}
	if a.tryHandleSoutuImage(data, messageType, groupID, userID, soutuScopeKey) {
		return
	}

	if rawMessage == "" {
		return
	}

	if matched(`^/jm\s+help$`, rawMessage) {
		a.sendMessage(messageType, groupID, userID, helpMessage())
		return
	}
	if m := mustMatch(`^/jm\s+mode\s+(pdf|zip)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可设置发送格式") {
			return
		}
		a.cfgMu.Lock()
		if messageType == "group" && groupID > 0 {
			a.cfg.SendModeGroup[strconv.FormatInt(groupID, 10)] = m[1]
		} else {
			a.cfg.SendModeGlobal = m[1]
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "发送格式已更新")
		return
	}
	if m := mustMatch(`^/jm\s+fname\s+(jm|full)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可设置发送文件命名方式") {
			return
		}
		a.cfgMu.Lock()
		if messageType == "group" && groupID > 0 {
			a.cfg.SendNameModeGroup[strconv.FormatInt(groupID, 10)] = m[1]
		} else {
			a.cfg.SendNameModeGlobal = m[1]
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "发送文件命名方式已更新")
		return
	}
	if m := mustMatch(`^/jm\s+enc\s+(on|off)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可设置加密开关") {
			return
		}
		enabled := m[1] == "on"
		a.cfgMu.Lock()
		if messageType == "group" && groupID > 0 {
			a.cfg.EncEnabledGroup[strconv.FormatInt(groupID, 10)] = enabled
		} else {
			a.cfg.EncEnabledGlobal = enabled
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "加密开关已更新")
		return
	}
	if m := mustMatch(`^/jm\s+passwd\s+(.+)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可设置加密密码") {
			return
		}
		pw := strings.TrimSpace(m[1])
		if pw == "" {
			a.sendMessage(messageType, groupID, userID, "密码不能为空")
			return
		}
		a.cfgMu.Lock()
		if messageType == "group" && groupID > 0 {
			a.cfg.EncPasswordGroup[strconv.FormatInt(groupID, 10)] = pw
		} else {
			a.cfg.EncPasswordGlobal = pw
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "加密密码已设置")
		return
	}
	if m := mustMatch(`^/jm\s+randpwd\s+(on|off)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可设置随机密码开关") {
			return
		}
		enabled := m[1] == "on"
		a.cfgMu.Lock()
		if messageType == "group" && groupID > 0 {
			a.cfg.RandomPasswordEnabledGroup[strconv.FormatInt(groupID, 10)] = enabled
		} else {
			a.cfg.RandomPasswordEnabledGlobal = enabled
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "随机密码开关已更新")
		return
	}
	if m := mustMatch(`^/jm\s+regex\s+(on|off)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可设置正则模式") {
			return
		}
		enabled := m[1] == "on"
		a.cfgMu.Lock()
		if messageType == "group" && groupID > 0 {
			a.cfg.RegexEnabledGroup[strconv.FormatInt(groupID, 10)] = enabled
		} else {
			a.cfg.RegexEnabledGlobal = enabled
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "正则模式已更新")
		return
	}
	if matched(`^/jm\s+goodluck$`, rawMessage) || matched(`^/goodluck$`, rawMessage) || rawMessage == "随机本子" {
		id := randomJMID()
		a.sendMessage(messageType, groupID, userID, "随机本子ID：JM"+id)
		a.enqueueDownloads([]string{id}, messageType, groupID, userID, data)
		return
	}
	if matched(`^/jm\s+on$`, rawMessage) {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可操作") {
			return
		}
		a.cfgMu.Lock()
		if groupID > 0 {
			a.cfg.BannedGroup = removeStr(a.cfg.BannedGroup, strconv.FormatInt(groupID, 10))
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "禁漫功能已开启")
		return
	}
	if matched(`^/jm\s+off$`, rawMessage) {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可操作") {
			return
		}
		a.cfgMu.Lock()
		if groupID > 0 && !contains(a.cfg.BannedGroup, strconv.FormatInt(groupID, 10)) {
			a.cfg.BannedGroup = append(a.cfg.BannedGroup, strconv.FormatInt(groupID, 10))
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "禁漫功能已关闭")
		return
	}
	if m := mustMatch(`^/jm\s+addban\s+(\d+)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可操作") {
			return
		}
		a.cfgMu.Lock()
		if !contains(a.cfg.BannedID, m[1]) {
			a.cfg.BannedID = append(a.cfg.BannedID, m[1])
		}
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "已封禁本子ID："+m[1])
		return
	}
	if m := mustMatch(`^/jm\s+delban\s+(\d+)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可操作") {
			return
		}
		a.cfgMu.Lock()
		a.cfg.BannedID = removeStr(a.cfg.BannedID, m[1])
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, "已解封本子ID："+m[1])
		return
	}
	if m := mustMatch(`^/jm\s+setmax\s+(\d+)$`, rawMessage); m != nil {
		if !a.requireAdmin(messageType, groupID, userID, "仅管理员可操作") {
			return
		}
		n, _ := strconv.Atoi(m[1])
		a.cfgMu.Lock()
		a.cfg.MaxEpisodes = n
		a.cfgMu.Unlock()
		a.saveConfig()
		a.sendMessage(messageType, groupID, userID, fmt.Sprintf("章节数阈值已设为 %d", n))
		return
	}
	if m := mustMatch(`^/jm\s+look\s+(\d+)$`, rawMessage); m != nil {
		if !a.isJMAllowed(messageType, groupID, userID) {
			return
		}
		a.sendMessage(messageType, groupID, userID, "正在检索本子 "+m[1])
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		al, err := a.jm.GetAlbum(ctx, m[1])
		if err != nil {
			a.sendMessage(messageType, groupID, userID, "查询失败")
			return
		}
		msg := fmt.Sprintf("ID：%s\n标题：%s\n描述：%s\n标签：%s\n章节：%d\n浏览：%s", al.ID, al.Title, al.Description, strings.Join(al.Tags, ", "), al.Episodes, al.Views)
		a.sendRecordMessage(messageType, groupID, userID, msg)
		return
	}
	if m := mustMatch(`^/jm\s+search\s+(.+)$`, rawMessage); m != nil {
		if !a.isJMAllowed(messageType, groupID, userID) {
			return
		}
		keyword := normalizeSearchKeyword(m[1])
		a.sendMessage(messageType, groupID, userID, "正在搜索："+keyword+" ...")
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		al, err := a.jm.SearchBestAlbum(ctx, keyword)
		if err != nil || al == nil {
			a.sendMessage(messageType, groupID, userID, "未找到相关本子")
			return
		}
		a.searchMu.Lock()
		a.search[scope] = PendingSearch{AlbumID: al.ID, Title: al.Title, At: time.Now()}
		a.searchMu.Unlock()
		a.sendRecordMessage(messageType, groupID, userID, fmt.Sprintf("找到最佳匹配：\nID：JM%s\n标题：%s\n是否下载？请在10分钟内回复“确认”", al.ID, al.Title))
		return
	}
	if rawMessage == "确认" {
		a.searchMu.Lock()
		pending, ok := a.search[scope]
		if ok {
			if time.Since(pending.At) <= time.Duration(a.cfg.SearchTimeout)*time.Second {
				delete(a.search, scope)
				a.searchMu.Unlock()
				a.sendMessage(messageType, groupID, userID, "已确认，开始处理本子："+pending.Title)
				a.enqueueDownloads([]string{pending.AlbumID}, messageType, groupID, userID, data)
				return
			}
			delete(a.search, scope)
		}
		a.searchMu.Unlock()
		return
	}

	regexEnabled := a.getRegexEnabled(messageType, groupID)
	numbers := extractJMNumbersFromEvent(data, regexEnabled)
	if len(numbers) > 0 {
		a.enqueueDownloads(numbers, messageType, groupID, userID, data)
	}
}

func (a *App) requireAdmin(messageType string, groupID, userID int64, deny string) bool {
	a.cfgMu.RLock()
	admin := a.cfg.AdminID
	a.cfgMu.RUnlock()
	if userID != admin {
		a.sendMessage(messageType, groupID, userID, deny)
		return false
	}
	return true
}

func (a *App) isJMAllowed(messageType string, groupID, userID int64) bool {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	if messageType == "group" && contains(a.cfg.BannedGroup, strconv.FormatInt(groupID, 10)) {
		a.sendMessage(messageType, groupID, userID, "禁漫功能未开启")
		return false
	}
	if contains(a.cfg.BannedUser, strconv.FormatInt(userID, 10)) {
		a.sendMessage(messageType, groupID, userID, "禁止下载或用户被封禁")
		return false
	}
	return true
}

func (a *App) getRegexEnabled(messageType string, groupID int64) bool {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	if messageType == "group" {
		if v, ok := a.cfg.RegexEnabledGroup[strconv.FormatInt(groupID, 10)]; ok {
			return v
		}
	}
	return a.cfg.RegexEnabledGlobal
}

func (a *App) enqueueDownloads(numbers []string, messageType string, groupID, userID int64, data map[string]any) {
	if !a.isJMAllowed(messageType, groupID, userID) {
		return
	}
	cfg := a.currentConfig()
	scope := requestScope(messageType, groupID, userID)
	queued := 0
	for _, n := range numbers {
		if contains(cfg.BannedID, n) {
			a.sendMessage(messageType, groupID, userID, "禁止下载或用户被封禁")
			continue
		}
		if a.isRecentRequest(scope, n, time.Duration(cfg.DedupWindow)*time.Second) {
			if len(n) >= 4 {
				a.sendMessage(messageType, groupID, userID, "本子 "+n+" 在过去12小时内已请求过，已跳过")
			}
			continue
		}
		a.markRequest(scope, n)
		nickname := toString(mapGet(mapGet(data, "sender"), "nickname"))
		task := DownloadTask{Number: n, MessageType: messageType, GroupID: groupID, UserID: userID, Scope: scope, Uploader: nickname}
		select {
		case a.queue <- task:
			queued++
		default:
			a.sendMessage(messageType, groupID, userID, "下载队列已满，请稍后重试")
			return
		}
	}
	if queued > 0 {
		a.sendMessage(messageType, groupID, userID, fmt.Sprintf("已加入队列，正在下载 %d 个本子，当前队列：%d", queued, len(a.queue)))
	}
}

func (a *App) worker() {
	for task := range a.queue {
		a.processTask(task)
	}
}

func (a *App) processTask(task DownloadTask) {
	cfg := a.currentConfig()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.DownloadTimeout)*time.Second)
	defer cancel()

	album, err := a.jm.GetAlbum(ctx, task.Number)
	if err != nil || album == nil {
		if len(task.Number) < 4 {
			return
		}
		a.sendMessage(task.MessageType, task.GroupID, task.UserID, "未能成功下载（可能ID错误或网络失败）")
		return
	}
	if album.Episodes > cfg.MaxEpisodes {
		a.sendMessage(task.MessageType, task.GroupID, task.UserID, fmt.Sprintf("本子章节过多(>%d)", cfg.MaxEpisodes))
		return
	}

	path, name := findPDF(cfg.FileDir, task.Number, album.Title)
	if path == "" {
		a.sendMessage(task.MessageType, task.GroupID, task.UserID, "正在下载本子 "+task.Number)
		if err := a.jm.Download(ctx, task.Number); err != nil {
			a.sendMessage(task.MessageType, task.GroupID, task.UserID, "下载失败或超时")
			return
		}
		path, name = findPDF(cfg.FileDir, task.Number, album.Title)
		if path == "" {
			a.sendMessage(task.MessageType, task.GroupID, task.UserID, "下载完成但未找到PDF文件")
			return
		}
	}

	sendMode := cfg.SendModeGlobal
	if task.MessageType == "group" {
		if v, ok := cfg.SendModeGroup[strconv.FormatInt(task.GroupID, 10)]; ok {
			sendMode = v
		}
	}
	nameMode := cfg.SendNameModeGlobal
	if task.MessageType == "group" {
		if v, ok := cfg.SendNameModeGroup[strconv.FormatInt(task.GroupID, 10)]; ok {
			nameMode = v
		}
	}

	sendPath := path
	cleanup := []string{}

	encEnabled := cfg.EncEnabledGlobal
	if task.MessageType == "group" {
		if v, ok := cfg.EncEnabledGroup[strconv.FormatInt(task.GroupID, 10)]; ok {
			encEnabled = v
		}
	}
	password := ""
	randomPasswordEnabled := cfg.RandomPasswordEnabledGlobal
	if task.MessageType == "group" {
		if v, ok := cfg.RandomPasswordEnabledGroup[strconv.FormatInt(task.GroupID, 10)]; ok {
			randomPasswordEnabled = v
		}
	}
	if randomPasswordEnabled {
		password = randomPassword(cfg.RandomPasswordLength)
	} else {
		password = strings.TrimSpace(cfg.EncPasswordGlobal)
		if task.MessageType == "group" {
			if v, ok := cfg.EncPasswordGroup[strconv.FormatInt(task.GroupID, 10)]; ok {
				password = strings.TrimSpace(v)
			}
		}
	}
	if encEnabled {
		if password == "" {
			a.sendMessage(task.MessageType, task.GroupID, task.UserID, "未设置加密密码，请先使用 /jm passwd <密码> 设置")
			return
		}
		encOut := filepath.Join(os.TempDir(), fmt.Sprintf("enc_%s_%d.pdf", task.Number, time.Now().UnixNano()))
		if err := a.jm.DownloadTo(ctx, task.Number, encOut, password); err != nil {
			a.sendMessage(task.MessageType, task.GroupID, task.UserID, "文件加密失败")
			return
		}
		cleanup = append(cleanup, encOut)
		sendPath = encOut
	}

	if sendMode == "zip" {
		zipPath, err := buildZip(sendPath)
		if err != nil {
			a.sendMessage(task.MessageType, task.GroupID, task.UserID, "文件压缩失败")
			return
		}
		cleanup = append(cleanup, zipPath)
		sendPath = zipPath
	}

	baseName := sanitizeFileName(album.Title)
	if nameMode == "jm" {
		baseName = "JM" + task.Number
	}
	renamed, renamedCleanup, err := cloneWithName(sendPath, baseName)
	if err == nil && renamed != sendPath {
		sendPath = renamed
		if renamedCleanup {
			cleanup = append(cleanup, renamed)
		}
	}

	hashPath, hashCleanup, err := randomizeHash(sendPath)
	if err == nil && hashCleanup {
		sendPath = hashPath
		cleanup = append(cleanup, hashPath)
	}

	sizeMB := fileSizeMB(sendPath)
	label := "PDF"
	if strings.HasSuffix(strings.ToLower(sendPath), ".zip") {
		label = "ZIP"
	}
	msg := fmt.Sprintf("正在发送：\n车牌号：%s\n本子名：%s\n文件类型：%s\n文件大小：(%.2fMB)", task.Number, album.Title, label, sizeMB)
	if encEnabled {
		msg += "\n密码：" + password
	}
	a.sendRecordMessage(task.MessageType, task.GroupID, task.UserID, msg)

	ok := false
	if task.MessageType == "group" {
		ok = a.bot.SendGroupFile(cfg, task.GroupID, sendPath)
	} else {
		ok = a.bot.SendPrivateFile(cfg, task.UserID, sendPath)
	}
	if !ok {
		a.sendMessage(task.MessageType, task.GroupID, task.UserID, "文件发送失败")
	}

	for _, c := range cleanup {
		_ = os.Remove(c)
	}
	_ = name
}

func (a *App) sendMessage(messageType string, groupID, userID int64, message string) {
	if messageType == "group" && groupID > 0 {
		_ = a.bot.SendGroupMessage(groupID, message)
		return
	}
	if messageType == "private" && userID > 0 {
		_ = a.bot.SendPrivateMessage(userID, message)
	}
}

func (a *App) sendRecordMessage(messageType string, groupID, userID int64, message string) {
	cfg := a.currentConfig()
	if messageType == "group" && groupID > 0 {
		if a.bot.SendGroupForwardCardMessage(groupID, message, cfg.CardUserID, cfg.CardNickname) {
			return
		}
	}
	if messageType == "private" && userID > 0 {
		if a.bot.SendPrivateForwardCardMessage(userID, message, cfg.CardUserID, cfg.CardNickname) {
			return
		}
	}
	a.sendMessage(messageType, groupID, userID, message)
}

func (a *App) currentConfig() Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	cp := *a.cfg
	return cp
}

func (a *App) isRecentRequest(scope, number string, window time.Duration) bool {
	a.recentMu.Lock()
	defer a.recentMu.Unlock()
	now := time.Now()
	m := a.recent[scope]
	if m == nil {
		return false
	}
	for k, t := range m {
		if now.Sub(t) > window {
			delete(m, k)
		}
	}
	t, ok := m[number]
	if !ok {
		return false
	}
	return now.Sub(t) <= window
}

func (a *App) markRequest(scope, number string) {
	a.recentMu.Lock()
	defer a.recentMu.Unlock()
	if a.recent[scope] == nil {
		a.recent[scope] = map[string]time.Time{}
	}
	a.recent[scope][number] = time.Now()
}

func (a *App) handleSoutuArmingCommand(rawMessage, messageType string, groupID, userID int64, scope string) bool {
	if !(matched(`^/jm\s+search$`, rawMessage) || matched(`^识图$`, rawMessage) || matched(`^/jm识图$`, rawMessage) || matched(`^/jm\s+识图$`, rawMessage)) {
		return false
	}
	cfg := a.currentConfig()
	a.soutuMu.Lock()
	a.soutuArmed[scope] = time.Now().Add(time.Duration(cfg.SoutuTriggerWindow) * time.Second)
	a.soutuMu.Unlock()
	a.sendMessage(messageType, groupID, userID, fmt.Sprintf("已进入识图模式，请在 %d 秒内发送图片", cfg.SoutuTriggerWindow))
	return true
}

func (a *App) tryHandleSoutuImage(data map[string]any, messageType string, groupID, userID int64, scope string) bool {
	cfg := a.currentConfig()
	now := time.Now()
	a.soutuMu.Lock()
	deadline, ok := a.soutuArmed[scope]
	if ok && now.After(deadline) {
		delete(a.soutuArmed, scope)
		ok = false
	}
	if !ok {
		a.soutuMu.Unlock()
		return false
	}

	imageURLs := extractImageURLsFromEvent(data)
	if len(imageURLs) == 0 {
		a.soutuMu.Unlock()
		return false
	}
	delete(a.soutuArmed, scope)
	a.soutuMu.Unlock()

	a.sendMessage(messageType, groupID, userID, "正在识图，请稍候...")
	for _, u := range imageURLs {
		result, err := searchSoutuByImageURL(u, cfg)
		if err != nil {
			log.Printf("soutu failed for %s: %v", u, err)
			a.sendMessage(messageType, groupID, userID, "识图失败："+briefError(err))
			continue
		}
		a.sendMessage(messageType, groupID, userID, formatSoutuResult(result))
	}
	return true
}

func requestScope(messageType string, groupID, userID int64) string {
	if messageType == "group" && groupID > 0 {
		return fmt.Sprintf("group:%d", groupID)
	}
	return fmt.Sprintf("private:%d", userID)
}

func requestSoutuScope(messageType string, groupID, userID int64) string {
	if messageType == "group" && groupID > 0 {
		return fmt.Sprintf("group:%d:user:%d", groupID, userID)
	}
	return fmt.Sprintf("private:%d", userID)
}

func randomJMID() string {
	l := 3 + randIntN(7)
	var b strings.Builder
	b.WriteString(strconv.Itoa(1 + randIntN(9)))
	for i := 1; i < l; i++ {
		b.WriteString(strconv.Itoa(randIntN(10)))
	}
	return b.String()
}

func randIntN(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(n))
	}
	return int(v.Int64())
}

func randomPassword(length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if length < 4 {
		length = 4
	}
	var b strings.Builder
	for i := 0; i < length; i++ {
		b.WriteByte(alphabet[randIntN(len(alphabet))])
	}
	return b.String()
}

func normalizeSearchKeyword(k string) string {
	s := strings.TrimSpace(htmlUnescape(k))
	re := regexp.MustCompile(`^\s*(?:\([^\)]*\)|\[[^\]]*\])\s*`)
	for {
		u := re.ReplaceAllString(s, "")
		if u == s {
			break
		}
		s = strings.TrimSpace(u)
	}
	return s
}

func htmlUnescape(s string) string {
	replacer := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")
	return replacer.Replace(s)
}

func findPDF(dir, number, title string) (string, string) {
	if number != "" {
		for _, n := range []string{number + ".pdf", "JM" + number + ".pdf"} {
			p := filepath.Join(dir, n)
			if fileExists(p) {
				return p, n
			}
		}
	}
	if title != "" {
		n := sanitizeFileName(title) + ".pdf"
		p := filepath.Join(dir, n)
		if fileExists(p) {
			return p, n
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(strings.ToLower(name), ".pdf") && strings.Contains(name, number) {
			return filepath.Join(dir, name), name
		}
	}
	return "", ""
}

func sanitizeFileName(s string) string {
	r := regexp.MustCompile(`[\\/:*?"<>|]+`)
	s = r.ReplaceAllString(s, "_")
	s = strings.TrimSpace(strings.Trim(s, "."))
	if s == "" {
		return "JM"
	}
	return s
}

func buildZip(filePath string) (string, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("%d_%s.zip", time.Now().UnixNano(), sanitizeFileName(strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath)))))
	cmd := exec.Command("zip", "-j", "-q", tmp, filePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("zip failed: %v: %s", err, string(out))
	}
	return tmp, nil
}

func cloneWithName(filePath, baseName string) (string, bool, error) {
	ext := filepath.Ext(filePath)
	newPath := filepath.Join(os.TempDir(), fmt.Sprintf("%s_%d%s", sanitizeFileName(baseName), time.Now().UnixNano(), ext))
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return filePath, false, err
	}
	if err := os.WriteFile(newPath, raw, 0o644); err != nil {
		return filePath, false, err
	}
	return newPath, true, nil
}

func randomizeHash(filePath string) (string, bool, error) {
	newPath := filepath.Join(os.TempDir(), fmt.Sprintf("hash_%d_%s", time.Now().UnixNano(), filepath.Base(filePath)))
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return filePath, false, err
	}
	if err := os.WriteFile(newPath, raw, 0o644); err != nil {
		return filePath, false, err
	}
	f, err := os.OpenFile(newPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return filePath, false, err
	}
	defer f.Close()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return filePath, false, err
	}
	if _, err := f.Write(append([]byte("\n"), buf...)); err != nil {
		return filePath, false, err
	}
	return newPath, true, nil
}

func fileSizeMB(path string) float64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return float64(st.Size()) / 1024.0 / 1024.0
}

func extractJMNumbersFromEvent(data map[string]any, regexEnabled bool) []string {
	text := extractTextFromEvent(data)
	if regexEnabled {
		re := regexp.MustCompile(`(?i)\bjm(\d+)\b`)
		m := re.FindAllStringSubmatch(text, -1)
		out := make([]string, 0, len(m))
		for _, g := range m {
			if len(g) > 1 {
				out = append(out, g[1])
			}
		}
		return unique(out)
	}
	re := regexp.MustCompile(`\d+`)
	return unique(re.FindAllString(stripCQCodes(text), -1))
}

func stripCQCodes(s string) string {
	re := regexp.MustCompile(`\[CQ:[^\]]*\]`)
	return re.ReplaceAllString(s, "")
}

func extractTextFromEvent(data map[string]any) string {
	if msg, ok := data["message"]; ok {
		switch t := msg.(type) {
		case string:
			return t
		case []any:
			var b strings.Builder
			for _, seg := range t {
				m, ok := seg.(map[string]any)
				if !ok {
					continue
				}
				if toString(m["type"]) != "text" {
					continue
				}
				dataMap := mapGet(m, "data")
				b.WriteString(toString(dataMap["text"]))
			}
			return b.String()
		}
	}
	return toString(data["raw_message"])
}

func extractImageURLsFromEvent(data map[string]any) []string {
	out := make([]string, 0, 2)
	appendCandidate := func(v string) {
		u := strings.TrimSpace(v)
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			out = append(out, u)
		}
	}
	parseSegments := func(msg []any) {
		for _, seg := range msg {
			m, ok := seg.(map[string]any)
			if !ok || toString(m["type"]) != "image" {
				continue
			}
			dataMap := mapGet(m, "data")
			appendCandidate(toString(dataMap["url"]))
			appendCandidate(toString(dataMap["file"]))
		}
	}
	if msg, ok := data["message"].([]any); ok {
		parseSegments(msg)
	}

	// Fallback for CQ-style message payloads.
	cqRaw := toString(data["raw_message"])
	if cqRaw == "" {
		cqRaw = toString(data["message"])
	}
	if cqRaw != "" {
		re := regexp.MustCompile(`url=([^,\]]+)`)
		matches := re.FindAllStringSubmatch(cqRaw, -1)
		for _, m := range matches {
			if len(m) > 1 {
				appendCandidate(m[1])
			}
		}
	}
	return out
}

func searchSoutuByImageURL(imageURL string, cfg Config) (map[string]any, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("download image status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	imageBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return searchSoutu(imageBytes, cfg, client)
}

func searchSoutu(imageBytes []byte, cfg Config, client *http.Client) (map[string]any, error) {
	cookies := currentCFCookies()
	result, status, respBody, err := searchSoutuOnce(imageBytes, cfg, client, cookies)
	if err != nil {
		return nil, err
	}
	if status >= 200 && status < 300 {
		return result, nil
	}
	if isCloudflareBlocked(status, respBody) {
		if err := ensureCfCookies(cfg, client); err == nil {
			result, status, respBody, err = searchSoutuOnce(imageBytes, cfg, client, currentCFCookies())
			if err != nil {
				return nil, err
			}
			if status >= 200 && status < 300 {
				return result, nil
			}
		} else {
			return nil, fmt.Errorf("cloudflare bypass failed: %w", err)
		}
	}
	return nil, fmt.Errorf("soutu status %d: %s", status, strings.TrimSpace(string(respBody)))
}

func searchSoutuOnce(imageBytes []byte, cfg Config, client *http.Client, cookies map[string]string) (map[string]any, int, []byte, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("factor", fmt.Sprintf("%g", cfg.SoutuFactor))
	fw, err := mw.CreateFormFile("file", "image.jpg")
	if err != nil {
		return nil, 0, nil, err
	}
	if _, err := fw.Write(imageBytes); err != nil {
		return nil, 0, nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, 0, nil, err
	}

	req, err := http.NewRequest(http.MethodPost, cfg.SoutuAPI, &body)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", cfg.SoutuUserAgent)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-API-KEY", generateSoutuAPIKey(cfg.SoutuUserAgent, cfg.SoutuGlobalM))
	req.Header.Set("Referer", cfg.SoutuURL)
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, respBody, nil
	}

	out := map[string]any{}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, resp.StatusCode, nil, err
	}
	return out, resp.StatusCode, nil, nil
}

func isCloudflareBlocked(status int, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusUnauthorized {
		return false
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "cloudflare") ||
		strings.Contains(text, "cf-mitigated") ||
		strings.Contains(text, "__cf_chl") ||
		strings.Contains(text, "just a moment") ||
		strings.Contains(text, "challenge-platform")
}

func ensureCfCookies(cfg Config, client *http.Client) error {
	soutuCFMu.RLock()
	if !soutuCFCookieExpires.IsZero() && time.Now().Before(soutuCFCookieExpires) && len(soutuCFCookies) > 0 {
		soutuCFMu.RUnlock()
		return nil
	}
	soutuCFMu.RUnlock()

	res, err := pollBypassAPI(cfg, client)
	if err != nil {
		return err
	}
	if !applyBypassResult(res) {
		return errors.New("invalid bypass result")
	}
	return nil
}

func pollBypassAPI(cfg Config, client *http.Client) (bypassResponse, error) {
	deadline := time.Now().Add(time.Duration(cfg.CFBypassPollTimeout * float64(time.Second)))
	interval := time.Duration(cfg.CFBypassPollInterval * float64(time.Second))
	if interval <= 0 {
		interval = 2 * time.Second
	}

	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := callBypassAPI(cfg, client)
		if err != nil {
			lastErr = err
			time.Sleep(interval)
			continue
		}
		if len(resp.Cookies) > 0 {
			return resp, nil
		}
		if strings.Contains(resp.Message, "继续轮询") {
			time.Sleep(interval)
			continue
		}
		lastErr = fmt.Errorf("bypass api returned message: %s", resp.Message)
		time.Sleep(interval)
	}
	if lastErr == nil {
		lastErr = errors.New("bypass api poll timeout")
	}
	return bypassResponse{}, lastErr
}

func callBypassAPI(cfg Config, client *http.Client) (bypassResponse, error) {
	payload := map[string]any{
		"url":        cfg.SoutuURL,
		"user_agent": cfg.SoutuUserAgent,
	}
	bs, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, cfg.CFBypassAPIURL, bytes.NewReader(bs))
	if err != nil {
		return bypassResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return bypassResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return bypassResponse{}, fmt.Errorf("bypass api status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out bypassResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return bypassResponse{}, err
	}
	return out, nil
}

func applyBypassResult(res bypassResponse) bool {
	if len(res.Cookies) == 0 {
		return false
	}
	cookies := map[string]string{}
	for _, c := range res.Cookies {
		if c.Name == "" || c.Value == "" {
			continue
		}
		cookies[c.Name] = c.Value
	}
	if len(cookies) == 0 {
		return false
	}
	soutuCFMu.Lock()
	soutuCFCookies = cookies
	soutuCFCookieExpires = time.Now().Add(25 * time.Minute)
	soutuCFMu.Unlock()
	return true
}

func currentCFCookies() map[string]string {
	soutuCFMu.RLock()
	defer soutuCFMu.RUnlock()
	out := make(map[string]string, len(soutuCFCookies))
	for k, v := range soutuCFCookies {
		out[k] = v
	}
	return out
}

func briefError(err error) string {
	if err == nil {
		return "未知错误"
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "未知错误"
	}
	msg = strings.ReplaceAll(msg, "\n", " ")
	r := []rune(msg)
	if len(r) > 80 {
		return string(r[:80]) + "..."
	}
	return msg
}

func generateSoutuAPIKey(userAgent string, globalM int64) string {
	unixTS := time.Now().Unix()
	uaLen := int64(len(userAgent))
	raw := strconv.FormatInt(unixTS*unixTS+uaLen*uaLen+globalM, 10)
	encoded := base64.StdEncoding.EncodeToString([]byte(raw))
	rev := reverseString(encoded)
	return strings.ReplaceAll(rev, "=", "")
}

func reverseString(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

func formatSoutuResult(result map[string]any) string {
	data, ok := result["data"].([]any)
	if !ok || len(data) == 0 {
		return "没有找到匹配的结果"
	}
	top, ok := data[0].(map[string]any)
	if !ok {
		return "没有找到匹配的结果"
	}

	sourceHosts := map[string]string{
		"nhentai": "nhentai.net",
		"ehentai": "e-hentai.org",
		"panda":   "panda.chaika.moe",
	}
	title := toString(top["title"])
	sim := toString(top["similarity"])
	source := toString(top["source"])
	subjectPath := toString(top["subjectPath"])
	pagePath := toString(top["pagePath"])
	url := ""
	if host := sourceHosts[source]; host != "" {
		path := pagePath
		if path == "" {
			path = subjectPath
		}
		url = "https://" + host + path
	}
	msg := "搜图结果：\n标题：" + title + "\n相似度：" + sim + "%"
	if url != "" {
		msg += "\n链接：" + url
	}
	return msg
}

func unique(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func helpMessage() string {
	return "使用说明：\n" +
		"1) /jm <ID>：下载并发送本子\n" +
		"2) /jm look <ID>：查看本子信息\n" +
		"3) /jm search <本子名>：搜索本子并下载（需确认）\n" +
		"4) /jm search | 识图 | /jm识图：开启2分钟识图等待窗口\n" +
		"5) /jm goodluck | /goodluck | 随机本子：随机本子下载\n" +
		"6) /jm mode pdf|zip：设置发送格式\n" +
		"7) /jm enc on|off：设置是否加密\n" +
		"8) /jm passwd <密码>：设置加密密码\n" +
		"9) /jm randpwd on|off：启用随机密码加密\n" +
		"10) /jm fname jm|full：设置发送文件命名方式\n" +
		"11) /jm regex on|off：设置正则模式\n" +
		"12) /jm help：查看帮助"
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func removeStr(list []string, v string) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if item != v {
			out = append(out, item)
		}
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func matched(pattern, s string) bool {
	return regexp.MustCompile(pattern).MatchString(s)
}

func mustMatch(pattern, s string) []string {
	return regexp.MustCompile(pattern).FindStringSubmatch(s)
}

func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func toInt64(v any) int64 {
	s := toString(v)
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func mapGet(v any, key string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	if key == "" {
		return m
	}
	mv, ok := m[key].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return mv
}

type NapcatClient struct {
	url    string
	token  string
	dryRun bool
}

func NewNapcatClient(url, token string, dryRun bool) *NapcatClient {
	return &NapcatClient{url: url, token: token, dryRun: dryRun}
}

func (c *NapcatClient) SendPrivateMessage(userID int64, message string) bool {
	_, err := c.send("send_private_msg", map[string]any{"user_id": userID, "message": []map[string]any{{"type": "text", "data": map[string]any{"text": message}}}}, echo("private_text", userID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendGroupMessage(groupID int64, message string) bool {
	_, err := c.send("send_group_msg", map[string]any{"group_id": groupID, "message": []map[string]any{{"type": "text", "data": map[string]any{"text": message}}}}, echo("group_text", groupID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendGroupJSONCardMessage(groupID int64, message string, nickname string) bool {
	card := buildJSONCardPayload(nickname, message)
	_, err := c.send("send_group_msg", map[string]any{
		"group_id": groupID,
		"message": []map[string]any{
			{"type": "json", "data": map[string]any{"data": card}},
		},
	}, echo("group_json_card", groupID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendPrivateJSONCardMessage(userID int64, message string, nickname string) bool {
	card := buildJSONCardPayload(nickname, message)
	_, err := c.send("send_private_msg", map[string]any{
		"user_id": userID,
		"message": []map[string]any{
			{"type": "json", "data": map[string]any{"data": card}},
		},
	}, echo("private_json_card", userID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendGroupXMLCardMessage(groupID int64, message string, nickname string) bool {
	card := buildXMLCardPayload(nickname, message)
	_, err := c.send("send_group_msg", map[string]any{
		"group_id": groupID,
		"message": []map[string]any{
			{"type": "xml", "data": map[string]any{"data": card}},
		},
	}, echo("group_xml_card", groupID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendPrivateXMLCardMessage(userID int64, message string, nickname string) bool {
	card := buildXMLCardPayload(nickname, message)
	_, err := c.send("send_private_msg", map[string]any{
		"user_id": userID,
		"message": []map[string]any{
			{"type": "xml", "data": map[string]any{"data": card}},
		},
	}, echo("private_xml_card", userID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendGroupMarkdownCardMessage(groupID int64, message string, nickname string) bool {
	md := buildMarkdownCard(nickname, message)
	_, err := c.send("send_group_msg", map[string]any{
		"group_id": groupID,
		"message": []map[string]any{
			{"type": "markdown", "data": map[string]any{"content": md}},
		},
	}, echo("group_md_card", groupID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendPrivateMarkdownCardMessage(userID int64, message string, nickname string) bool {
	md := buildMarkdownCard(nickname, message)
	_, err := c.send("send_private_msg", map[string]any{
		"user_id": userID,
		"message": []map[string]any{
			{"type": "markdown", "data": map[string]any{"content": md}},
		},
	}, echo("private_md_card", userID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendGroupForwardCardMessage(groupID int64, message string, senderID int64, nickname string) bool {
	node := map[string]any{
		"type": "node",
		"data": map[string]any{
			"user_id":  senderID,
			"nickname": nickname,
			"content": []map[string]any{
				{"type": "text", "data": map[string]any{"text": message}},
			},
		},
	}
	_, err := c.send("send_group_forward_msg", map[string]any{
		"group_id": groupID,
		"message":  []map[string]any{node},
	}, echo("group_forward_card", groupID), 10*time.Second)
	return err == nil
}

func (c *NapcatClient) SendPrivateForwardCardMessage(userID int64, message string, senderID int64, nickname string) bool {
	node := map[string]any{
		"type": "node",
		"data": map[string]any{
			"user_id":  senderID,
			"nickname": nickname,
			"content": []map[string]any{
				{"type": "text", "data": map[string]any{"text": message}},
			},
		},
	}
	_, err := c.send("send_private_forward_msg", map[string]any{
		"user_id": userID,
		"message": []map[string]any{node},
	}, echo("private_forward_card", userID), 10*time.Second)
	return err == nil
}

func buildJSONCardPayload(nickname, message string) string {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = "文件助手"
	}
	text := strings.TrimSpace(message)
	if text == "" {
		text = " "
	}
	if len([]rune(text)) > 1200 {
		text = string([]rune(text)[:1200]) + "..."
	}

	title := nickname
	summary := firstLine(text)
	if summary == "" {
		summary = nickname
	}

	payload := map[string]any{
		"app":    "com.tencent.card.notify",
		"desc":   "消息",
		"view":   "notify",
		"ver":    "1.0.0.0",
		"prompt": summary,
		"meta": map[string]any{
			"notify": map[string]any{
				"title":   title,
				"content": text,
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		fallback := map[string]any{
			"app":    "com.tencent.card.notify",
			"desc":   "消息",
			"view":   "notify",
			"ver":    "1.0.0.0",
			"prompt": nickname,
			"meta": map[string]any{
				"notify": map[string]any{
					"title":   nickname,
					"content": "消息发送失败",
				},
			},
		}
		b, _ = json.Marshal(fallback)
	}
	return string(b)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, sep := range []string{"\r\n", "\n", "\r"} {
		if idx := strings.Index(s, sep); idx >= 0 {
			s = s[:idx]
			break
		}
	}
	if len([]rune(s)) > 60 {
		return string([]rune(s)[:60]) + "..."
	}
	return s
}

func buildXMLCardPayload(nickname, message string) string {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = "文件助手"
	}
	text := strings.TrimSpace(message)
	if text == "" {
		text = " "
	}
	if len([]rune(text)) > 1200 {
		text = string([]rune(text)[:1200]) + "..."
	}
	title := firstLine(text)
	if title == "" {
		title = nickname
	}

	esc := func(s string) string {
		r := strings.NewReplacer(
			"&", "&amp;",
			"<", "&lt;",
			">", "&gt;",
			`"`, "&quot;",
			"'", "&apos;",
		)
		return r.Replace(s)
	}

	return fmt.Sprintf(
		`<?xml version='1.0' encoding='UTF-8' standalone='yes'?><msg serviceID="1" templateID="1" action="" brief="%s"><item layout="2"><title>%s</title><summary>%s</summary></item><source name="%s"/></msg>`,
		esc(title),
		esc(title),
		esc(text),
		esc(nickname),
	)
}

func buildMarkdownCard(nickname, message string) string {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		nickname = "文件助手"
	}
	text := strings.TrimSpace(message)
	if text == "" {
		text = " "
	}
	if len([]rune(text)) > 1200 {
		text = string([]rune(text)[:1200]) + "..."
	}
	return fmt.Sprintf("### %s\n\n%s", nickname, text)
}

func (c *NapcatClient) SendPrivateFile(cfg Config, userID int64, filePath string) bool {
	return c.sendFile(cfg, "send_private_msg", map[string]any{"user_id": userID}, filePath)
}

func (c *NapcatClient) SendGroupFile(cfg Config, groupID int64, filePath string) bool {
	return c.sendFile(cfg, "send_group_msg", map[string]any{"group_id": groupID}, filePath)
}

func (c *NapcatClient) sendFile(cfg Config, action string, baseParams map[string]any, filePath string) bool {
	if c.dryRun {
		log.Printf("[local-test] file message action=%s path=%s", action, filePath)
		return true
	}
	remotePath, err := stageForNapcat(cfg, filePath)
	if err != nil {
		log.Printf("stage file failed: %v", err)
		return false
	}
	defer cleanupRemote(cfg, remotePath)
	fileURL := "file://" + filepath.Join(cfg.DockerPath, filepath.Base(remotePath))
	params := copyMap(baseParams)
	params["message"] = []map[string]any{{"type": "file", "data": map[string]any{"file": fileURL}}}
	_, err = c.send(action, params, echo("file", time.Now().UnixNano()), 120*time.Second)
	if err != nil {
		log.Printf("send file failed: %v", err)
		return false
	}
	return true
}

func (c *NapcatClient) send(action string, params map[string]any, echoValue string, timeout time.Duration) (map[string]any, error) {
	if c.dryRun {
		log.Printf("[local-test] action=%s echo=%s", action, echoValue)
		return map[string]any{"status": "ok", "echo": echoValue}, nil
	}
	h := http.Header{}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	conn, _, err := websocket.DefaultDialer.Dial(c.url, h)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	payload := map[string]any{"action": action, "params": params, "echo": echoValue}
	b, _ := json.Marshal(payload)
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		var resp map[string]any
		if err := json.Unmarshal(msg, &resp); err != nil {
			continue
		}
		if toString(resp["echo"]) != echoValue {
			continue
		}
		if toString(resp["status"]) != "ok" {
			return resp, fmt.Errorf("napcat ret failed: %v", resp)
		}
		return resp, nil
	}
}

func stageForNapcat(cfg Config, localFile string) (string, error) {
	remote := filepath.Join(cfg.RemoteTempDir, filepath.Base(localFile))
	if strings.ToLower(cfg.TransferMode) == "local" {
		raw, err := os.ReadFile(localFile)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(remote, raw, 0o644); err != nil {
			return "", err
		}
		return remote, nil
	}
	cmd := exec.Command("scp", "-i", cfg.LocalSSHKey, "-o", "StrictHostKeyChecking=no", localFile, fmt.Sprintf("%s@%s:%s", cfg.RemoteUser, cfg.RemoteHost, remote))
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("scp failed: %v: %s", err, string(out))
	}
	return remote, nil
}

func cleanupRemote(cfg Config, remotePath string) {
	if remotePath == "" {
		return
	}
	if strings.ToLower(cfg.TransferMode) == "local" {
		_ = os.Remove(remotePath)
		return
	}
	cmd := exec.Command("ssh", "-i", cfg.LocalSSHKey, "-o", "StrictHostKeyChecking=no", fmt.Sprintf("%s@%s", cfg.RemoteUser, cfg.RemoteHost), fmt.Sprintf("rm -f '%s'", remotePath))
	_, _ = cmd.CombinedOutput()
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func echo(prefix any, id any) string {
	return fmt.Sprintf("%v_%v_%d", prefix, id, time.Now().UnixNano())
}

// JMBridge implementation moved to jm_pure.go
