package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run against released Autentico binaries with AUTENTICO_TEST_BINARY=/path/to/autentico.
// No existing database, configuration or running server is used.
func TestAutenticoCompatibility(t *testing.T) {
	testBinary := os.Getenv("AUTENTICO_TEST_BINARY")
	if testBinary == "" {
		t.Skip("set AUTENTICO_TEST_BINARY to run the real Autentico compatibility test")
	}
	testBinary, err := filepath.Abs(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistry(t)
	// A relative executable and data path must still work when the child chdirs.
	binary = "./autentico"
	if err := os.Symlink(testBinary, binary); err != nil {
		t.Fatal(err)
	}
	// Neither a parent .env nor the service's environment may leak into demos.
	if err := os.WriteFile(".env", []byte("AUTENTICO_TOTP_ENCRYPTION_KEY=invalid\nAUTENTICO_APP_OAUTH_PATH=/wrong\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTENTICO_TOTP_ENCRYPTION_KEY", "invalid")
	t.Setenv("AUTENTICO_PRIVATE_KEY", "invalid")
	t.Setenv("AUTENTICO_DB_FILE_PATH", filepath.Join(t.TempDir(), "must-not-exist.db"))
	t.Setenv("AUTENTICO_APP_URL", "https://wrong.example.test")
	t.Setenv("AUTENTICO_LISTEN_PORT", "1")
	t.Setenv("AUTENTICO_APP_OAUTH_PATH", "/wrong")
	t.Setenv("AUTENTICO_CSRF_SECURE_COOKIE", "true")
	t.Setenv("AUTENTICO_IDP_SESSION_SECURE", "true")
	server := httptest.NewServer(http.HandlerFunc(reg.handleProxy))
	defer server.Close()
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var previous map[string]string
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			baseScheme = scheme
			start := httptest.NewRecorder()
			reg.handleLaunchStart(start, httptest.NewRequest(http.MethodGet, "http://demo.example.test/launch/start", nil))
			if start.Code != http.StatusOK {
				t.Fatalf("launch failed: %d %s", start.Code, start.Body.String())
			}
			var result struct{ URL string }
			if err := json.Unmarshal(start.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			public, err := url.Parse(result.URL)
			if err != nil || !strings.HasSuffix(result.URL, "/onboard/") {
				t.Fatalf("invalid onboarding URL: %s (%v)", result.URL, err)
			}
			slug := strings.TrimSuffix(public.Hostname(), "."+baseDomain)
			inst := reg.instances[slug]
			if inst == nil {
				t.Fatal("instance not registered")
			}
			env := readInstanceConfig(t, filepath.Join(inst.dir, ".env"))
			for _, name := range []string{"AUTENTICO_ACCESS_TOKEN_SECRET", "AUTENTICO_REFRESH_TOKEN_SECRET", "AUTENTICO_CSRF_SECRET_KEY", "AUTENTICO_PRIVATE_KEY", "AUTENTICO_TOTP_ENCRYPTION_KEY"} {
				if env[name] == "" || (previous != nil && env[name] == previous[name]) {
					t.Fatalf("missing or shared instance secret: %s", name)
				}
			}
			key, err := base64.StdEncoding.DecodeString(env["AUTENTICO_TOTP_ENCRYPTION_KEY"])
			if err != nil || len(key) != 32 {
				t.Fatal("TOTP encryption key is not a base64-encoded AES-256 key")
			}
			secure := strconv.FormatBool(scheme == "https")
			if env["AUTENTICO_CSRF_SECURE_COOKIE"] != secure || env["AUTENTICO_IDP_SESSION_SECURE"] != secure {
				t.Fatal("cookie security does not match the public scheme")
			}
			if _, err := os.Stat(filepath.Join(inst.dir, "autentico.db")); err != nil {
				t.Fatalf("database is not isolated in the instance directory: %v", err)
			}
			previous = env

			request := func(method, path string, form url.Values, cookies []*http.Cookie, token string) (*http.Response, string) {
				t.Helper()
				var body io.Reader
				if form != nil {
					body = strings.NewReader(form.Encode())
				}
				req, err := http.NewRequest(method, server.URL+path, body)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = public.Host
				if form != nil {
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					req.Header.Set("Origin", instanceURL(slug))
					req.Header.Set("Referer", instanceURL(slug)+"/onboard/")
				}
				for _, cookie := range cookies {
					req.AddCookie(cookie)
				}
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				content, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				return resp, string(content)
			}
			resp, page := request(http.MethodGet, "/onboard/", nil, nil, "")
			if resp.StatusCode != 200 || !strings.Contains(page, "Autentico Admin Setup") {
				t.Fatalf("onboarding page: status=%d", resp.StatusCode)
			}
			csrf := regexp.MustCompile(`name="gorilla.csrf.Token" value="([^"]+)"`).FindStringSubmatch(page)
			if len(csrf) != 2 {
				t.Fatal("missing onboarding CSRF token")
			}
			cookies := resp.Cookies()
			if len(cookies) == 0 || cookies[0].Secure != (scheme == "https") {
				t.Fatal("unexpected CSRF cookie security")
			}
			form := url.Values{
				"username": {"demo-admin"}, "email": {"admin@example.test"},
				"password": {"Demo-password-123!"}, "confirm_password": {"Demo-password-123!"},
			}
			resp, _ = request(http.MethodPost, "/onboard", form, cookies, "")
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("onboarding without CSRF token: status=%d", resp.StatusCode)
			}
			form.Set("gorilla.csrf.Token", html.UnescapeString(csrf[1]))
			resp, _ = request(http.MethodPost, "/onboard", form, cookies, "")
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin/" {
				t.Fatalf("onboarding submission: status=%d, location=%s", resp.StatusCode, resp.Header.Get("Location"))
			}
			cookies = append(cookies, resp.Cookies()...)
			resp, _ = request(http.MethodGet, "/admin/", nil, cookies, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("admin UI: status=%d", resp.StatusCode)
			}

			// Complete the admin UI's Authorization Code + PKCE flow through the proxy.
			verifier := strings.Repeat("a", 43)
			challenge := sha256.Sum256([]byte(verifier))
			redirectURI := instanceURL(slug) + "/admin/callback"
			authorize := url.Values{
				"client_id": {"autentico-admin"}, "redirect_uri": {redirectURI},
				"response_type": {"code"}, "scope": {"openid profile email"},
				"state": {"compatibility-test"}, "nonce": {"compatibility-test"},
				"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
				"code_challenge_method": {"S256"}, "prompt": {"none"},
			}
			resp, _ = request(http.MethodGet, "/oauth2/authorize?"+authorize.Encode(), nil, cookies, "")
			callback, err := url.Parse(resp.Header.Get("Location"))
			if err != nil || resp.StatusCode != http.StatusFound || callback.Query().Get("code") == "" {
				t.Fatalf("admin SSO: status=%d, callback=%s", resp.StatusCode, resp.Header.Get("Location"))
			}
			resp, body := request(http.MethodPost, "/oauth2/token", url.Values{
				"grant_type": {"authorization_code"}, "client_id": {"autentico-admin"},
				"code": {callback.Query().Get("code")}, "redirect_uri": {redirectURI},
				"code_verifier": {verifier},
			}, nil, "")
			var tokens struct {
				AccessToken string `json:"access_token"`
			}
			if err := json.Unmarshal([]byte(body), &tokens); err != nil || resp.StatusCode != 200 || tokens.AccessToken == "" {
				t.Fatalf("admin token exchange: status=%d", resp.StatusCode)
			}
			resp, _ = request(http.MethodGet, "/admin/api/users", nil, nil, tokens.AccessToken)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("authenticated admin API: status=%d", resp.StatusCode)
			}
		})
	}
	if _, err := os.Stat(os.Getenv("AUTENTICO_DB_FILE_PATH")); !os.IsNotExist(err) {
		t.Fatalf("parent database path was used: %v", err)
	}
	reg.cleanupExpired(time.Now())
	if len(reg.instances) != 2 {
		t.Fatal("cleanup removed unexpired demos")
	}
	reg.cleanupExpired(time.Now().Add(demoTTL))
	assertNoInstances(t, reg)
}

func readInstanceConfig(t *testing.T, path string) map[string]string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(content), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			values[key] = value
		}
	}
	return values
}
