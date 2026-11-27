package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*.html web/assets/*
var web embed.FS

const (
	sessionDuration           = 7 * 24 * time.Hour
	defaultAutoWeightInterval = 30 * time.Minute
)

type config struct {
	addr               string
	baseURL            string
	upstreamKey        string
	password           string
	sessionKey         string
	secureCookie       bool
	autoWeightEnabled  bool
	autoWeightInterval time.Duration
}

func loadConfig() (config, error) {
	cfg := config{
		addr:               os.Getenv("ADDR"),
		baseURL:            os.Getenv("GPT_LOAD_BASE_URL"),
		upstreamKey:        strings.TrimSpace(os.Getenv("GPT_LOAD_AUTH_KEY")),
		password:           os.Getenv("VIEWER_PASSWORD"),
		sessionKey:         os.Getenv("SESSION_SECRET"),
		autoWeightEnabled:  true,
		autoWeightInterval: defaultAutoWeightInterval,
	}
	if cfg.addr == "" {
		cfg.addr = "127.0.0.1:8080"
	}
	if cfg.baseURL == "" {
		return cfg, errors.New("set GPT_LOAD_BASE_URL")
	}
	base, err := url.Parse(cfg.baseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return cfg, errors.New("GPT_LOAD_BASE_URL must be an HTTP(S) base URL")
	}
	cfg.baseURL = strings.TrimRight(base.String(), "/")
	if cfg.upstreamKey == "" || cfg.password == "" || len(cfg.sessionKey) < 32 {
		return cfg, errors.New("set GPT_LOAD_AUTH_KEY, VIEWER_PASSWORD, and SESSION_SECRET (at least 32 characters)")
	}
	if value := os.Getenv("COOKIE_SECURE"); value != "" {
		cfg.secureCookie, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, errors.New("COOKIE_SECURE must be true or false")
		}
	}
	if value := os.Getenv("AUTO_WEIGHT_ENABLED"); value != "" {
		cfg.autoWeightEnabled, err = strconv.ParseBool(value)
		if err != nil {
			return cfg, errors.New("AUTO_WEIGHT_ENABLED must be true or false")
		}
	}
	if value := os.Getenv("AUTO_WEIGHT_INTERVAL"); value != "" {
		cfg.autoWeightInterval, err = time.ParseDuration(value)
		if err != nil || cfg.autoWeightInterval <= 0 {
			return cfg, errors.New("AUTO_WEIGHT_INTERVAL must be a positive duration")
		}
	}
	return cfg, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	application := newApp(cfg, time.Now)
	if cfg.autoWeightEnabled {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		go application.adjuster.Start(ctx)
	}
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           application.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       time.Minute,
	}
	log.Printf("GPT-Load dashboard listening on %s", cfg.addr)
	log.Fatal(server.ListenAndServe())
}

type app struct {
	upstream     upstream
	adjuster     *weightAdjuster
	now          func() time.Time
	loginPage    *template.Template
	indexPage    []byte
	password     [sha256.Size]byte
	sessionKey   []byte
	secureCookie bool
	loginLimit   loginLimit
}

func newHandler(cfg config, now func() time.Time) http.Handler {
	return newApp(cfg, now).handler()
}

func newApp(cfg config, now func() time.Time) *app {
	var indexPage bytes.Buffer
	indexTemplate := template.Must(template.ParseFS(web, "web/index.html"))
	if err := indexTemplate.ExecuteTemplate(&indexPage, "index.html", struct{ MainSiteURL string }{cfg.baseURL}); err != nil {
		panic(err)
	}
	up := upstream{
		baseURL: cfg.baseURL,
		key:     cfg.upstreamKey,
		slots:   make(chan struct{}, 8),
		client: &http.Client{
			Timeout: 8 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	return &app{
		now:          now,
		upstream:     up,
		adjuster:     newWeightAdjuster(up, cfg.autoWeightEnabled, cfg.autoWeightInterval, now),
		loginPage:    template.Must(template.ParseFS(web, "web/login.html")),
		indexPage:    indexPage.Bytes(),
		password:     sha256.Sum256([]byte(cfg.password)),
		sessionKey:   []byte(cfg.sessionKey),
		secureCookie: cfg.secureCookie,
	}
}

func (a *app) handler() http.Handler {
	assets, err := fs.Sub(web, "web/assets")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("POST /login", a.login)
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("GET /api/dashboard", a.dashboard)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(assets)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		headers.Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		headers.Set("X-Content-Type-Options", "nosniff")
		headers.Set("Referrer-Policy", "no-referrer")
		headers.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (a *app) index(w http.ResponseWriter, r *http.Request) {
	if !a.authenticated(r) {
		a.renderLogin(w, http.StatusOK, "")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(a.indexPage)
}

func (a *app) renderLogin(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.loginPage.ExecuteTemplate(w, "login.html", struct{ Error string }{message}); err != nil {
		log.Printf("render login: %v", err)
	}
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !a.loginLimit.allow(peer, time.Now()) {
		w.Header().Set("Retry-After", "60")
		a.renderLogin(w, http.StatusTooManyRequests, "登录尝试过多，请一分钟后再试。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		a.renderLogin(w, http.StatusBadRequest, "无法读取登录表单，请重试。")
		return
	}
	provided := sha256.Sum256([]byte(r.PostForm.Get("password")))
	if !hmac.Equal(provided[:], a.password[:]) {
		a.renderLogin(w, http.StatusUnauthorized, "查看密码不正确。")
		return
	}
	expires := time.Now().Add(sessionDuration)
	value := a.makeSession(expires)
	http.SetCookie(w, a.cookie(value, int(sessionDuration.Seconds())))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, a.cookie("", -1))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) cookie(value string, maxAge int) *http.Cookie {
	name := "gpt_load_viewer"
	if a.secureCookie {
		name = "__Host-" + name
	}
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: a.secureCookie, SameSite: http.SameSiteStrictMode,
	}
}

func (a *app) sessionMAC(payload []byte) []byte {
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write(a.password[:])
	mac.Write(payload)
	return mac.Sum(nil)
}

func (a *app) makeSession(expires time.Time) string {
	var expiry [8]byte
	binary.BigEndian.PutUint64(expiry[:], uint64(expires.Unix()))
	return base64.RawURLEncoding.EncodeToString(append(expiry[:], a.sessionMAC(expiry[:])...))
}

func (a *app) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(a.cookie("", 0).Name)
	if err != nil || len(cookie.Value) != base64.RawURLEncoding.EncodedLen(8+sha256.Size) {
		return false
	}
	token, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil || len(token) != 8+sha256.Size || !hmac.Equal(token[8:], a.sessionMAC(token[:8])) {
		return false
	}
	return time.Now().Unix() < int64(binary.BigEndian.Uint64(token[:8]))
}

type loginLimit struct {
	mu            sync.Mutex
	windowExpires time.Time
	attempts      map[string]int
}

func (l *loginLimit) allow(peer string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.windowExpires) {
		l.attempts = make(map[string]int)
		l.windowExpires = now.Add(time.Minute)
	}
	if l.attempts[peer] >= 20 {
		return false
	}
	l.attempts[peer]++
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("send dashboard JSON: %v", err)
	}
}
