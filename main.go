package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	baseDomain   = getEnv("BASE_DOMAIN", "demo.autentico.top")
	baseScheme   = getEnv("BASE_SCHEME", "https")
	basePort     = getEnv("BASE_PORT", "")
	binary       = getEnv("AUTENTICO_BINARY", "./autentico")
	dataDir      = getEnv("DATA_DIR", "./demos")
	listenAddr   = getEnv("LISTEN_ADDR", ":8080")
	demoTTL      = parseDuration(getEnv("DEMO_TTL", "24h"), 24*time.Hour)
	maxInstances = parseInt(getEnv("MAX_INSTANCES", "15"), 15)
	maxPerIP     = parseInt(getEnv("MAX_PER_IP", "3"), 3)
)

type instance struct {
	slug      string
	port      int
	cmd       *exec.Cmd
	dir       string
	proxy     *httputil.ReverseProxy
	expiresAt time.Time
	creatorIP string
}

type registry struct {
	mu        sync.RWMutex
	instances map[string]*instance
}

func main() {
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		slog.Error("failed to create data dir", "err", err)
		os.Exit(1)
	}

	reg := &registry{instances: make(map[string]*instance)}
	go reg.runCleanup()

	http.HandleFunc("/launch", reg.handleLaunchPage)
	http.HandleFunc("/launch/start", reg.handleLaunchStart)
	http.HandleFunc("/status", reg.handleStatus)
	http.HandleFunc("/", reg.handleProxy)

	slog.Info("provisioner listening", "addr", listenAddr, "domain", baseDomain)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func (r *registry) handleLaunchPage(w http.ResponseWriter, req *http.Request) {
	renderPage(w, http.StatusOK, "Launching demo — Autentico", launchCardHTML)
}

func (r *registry) handleLaunchStart(w http.ResponseWriter, req *http.Request) {
	ip := clientIP(req)

	r.mu.RLock()
	total := len(r.instances)
	perIP := 0
	for _, inst := range r.instances {
		if inst.creatorIP == ip {
			perIP++
		}
	}
	r.mu.RUnlock()

	if total >= maxInstances {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"server is at capacity, please try again later"}`)
		return
	}
	if maxPerIP > 0 && perIP >= maxPerIP {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"demo limit reached for your IP — wait for an existing demo to expire"}`)
		return
	}

	inst, err := r.launch(ip)
	if err != nil {
		slog.Error("failed to launch demo", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"failed to launch demo, please try again"}`)
		return
	}

	target := instanceURL(inst.slug) + "/admin/"
	slog.Info("launched demo", "slug", inst.slug, "port", inst.port, "ip", ip, "expires", inst.expiresAt.Format(time.RFC3339))
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"url":%q}`, target)
}

func (r *registry) handleStatus(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "instances: %d / %d\n\n", len(r.instances), maxInstances)
	for slug, inst := range r.instances {
		ttl := time.Until(inst.expiresAt).Round(time.Minute)
		fmt.Fprintf(w, "%s  port=%-6d  ip=%-15s  expires in %s\n", slug, inst.port, inst.creatorIP, ttl)
	}
}

func (r *registry) handleProxy(w http.ResponseWriter, req *http.Request) {
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	suffix := "." + baseDomain
	if !strings.HasSuffix(strings.ToLower(host), suffix) {
		http.NotFound(w, req)
		return
	}

	slug := strings.TrimSuffix(strings.ToLower(host), suffix)

	r.mu.RLock()
	inst, ok := r.instances[slug]
	r.mu.RUnlock()

	if !ok {
		expiredPage(w, baseDomain)
		return
	}

	inst.proxy.ServeHTTP(w, req)
}

