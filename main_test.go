package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testRegistry(t *testing.T) *registry {
	t.Helper()
	oldBinary, oldDataDir := binary, dataDir
	oldScheme, oldDomain, oldPort := baseScheme, baseDomain, basePort
	t.Cleanup(func() {
		binary, dataDir = oldBinary, oldDataDir
		baseScheme, baseDomain, basePort = oldScheme, oldDomain, oldPort
	})
	t.Chdir(t.TempDir())
	dataDir = "./demos"
	baseScheme, baseDomain, basePort = "http", "demo.example.test", "8080"
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	reg := &registry{instances: make(map[string]*instance)}
	t.Cleanup(func() {
		for _, inst := range reg.instances {
			_ = inst.cmd.Process.Kill()
			<-inst.done
		}
	})
	return reg
}

func TestInstanceEnvIsolatesAutenticoConfig(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin", "HOME=/tmp/home", "OTHER=value",
		"AUTENTICO_TOTP_ENCRYPTION_KEY=invalid",
		"AUTENTICO_DB_FILE_PATH=/production.db",
		"AUTENTICO_LISTEN_PORT=9999", "AUTENTICO_LISTEN_PORT=443",
		"AUTENTICO_PRIVATE_KEY=shared", "AUTENTICO_APP_OAUTH_PATH=/custom",
	}
	env := instanceEnv(parent, 12345)
	for _, want := range []string{"PATH=/usr/bin", "HOME=/tmp/home", "OTHER=value", "AUTENTICO_LISTEN_PORT=12345", "AUTENTICO_TRUSTED_PROXIES=127.0.0.1/32,::1/128"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %q", want)
		}
	}
	if len(env) != 5 {
		t.Fatalf("inherited Autentico configuration: %v", env)
	}
}

func TestLaunchRejectsMissingBinary(t *testing.T) {
	reg := testRegistry(t)
	binary = "./missing-autentico"
	if _, err := reg.launch("127.0.0.1"); err == nil {
		t.Fatal("expected missing binary error")
	}
	assertNoInstances(t, reg)
}

func TestLaunchReportsEarlyExitAndRemovesData(t *testing.T) {
	reg := testRegistry(t)
	binary = "./autentico"
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err := reg.launch("127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "exit status 42") {
		t.Fatalf("expected process exit error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("early exit was not reported promptly: %s", elapsed)
	}
	assertNoInstances(t, reg)
}

func assertNoInstances(t *testing.T, reg *registry) {
	t.Helper()
	if len(reg.instances) != 0 {
		t.Fatal("failed startup registered an instance")
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed startup left data: %v", entries)
	}
}

func TestWaitForReady(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		ready  bool
	}{
		{"expected issuer", 200, `{"issuer":"https://demo.example.test/oauth2"}`, true},
		{"wrong issuer", 200, `{"issuer":"https://other.example.test/oauth2"}`, false},
		{"invalid JSON", 200, `<html>not discovery</html>`, false},
		{"unavailable", 503, `{"issuer":"https://demo.example.test/oauth2"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
			port, _ := strconv.Atoi(portStr)
			err := waitForReady(port, "https://demo.example.test", make(chan error), 100*time.Millisecond)
			if (err == nil) != tc.ready {
				t.Fatalf("ready=%v, error=%v", tc.ready, err)
			}
		})
	}
}

func TestWaitForReadyReportsProcessExit(t *testing.T) {
	done := make(chan error, 1)
	done <- errors.New("test startup failure")
	if err := waitForReady(1, "https://demo.example.test", done, time.Minute); err == nil || !strings.Contains(err.Error(), "test startup failure") {
		t.Fatalf("expected startup failure, got %v", err)
	}
}

func TestProxyRejectsUnknownHostAndInstance(t *testing.T) {
	reg := testRegistry(t)
	for _, tc := range []struct {
		host string
		want int
	}{
		{"other.example.test", http.StatusNotFound},
		{"expired.demo.example.test:8080", http.StatusGone},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/admin/", nil)
		w := httptest.NewRecorder()
		reg.handleProxy(w, req)
		if w.Code != tc.want {
			t.Errorf("host %s: status=%d, want %d", tc.host, w.Code, tc.want)
		}
	}
}

func TestLaunchStartReturnsFailure(t *testing.T) {
	reg := testRegistry(t)
	binary = filepath.Join(t.TempDir(), "missing")
	req := httptest.NewRequest(http.MethodGet, "http://demo.example.test/launch/start", nil)
	w := httptest.NewRecorder()
	reg.handleLaunchStart(w, req)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("unexpected failure response: %d %s", w.Code, w.Body.String())
	}
}
