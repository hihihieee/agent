package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestParseAccess(t *testing.T) {
	cases := []struct{ line, email, ip string }{
		{"2026 from 1.2.3.4:555 accepted tcp:x:443 email: 42_2", "42_2", "1.2.3.4"},
		{"from [2001:db8::1]:443 accepted udp:x:53 email: 99_7", "99_7", "2001:db8::1"},
	}
	for _, c := range cases {
		e, ip, ok := parseAccess(c.line)
		if !ok || e != c.email || ip != c.ip {
			t.Fatalf("got %q %q %v", e, ip, ok)
		}
	}
}

func TestParseAccessRejectsInvalidInput(t *testing.T) {
	cases := []string{
		"from not-an-ip:443 accepted tcp:x:443 email: 42_2",
		"from 1.2.3.4:443 accepted tcp:x:443 email: customer@example.com",
		"from 1.2.3.4:443 accepted tcp:x:443",
	}
	for _, line := range cases {
		if _, _, ok := parseAccess(line); ok {
			t.Fatalf("accepted invalid line %q", line)
		}
	}
}

func TestTrackerExpiry(t *testing.T) {
	now := time.Now()
	tr := Tracker{seen: map[string]map[string]time.Time{}, window: 90 * time.Second}
	tr.Add("1_2", "1.1.1.1", now.Add(-100*time.Second))
	tr.Add("1_2", "2.2.2.2", now)
	got := tr.Snapshot(now)
	if len(got) != 1 || len(got[0].IPs) != 1 || got[0].IPs[0] != "2.2.2.2" {
		t.Fatalf("unexpected %#v", got)
	}
}

func TestTrackerSnapshotIsSortedAndBounded(t *testing.T) {
	now := time.Now()
	tr := Tracker{seen: map[string]map[string]time.Time{}, window: 90 * time.Second}
	tr.Add("2_1", "2.2.2.2", now)
	tr.Add("1_1", "3.3.3.3", now)
	tr.Add("1_1", "1.1.1.1", now)
	for i := 0; i < 70; i++ {
		tr.Add("3_1", fmt.Sprintf("2001:db8::%x", i), now)
	}
	got := tr.Snapshot(now)
	if len(got) != 3 || got[0].Email != "1_1" || !reflect.DeepEqual(got[0].IPs, []string{"1.1.1.1", "3.3.3.3"}) {
		t.Fatalf("snapshot is not deterministic: %#v", got)
	}
	if len(got[2].IPs) != 64 {
		t.Fatalf("per-client bound = %d, want 64", len(got[2].IPs))
	}
}

func TestTrackerExportRestore(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	source := Tracker{seen: map[string]map[string]time.Time{}, window: 90 * time.Second}
	source.Add("1_2", "1.1.1.1", now.Add(-10*time.Second))
	source.Add("expired", "2.2.2.2", now.Add(-100*time.Second))
	source.Add("future", "3.3.3.3", now.Add(time.Second))

	restored := Tracker{seen: map[string]map[string]time.Time{}, window: 90 * time.Second}
	restored.Restore(source.Export(now), now)
	got := restored.Snapshot(now)
	if len(got) != 1 || got[0].Email != "1_2" || got[0].IPs[0] != "1.1.1.1" {
		t.Fatalf("unexpected restored state: %#v", got)
	}
}

func TestSignedPost(t *testing.T) {
	const secret = "test-secret"
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		if got, want := r.Header.Get("X-Signature"), hex.EncodeToString(mac.Sum(nil)); got != want {
			t.Errorf("signature = %q, want %q", got, want)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected content type %q", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := signedPost(context.Background(), server.Client(), server.URL, secret, Ingest{Node: "test", Clients: []Client{}})
	if err != nil || !called {
		t.Fatalf("signedPost error = %v, called = %v", err, called)
	}
}

func TestSignedPostReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer server.Close()
	if err := signedPost(context.Background(), server.Client(), server.URL, "secret", struct{}{}); err == nil {
		t.Fatal("expected an error for non-2xx response")
	}
}

func TestNetJSONFields(t *testing.T) {
	b, err := json.Marshal(Net{RX: 12, TX: 34})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"rx":12,"tx":34}` {
		t.Fatalf("unexpected network JSON: %s", b)
	}
}

func TestFollowWaitsForCompleteLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	if err := os.WriteFile(logPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	tracker := &Tracker{seen: map[string]map[string]time.Time{}, window: 90 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- follow(ctx, Config{LogPath: logPath, StateDir: dir}, tracker)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	time.Sleep(100 * time.Millisecond)

	appendLog := func(text string) {
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(text); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	appendLog("from 1.2.3.4:443 accepted tcp:x:443")
	time.Sleep(1100 * time.Millisecond)
	if got := tracker.Snapshot(time.Now()); len(got) != 0 {
		t.Fatalf("partial line was processed: %#v", got)
	}

	appendLog(" email: 42_2\n")
	time.Sleep(1100 * time.Millisecond)
	got := tracker.Snapshot(time.Now())
	if len(got) != 1 || got[0].Email != "42_2" || got[0].IPs[0] != "1.2.3.4" {
		t.Fatalf("completed line was not processed: %#v", got)
	}
}

func TestLoadEnvFileDoesNotOverrideEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.env")
	content := "ENDPOINT=\"https://example.test/limiter/ingest\"\nSECRET=\"file-secret\"\nUNKNOWN=\"ignored\"\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENDPOINT", "https://override.test/limiter/ingest")
	t.Setenv("SECRET", "")
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ENDPOINT"); got != "https://override.test/limiter/ingest" {
		t.Fatalf("ENDPOINT was overridden: %q", got)
	}
	if got := os.Getenv("SECRET"); got != "file-secret" {
		t.Fatalf("SECRET = %q", got)
	}
	if got := os.Getenv("UNKNOWN"); got != "" {
		t.Fatalf("unexpected unknown variable: %q", got)
	}
}
