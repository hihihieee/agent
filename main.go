package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "2.0.0"

type Config struct {
	Endpoint, Secret, Node, LogPath, StateDir, NetworkInterface string
	Window, Interval, StatsInterval                             time.Duration
}
type Client struct {
	Email string   `json:"email"`
	IPs   []string `json:"ips"`
}
type Ingest struct {
	Node    string   `json:"node"`
	TS      int64    `json:"ts"`
	Clients []Client `json:"clients"`
}
type Net struct {
	RX uint64 `json:"rx"`
	TX uint64 `json:"tx"`
}
type Stats struct {
	Node   string             `json:"node"`
	TS     int64              `json:"ts"`
	CPU    map[string]float64 `json:"cpu"`
	Load   []float64          `json:"load"`
	Cores  int                `json:"cores"`
	Uptime uint64             `json:"uptime"`
	Mem    map[string]uint64  `json:"mem"`
	Swap   map[string]uint64  `json:"swap"`
	Net    Net                `json:"net"`
}
type State struct {
	Path   string                      `json:"path"`
	Inode  uint64                      `json:"inode"`
	Offset int64                       `json:"offset"`
	Seen   map[string]map[string]int64 `json:"seen,omitempty"`
}

var fromRE = regexp.MustCompile(`\bfrom\s+(?:\[([^]]+)\]|([^\s:]+)):\d+`)
var emailRE = regexp.MustCompile(`\bemail:\s*([^\s]+)`)
var clientEmailRE = regexp.MustCompile(`^[0-9]+(?:_[0-9]+)?$`)

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", key)
	}
	return time.Duration(n) * time.Second, nil
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	allowed := map[string]bool{
		"ENDPOINT": true, "SECRET": true, "NODE": true, "LOG_PATH": true,
		"NETWORK_INTERFACE": true, "STATE_DIR": true, "WINDOW": true,
		"INTERVAL": true, "STATS_INTERVAL": true,
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !allowed[key] || os.Getenv(key) != "" {
			continue
		}
		if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return fmt.Errorf("parse %s: %w", key, err)
			}
			value = decoded
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func loadConfig() (Config, error) {
	w, err := envDuration("WINDOW", 90*time.Second)
	if err != nil {
		return Config{}, err
	}
	i, err := envDuration("INTERVAL", 20*time.Second)
	if err != nil {
		return Config{}, err
	}
	s, err := envDuration("STATS_INTERVAL", 15*time.Second)
	if err != nil {
		return Config{}, err
	}
	c := Config{Endpoint: strings.TrimRight(os.Getenv("ENDPOINT"), "/"), Secret: os.Getenv("SECRET"), Node: os.Getenv("NODE"), LogPath: os.Getenv("LOG_PATH"), StateDir: os.Getenv("STATE_DIR"), NetworkInterface: os.Getenv("NETWORK_INTERFACE"), Window: w, Interval: i, StatsInterval: s}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/palm-agent"
	}
	if c.Endpoint == "" || c.Secret == "" || c.Node == "" || c.LogPath == "" {
		return c, errors.New("ENDPOINT, SECRET, NODE and LOG_PATH are required")
	}
	if c.Interval < time.Second || c.Window < c.Interval {
		return c, errors.New("INTERVAL must be >=1 and WINDOW must be >= INTERVAL")
	}
	return c, nil
}

func parseAccess(line string) (string, string, bool) {
	f, e := fromRE.FindStringSubmatch(line), emailRE.FindStringSubmatch(line)
	if len(f) == 0 || len(e) == 0 {
		return "", "", false
	}
	ip := f[1]
	if ip == "" {
		ip = f[2]
	}
	if net.ParseIP(ip) == nil || !clientEmailRE.MatchString(e[1]) {
		return "", "", false
	}
	return e[1], ip, true
}

type Tracker struct {
	mu     sync.Mutex
	seen   map[string]map[string]time.Time
	window time.Duration
}