func (r *registry) launch(creatorIP string) (*instance, error) {
	slug, err := generateSlug()
	if err != nil {
		return nil, fmt.Errorf("generate slug: %w", err)
	}

	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("find port: %w", err)
	}

	dir := filepath.Join(dataDir, slug)
	if err := os.MkdirAll(filepath.Join(dir, "db"), 0750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	logFile, err := os.Create(filepath.Join(dir, "autentico.log"))
	if err != nil {
		return nil, fmt.Errorf("create log file: %w", err)
	}

	publicURL := instanceURL(slug)
	cmd := exec.Command(binary, "start")
	// Our vars come first so they take precedence over any duplicates in os.Environ()
	// (on Linux, getenv returns the first match in the env array)
	cmd.Env = append([]string{
		"AUTENTICO_APP_URL=" + publicURL,
		fmt.Sprintf("AUTENTICO_LISTEN_PORT=%d", port),
		"AUTENTICO_DB_FILE_PATH=" + filepath.Join(dir, "db", "autentico.db"),
		"AUTENTICO_ACCESS_TOKEN_SECRET=" + randomHex(32),
		"AUTENTICO_REFRESH_TOKEN_SECRET=" + randomHex(32),
		"AUTENTICO_CSRF_SECRET_KEY=" + randomHex(32),
		"AUTENTICO_APP_ENABLE_CORS=true",
		"AUTENTICO_CSRF_SECURE_COOKIE=true",
		"AUTENTICO_REFRESH_TOKEN_SECURE=true",
		"AUTENTICO_IDP_SESSION_SECURE=true",
	}, os.Environ()...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start autentico: %w", err)
	}

	if err := waitForReady(port, 3*time.Minute); err != nil {
		cmd.Process.Kill()
		logFile.Close()
		return nil, fmt.Errorf("autentico did not start: %w", err)
	}

	target, _ := url.Parse(fmt.Sprintf("http://localhost:%d", port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("proxy error", "slug", slug, "err", err)
		warmingPage(w, baseDomain)
	}

	inst := &instance{
		slug:      slug,
		port:      port,
		cmd:       cmd,
		dir:       dir,
		proxy:     proxy,
		expiresAt: time.Now().Add(demoTTL),
		creatorIP: creatorIP,
	}

	r.mu.Lock()
	r.instances[slug] = inst
	r.mu.Unlock()

	return inst, nil
}

func (r *registry) runCleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		r.mu.Lock()
		for slug, inst := range r.instances {
			if time.Now().After(inst.expiresAt) {
				slog.Info("expiring demo", "slug", slug)
				_ = inst.cmd.Process.Kill()
				_ = inst.cmd.Wait()
				_ = os.RemoveAll(inst.dir)
				delete(r.instances, slug)
			}
		}
		r.mu.Unlock()
	}
}

// ── HTML pages ───────────────────────────────────────────────────────────────

func renderPage(w http.ResponseWriter, status int, title, card string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	page := strings.ReplaceAll(pageShell, "{{TITLE}}", title)
	page = strings.ReplaceAll(page, "{{CARD}}", card)
	fmt.Fprint(w, page)
}

func expiredPage(w http.ResponseWriter, domain string) {
	card := strings.ReplaceAll(expiredCardHTML, "{{DOMAIN}}", domain)
	renderPage(w, http.StatusGone, "Demo Expired — Autentico", card)
}

func warmingPage(w http.ResponseWriter, domain string) {
	card := strings.ReplaceAll(warmingCardHTML, "{{DOMAIN}}", domain)
	renderPage(w, http.StatusServiceUnavailable, "Demo Starting — Autentico", card)
}