func (t *Tracker) Add(email, ip string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen[email] == nil && len(t.seen) >= 100000 {
		return
	}
	if t.seen[email] == nil {
		t.seen[email] = map[string]time.Time{}
	}
	if _, exists := t.seen[email][ip]; !exists && len(t.seen[email]) >= 64 {
		return
	}
	t.seen[email][ip] = now
}
func (t *Tracker) Snapshot(now time.Time) []Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []Client{}
	for email, ips := range t.seen {
		live := []string{}
		for ip, ts := range ips {
			if now.Sub(ts) <= t.window {
				live = append(live, ip)
			} else {
				delete(ips, ip)
			}
		}
		if len(live) > 0 {
			sort.Strings(live)
			out = append(out, Client{email, live})
		} else {
			delete(t.seen, email)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

func (t *Tracker) Export(now time.Time) map[string]map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[string]map[string]int64{}
	for email, ips := range t.seen {
		for ip, ts := range ips {
			if now.Sub(ts) <= t.window {
				if out[email] == nil {
					out[email] = map[string]int64{}
				}
				out[email][ip] = ts.Unix()
			}
		}
	}
	return out
}

func (t *Tracker) Restore(saved map[string]map[string]int64, now time.Time) {
	for email, ips := range saved {
		for ip, unix := range ips {
			ts := time.Unix(unix, 0)
			if now.Sub(ts) <= t.window && now.Sub(ts) >= 0 {
				t.Add(email, ip, ts)
			}
		}
	}
}

func saveState(path string, state State, tracker *Tracker) {
	state.Seen = tracker.Export(time.Now())
	b, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0600) == nil {
		_ = os.Rename(tmp, path)
	}
}
func follow(ctx context.Context, cfg Config, tracker *Tracker) error {
	statePath := filepath.Join(cfg.StateDir, "tail-state.json")
	var state State
	if b, e := os.ReadFile(statePath); e == nil {
		_ = json.Unmarshal(b, &state)
	}
	hadState := state.Path != ""
	tracker.Restore(state.Seen, time.Now())
	lastSaved := time.Time{}
	for ctx.Err() == nil {
		f, err := os.Open(cfg.LogPath)
		if err != nil {
			log.Printf("access log unavailable: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
				continue
			}
		}
		info, _ := f.Stat()
		start := info.Size()
		currentInode := inode(info)
		if state.Path == cfg.LogPath && state.Inode == currentInode && state.Offset <= info.Size() {
			start = state.Offset
		} else if hadState {
			start = 0
		}
		_, _ = f.Seek(start, io.SeekStart)
		r := bufio.NewReader(f)
		pending := ""
		for ctx.Err() == nil {
			line, readErr := r.ReadString('\n')
			if pending != "" {
				line = pending + line
				pending = ""
			}
			if len(line) > 0 {
				if readErr != nil && !strings.HasSuffix(line, "\n") {
					pending = line
				} else {
					if em, ip, ok := parseAccess(line); ok {
						tracker.Add(em, ip, time.Now())
					}
					pos, _ := f.Seek(0, io.SeekCurrent)
					state = State{
						Path:   cfg.LogPath,
						Inode:  currentInode,
						Offset: pos - int64(r.Buffered()),
					}
					if time.Since(lastSaved) >= 2*time.Second {
						saveState(statePath, state, tracker)
						lastSaved = time.Now()
					}
				}
			}
			if readErr == nil {
				continue
			}
			time.Sleep(time.Second)
			newInfo, e := os.Stat(cfg.LogPath)
			if e == nil && (inode(newInfo) != currentInode || newInfo.Size() < state.Offset) {
				break
			}
		}
		_ = f.Close()
	}
	saveState(statePath, state, tracker)
	return nil
}

func signedPost(ctx context.Context, client *http.Client, url, secret string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("User-Agent", "palm-agent/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}
func sender(ctx context.Context, cfg Config, tracker *Tracker) {
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	client := &http.Client{Timeout: 10 * time.Second}
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			clients := tracker.Snapshot(now)
			if len(clients) == 0 {
				continue
			}
			err := signedPost(ctx, client, cfg.Endpoint, cfg.Secret, Ingest{cfg.Node, now.Unix(), clients})
			if err != nil {
				failures++
				log.Printf("ingest failed (%d): %v", failures, err)
			} else {
				if failures > 0 {
					log.Printf("ingest recovered")
				}
				failures = 0
			}
		}
	}
}