// pageShell is the full HTML skeleton. {{TITLE}} and {{CARD}} are replaced at render time.
const pageShell = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>{{TITLE}}</title>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Inter:wght@200;300;500&display=swap" rel="stylesheet">
  <style>
    :root {
      --font-family: "Inter", system-ui, -apple-system, sans-serif;
      --border-radius: 10px;
      --card-width: 420px;
      --card-padding: 36px;
      --card-gap: 1.2rem;
      --color-primary: #5b8cff;
      --color-primary-glow: rgba(91, 140, 255, 0.25);
      --color-accent: #1aa070;
      --color-danger: #ff5a5a;
      --color-text: #e8eaf0;
      --color-text-muted: #8b90a0;
      --color-inverse: #ffffff;
      --color-background: #080c14;
      --color-card: rgba(255, 255, 255, 0.025);
      --color-card-border: rgba(255, 255, 255, 0.08);
      --color-input-border: rgba(255, 255, 255, 0.13);
      --color-logo-bg: rgba(255, 255, 255, 0.08);
    }
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    html, body { height: 100%; }
    body {
      display: flex; flex-direction: column;
      align-items: center; justify-content: center;
      min-height: 100dvh;
      font-family: var(--font-family);
      font-weight: 300;
      background-color: var(--color-background);
      color: var(--color-text);
      -webkit-font-smoothing: antialiased;
      -moz-osx-font-smoothing: grayscale;
    }
    #bg-canvas { position: fixed; inset: 0; width: 100%; height: 100%; z-index: 0; }
    main {
      position: relative; z-index: 1; flex-grow: 1;
      display: flex; width: 100%;
      align-items: center; justify-content: center;
      padding: 1.5rem;
    }
    footer { position: relative; z-index: 1; }
    .card {
      background: var(--color-card);
      backdrop-filter: blur(20px);
      -webkit-backdrop-filter: blur(20px);
      border: 1px solid var(--color-card-border);
      box-shadow: 0 8px 40px rgba(0,0,0,.5), inset 0 1px 0 rgba(255,255,255,.08);
      padding: var(--card-padding);
      border-radius: var(--border-radius);
      width: 100%; max-width: var(--card-width);
      display: flex; flex-direction: column;
      align-items: center; gap: var(--card-gap);
      text-align: center;
    }
    .logo {
      width: 72px; height: 72px;
      display: flex; justify-content: center; align-items: center;
      border-radius: 9999px;
      background: var(--color-logo-bg);
      border: 1px solid var(--color-input-border);
    }
    .logo svg { max-width: 100%; max-height: 100%; }
    h1 { font-size: 1.6rem; font-weight: 200; letter-spacing: 0.04em; color: var(--color-inverse); }
    .subtitle { color: var(--color-text-muted); font-size: 0.9rem; line-height: 1.7; }
    .spinner {
      width: 36px; height: 36px;
      border: 2px solid rgba(91, 140, 255, 0.2);
      border-top-color: var(--color-primary);
      border-radius: 50%;
      animation: spin 0.8s linear infinite;
    }
    @keyframes spin { to { transform: rotate(360deg); } }
    .steps { list-style: none; width: 100%; display: flex; flex-direction: column; gap: 0.3rem; text-align: left; }
    .step {
      padding: 5px 10px; border-radius: 6px;
      font-size: 0.875rem; color: var(--color-text-muted);
      display: flex; align-items: center; gap: 0.6rem;
      transition: color 0.3s ease;
    }
    .si { font-size: 0.7rem; width: 1rem; text-align: center; flex-shrink: 0; }
    .step.active { color: var(--color-text); }
    .step.active .si { color: var(--color-primary); }
    .step.done { color: var(--color-accent); }
    .step.done .si { color: var(--color-accent); }
    .note { color: var(--color-text-muted); font-size: 0.85rem; line-height: 1.6; }
    .error-box {
      width: 100%;
      padding: 10px 14px; border-radius: 8px;
      background: rgba(255, 90, 90, 0.1);
      border: 1px solid rgba(255, 90, 90, 0.25);
      color: var(--color-danger); font-size: 0.9rem; line-height: 1.5;
    }
    a { color: var(--color-primary); text-decoration: none; }
    a:hover { text-decoration: underline; }
    .btn {
      display: inline-block;
      padding: 10px 24px;
      background: linear-gradient(135deg, #5b8cff 0%, #2e5bff 100%);
      color: #fff; border: none; border-radius: 8px;
      font-family: var(--font-family); font-size: 0.9rem; font-weight: 500;
      letter-spacing: 0.05em; text-transform: uppercase;
      cursor: pointer; text-decoration: none;
      box-shadow: 0 4px 20px rgba(46, 91, 255, 0.35);
      transition: box-shadow 0.15s ease;
    }
    .btn:hover { box-shadow: 0 0 22px rgba(91, 140, 255, 0.5); text-decoration: none; }
  </style>
</head>
<body>
  <canvas id="bg-canvas"></canvas>
  <main>
    <div class="card">
      <div class="logo">
        <svg version="1.1" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" xml:space="preserve">
          <circle style="fill:#FF5419;" cx="256" cy="256" r="256"/>
          <path style="fill:#C92F00;" d="M134.325,337.401l170.02,170.02c102.64-19.62,183.51-100.516,203.092-203.169L378.859,175.673 l-32.035,35.047l-66.396-66.396l-29.537,45.08l60.302,60.302l-3.089,3.379L134.325,337.401z"/>
          <path style="fill:#FFEDB5;" d="M366.689,341.333H145.31c-9.561,0-17.31-7.751-17.31-17.311V187.976c0-9.559,7.749-17.31,17.31-17.31 h221.379c9.561,0,17.311,7.751,17.311,17.31v136.045C384,333.583,376.249,341.333,366.689,341.333z"/>
          <path style="fill:#FEE187;" d="M366.689,170.667H256v170.667h110.689c9.561,0,17.311-7.751,17.311-17.311V187.976 C384,178.417,376.249,170.667,366.689,170.667z"/>
          <path style="fill:#02B2B2;" d="M274.244,199.111h-36.488c-5.634,0-10.2-4.567-10.2-10.2v-36.488c0-5.634,4.567-10.2,10.2-10.2 h36.488c5.634,0,10.2,4.567,10.2,10.2v36.488C284.444,194.544,279.878,199.111,274.244,199.111z"/>
          <path style="fill:#386895;" d="M274.244,142.222H256v56.889h18.244c5.634,0,10.2-4.567,10.2-10.2v-36.488 C284.444,146.789,279.878,142.222,274.244,142.222z"/>
          <rect x="270.222" y="227.556" style="fill:#FFC61B;" width="85.333" height="85.333"/>
          <path style="fill:#A6A8AA;" d="M223.956,264.62H145.82c-1.988,0-3.598-1.612-3.598-3.598V259.6c0-1.988,1.612-3.598,3.598-3.598 h78.136c1.988,0,3.598,1.612,3.598,3.598v1.422C227.556,263.008,225.944,264.62,223.956,264.62z"/>
          <path style="fill:#A6A8AA;" d="M223.956,302.976H145.82c-1.988,0-3.598-1.612-3.598-3.598v-1.422c0-1.988,1.612-3.598,3.598-3.598 h78.136c1.988,0,3.598,1.612,3.598,3.598v1.422C227.556,301.365,225.944,302.976,223.956,302.976z"/>
          <path style="fill:#A6A8AA;" d="M223.956,283.152H145.82c-1.988,0-3.598-1.612-3.598-3.598v-1.422c0-1.988,1.612-3.598,3.598-3.598 h78.136c1.988,0,3.598,1.612,3.598,3.598v1.422C227.556,281.54,225.944,283.152,223.956,283.152z"/>
        </svg>
      </div>
      {{CARD}}
    </div>
  </main>
  <footer></footer>
  <script>
    (function () {
      const canvas = document.getElementById('bg-canvas');
      const ctx = canvas.getContext('2d');
      const MOBILE = window.innerWidth < 768;
      const COUNT = MOBILE ? 70 : 160;
      function resize() { canvas.width = window.innerWidth; canvas.height = window.innerHeight; }
      function mkStar() {
        return {
          x: Math.random() * canvas.width, y: Math.random() * canvas.height,
          r: Math.random() * 1.2 + 0.2,
          baseOpacity: Math.random() * 0.55 + 0.15,
          twinkleSpeed: Math.random() * 0.025 + 0.004,
          twinkleOffset: Math.random() * Math.PI * 2,
          dx: (Math.random() - 0.5) * 0.06, dy: (Math.random() - 0.5) * 0.06,
        };
      }
      resize();
      const stars = Array.from({ length: COUNT }, mkStar);
      let t = 0, shooter = null;
      function spawnShooter() {
        const angle = Math.PI / 4 + (Math.random() - 0.5) * 0.4;
        shooter = {
          x: Math.random() * canvas.width * 0.7, y: Math.random() * canvas.height * 0.4,
          angle, speed: 5 + Math.random() * 3, length: 180 + Math.random() * 80,
          life: 0, maxLife: 120 + Math.random() * 30,
        };
        setTimeout(spawnShooter, 6500 + Math.random() * 2000);
      }
      setTimeout(spawnShooter, 3000 + Math.random() * 4000);
      function drawShooter() {
        if (!shooter) return;
        shooter.life++;
        const progress = shooter.life / shooter.maxLife;
        const opacity = progress < 0.15 ? progress / 0.15 : progress > 0.75 ? 1 - (progress - 0.75) / 0.25 : 1;
        shooter.x += Math.cos(shooter.angle) * shooter.speed;
        shooter.y += Math.sin(shooter.angle) * shooter.speed;
        const tailX = shooter.x - Math.cos(shooter.angle) * shooter.length;
        const tailY = shooter.y - Math.sin(shooter.angle) * shooter.length;
        const grad = ctx.createLinearGradient(tailX, tailY, shooter.x, shooter.y);
        grad.addColorStop(0, 'rgba(255,255,255,0)');
        grad.addColorStop(1, 'rgba(255,255,255,' + (opacity * 0.9) + ')');
        ctx.save();
        ctx.strokeStyle = grad; ctx.lineWidth = 1.5;
        ctx.shadowBlur = 6; ctx.shadowColor = '#c8d8ff';
        ctx.beginPath(); ctx.moveTo(tailX, tailY); ctx.lineTo(shooter.x, shooter.y);
        ctx.stroke(); ctx.restore();
        if (shooter.life >= shooter.maxLife) shooter = null;
      }
      function frame() {
        ctx.clearRect(0, 0, canvas.width, canvas.height);
        t++;
        for (const s of stars) {
          const opacity = s.baseOpacity * (0.55 + 0.45 * Math.sin(t * s.twinkleSpeed + s.twinkleOffset));
          s.x += s.dx; s.y += s.dy;
          if (s.x < 0) s.x = canvas.width; else if (s.x > canvas.width) s.x = 0;
          if (s.y < 0) s.y = canvas.height; else if (s.y > canvas.height) s.y = 0;
          ctx.save();
          ctx.globalAlpha = opacity; ctx.shadowBlur = s.r * 8; ctx.shadowColor = '#c8d8ff';
          ctx.fillStyle = '#ffffff';
          ctx.beginPath(); ctx.arc(s.x, s.y, s.r, 0, Math.PI * 2); ctx.fill();
          ctx.restore();
        }
        drawShooter();
        requestAnimationFrame(frame);
      }
      window.addEventListener('resize', resize);
      frame();
    })();
  </script>
</body>
</html>`

const launchCardHTML = `
  <h1>Setting up your demo</h1>
  <p class="subtitle">
    This usually takes a few seconds.<br>
    On busy days it might take a minute or two — hang tight.
  </p>
  <div class="spinner" id="spinner"></div>
  <ul class="steps">
    <li class="step active" id="s1"><span class="si">●</span>Allocating a fresh instance</li>
    <li class="step"        id="s2"><span class="si">○</span>Starting Autentico</li>
    <li class="step"        id="s3"><span class="si">○</span>Waiting for service to come online</li>
    <li class="step"        id="s4"><span class="si">○</span>Redirecting you</li>
  </ul>
  <p class="note" id="slow" style="display:none">
    Still starting up — this can take up to 3 minutes on first boot.
  </p>
  <div class="error-box" id="err" style="display:none">
    <span id="err-msg">Something went wrong.</span>
  </div>
  <a class="btn" href="/launch" id="retry-btn" style="display:none">Try again</a>
  <script>
    function setStep(n) {
      for (var i = 1; i <= 4; i++) {
        var el = document.getElementById('s' + i);
        var icon = el.querySelector('.si');
        if (i < n)       { el.className = 'step done';   icon.textContent = '✓'; }
        else if (i === n){ el.className = 'step active'; icon.textContent = '●'; }
        else             { el.className = 'step';        icon.textContent = '○'; }
      }
    }
    var t2   = setTimeout(function(){ setStep(2); }, 3000);
    var t3   = setTimeout(function(){ setStep(3); }, 8000);
    var slow = setTimeout(function(){ document.getElementById('slow').style.display = ''; }, 22000);
    fetch('/launch/start')
      .then(function(r){ return r.ok ? r.json() : r.json().then(function(e){ return Promise.reject(e); }); })
      .then(function(data) {
        clearTimeout(t2); clearTimeout(t3); clearTimeout(slow);
        setStep(4);
        setTimeout(function(){ window.location.href = data.url; }, 300);
      })
      .catch(function(e) {
        clearTimeout(t2); clearTimeout(t3); clearTimeout(slow);
        document.getElementById('spinner').style.display = 'none';
        document.getElementById('slow').style.display = 'none';
        var msg = (e && e.error) ? e.error : 'Something went wrong. Please try again.';
        document.getElementById('err-msg').textContent = msg;
        document.getElementById('err').style.display = '';
        document.getElementById('retry-btn').style.display = '';
      });
  </script>`

const expiredCardHTML = `
  <h1>Demo expired</h1>
  <p class="subtitle">
    This demo has been automatically cleaned up after 24 hours.<br><br>
    If you just launched a new demo and ended up here, the instance
    may still be starting up — wait a minute or two and refresh the page.
  </p>
  <a class="btn" href="https://{{DOMAIN}}/launch">Start a new demo</a>`

const warmingCardHTML = `
  <h1>Instance warming up</h1>
  <p class="subtitle">
    Your demo is still coming online. This can take a minute or two
    on a cold start.<br><br>
    Wait a moment and refresh the page — it will be ready shortly.
  </p>
  <a class="btn" href="javascript:location.reload()">Refresh</a>
  <p class="note">
    Or <a href="https://{{DOMAIN}}/launch">start a fresh demo</a> if this keeps happening.
  </p>`

// ── Helpers ──────────────────────────────────────────────────────────────────

func generateSlug() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b), nil
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// waitForReady polls the OIDC discovery endpoint until it returns 200,
// which means the instance is fully initialised and ready to serve traffic.
func waitForReady(port int, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("http://localhost:%d/.well-known/openid-configuration", port)
	for time.Now().Before(deadline) {
		resp, err := client.Get(addr)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("instance on port %d not ready after %s", port, timeout)
}

// clientIP extracts the real client IP, honouring X-Forwarded-For when present.
func clientIP(req *http.Request) string {
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first (leftmost) address — the original client.
		if ip, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(ip)
		}
		return strings.TrimSpace(xff)
	}
	if xri := req.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	host, _, _ := net.SplitHostPort(req.RemoteAddr)
	return host
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// instanceURL builds the public-facing URL for a demo slug.
// Uses BASE_SCHEME and BASE_PORT so local testing works without TLS.
func instanceURL(slug string) string {
	host := slug + "." + baseDomain
	if basePort != "" {
		host = host + ":" + basePort
	}
	return baseScheme + "://" + host
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

func parseInt(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