func readCPU() ([]uint64, error) {
	f, e := os.Open("/proc/stat")
	if e != nil {
		return nil, e
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	if !s.Scan() {
		return nil, s.Err()
	}
	p := strings.Fields(s.Text())
	v := make([]uint64, 8)
	for i := range v {
		if i+1 < len(p) {
			v[i], _ = strconv.ParseUint(p[i+1], 10, 64)
		}
	}
	return v, nil
}
func defaultInterface() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		p := strings.Fields(s.Text())
		if len(p) >= 4 && p[1] == "00000000" {
			return p[0]
		}
	}
	return ""
}
func readNet(networkInterface string) (uint64, uint64) {
	f, e := os.Open("/proc/net/dev")
	if e != nil {
		return 0, 0
	}
	defer f.Close()
	var rx, tx uint64
	s := bufio.NewScanner(f)
	for s.Scan() {
		p := strings.Fields(strings.ReplaceAll(s.Text(), ":", " "))
		if len(p) < 10 || p[0] == "lo" || (networkInterface != "" && p[0] != networkInterface) {
			continue
		}
		a, _ := strconv.ParseUint(p[1], 10, 64)
		b, _ := strconv.ParseUint(p[9], 10, 64)
		rx += a
		tx += b
	}
	return rx, tx
}
func memInfo() map[string]uint64 {
	out := map[string]uint64{}
	b, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(b), "\n") {
		p := strings.Fields(line)
		if len(p) >= 2 {
			n, _ := strconv.ParseUint(p[1], 10, 64)
			out[strings.TrimSuffix(p[0], ":")] = n * 1024
		}
	}
	return out
}
func collectStats(node, networkInterface string, prevCPU []uint64, prevRX, prevTX uint64, dt float64) (Stats, []uint64, uint64, uint64) {
	nowCPU, _ := readCPU()
	d := make([]uint64, 8)
	var total uint64
	for i := range d {
		if i < len(prevCPU) && nowCPU[i] >= prevCPU[i] {
			d[i] = nowCPU[i] - prevCPU[i]
			total += d[i]
		}
	}
	pct := func(n uint64) float64 {
		if total == 0 {
			return 0
		}
		return float64(n) * 100 / float64(total)
	}
	loads := []float64{0, 0, 0}
	if b, e := os.ReadFile("/proc/loadavg"); e == nil {
		p := strings.Fields(string(b))
		for i := 0; i < 3; i++ {
			loads[i], _ = strconv.ParseFloat(p[i], 64)
		}
	}
	cores := 0
	if b, e := os.ReadFile("/proc/cpuinfo"); e == nil {
		cores = strings.Count(string(b), "processor\t:")
	}
	up := uint64(0)
	if b, e := os.ReadFile("/proc/uptime"); e == nil {
		v, _ := strconv.ParseFloat(strings.Fields(string(b))[0], 64)
		up = uint64(v)
	}
	m := memInfo()
	rx, tx := readNet(networkInterface)
	rate := func(n, p uint64) uint64 {
		if n < p || dt <= 0 {
			return 0
		}
		return uint64(float64(n-p) / dt)
	}
	st := Stats{Node: node, TS: time.Now().Unix(), CPU: map[string]float64{"us": pct(d[0] + d[1]), "sy": pct(d[2] + d[5] + d[6]), "id": pct(d[3]), "wa": pct(d[4]), "st": pct(d[7])}, Load: loads, Cores: cores, Uptime: up, Mem: map[string]uint64{"total": m["MemTotal"], "avail": m["MemAvailable"]}, Swap: map[string]uint64{"total": m["SwapTotal"], "free": m["SwapFree"]}, Net: Net{rate(rx, prevRX), rate(tx, prevTX)}}
	return st, nowCPU, rx, tx
}
func statsLoop(ctx context.Context, cfg Config) {
	if cfg.StatsInterval <= 0 {
		return
	}
	cpu, _ := readCPU()
	iface := cfg.NetworkInterface
	if iface == "" {
		iface = defaultInterface()
	}
	rx, tx := readNet(iface)
	last := time.Now()
	timer := time.NewTicker(cfg.StatsInterval)
	defer timer.Stop()
	url := strings.TrimSuffix(cfg.Endpoint, "/limiter/ingest") + "/limiter/stats"
	client := &http.Client{Timeout: 10 * time.Second}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			st, nc, nr, nt := collectStats(cfg.Node, iface, cpu, rx, tx, now.Sub(last).Seconds())
			cpu, rx, tx, last = nc, nr, nt, now
			if e := signedPost(ctx, client, url, cfg.Secret, st); e != nil {
				log.Printf("stats failed: %v", e)
			}
		}
	}
}

func doctor(cfg Config) int {
	ok := true
	fmt.Printf("Palm Agent %s\n", version)
	for _, check := range []struct {
		name   string
		pass   bool
		detail string
	}{{"configuration", cfg.Secret != "", cfg.Node}, {"access log", fileExists(cfg.LogPath), cfg.LogPath}, {"state directory", fileExists(cfg.StateDir), cfg.StateDir}} {
		mark := "OK"
		if !check.pass {
			mark = "FAIL"
			ok = false
		}
		fmt.Printf("%-5s %-18s %s\n", mark, check.name, check.detail)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := signedPost(ctx, &http.Client{Timeout: 10 * time.Second}, cfg.Endpoint, cfg.Secret, Ingest{Node: cfg.Node, TS: time.Now().Unix(), Clients: []Client{}})
	if err != nil {
		fmt.Printf("FAIL  endpoint           %v\n", err)
		ok = false
	} else {
		fmt.Println("OK    endpoint           authenticated")
	}
	if ok {
		return 0
	}
	return 1
}
func fileExists(p string) bool { _, e := os.Stat(p); return e == nil }
func main() {
	flag.Parse()
	cmd := "run"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}
	if cmd == "version" {
		fmt.Println(version)
		return
	}
	if os.Getenv("ENDPOINT") == "" {
		if err := loadEnvFile("/etc/palm-agent.env"); err != nil && !os.IsNotExist(err) {
			log.Printf("cannot read /etc/palm-agent.env: %v", err)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if cmd == "doctor" {
		os.Exit(doctor(cfg))
	}
	if cmd != "run" {
		log.Fatalf("unknown command %q", cmd)
	}
	if e := os.MkdirAll(cfg.StateDir, 0700); e != nil {
		log.Fatal(e)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	tracker := &Tracker{seen: map[string]map[string]time.Time{}, window: cfg.Window}
	go sender(ctx, cfg, tracker)
	go statsLoop(ctx, cfg)
	if err := follow(ctx, cfg, tracker); err != nil {
		log.Fatal(err)
	}
}
