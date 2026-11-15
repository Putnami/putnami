package screens

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Viewport is one window size a page is captured at.
type Viewport struct {
	Name   string
	Width  int
	Height int
	Scale  float64
	Mobile bool
}

// Viewports are the two sizes every page is captured at, desktop first.
var Viewports = []Viewport{
	{Name: "desktop", Width: 1440, Height: 900, Scale: 1},
	{Name: "mobile", Width: 390, Height: 844, Scale: 2, Mobile: true},
}

// Schemes are the two color schemes every page is captured in, light first.
var Schemes = []string{"light", "dark"}

// maxPageHeight bounds the height, in CSS pixels, of a full-page capture.
const maxPageHeight = 10000

// Shot is one capture: a URL in a viewport and a color scheme.
type Shot struct {
	URL      string
	Viewport Viewport
	Scheme   string
}

// Captured is one capture: the PNG, the HTTP status of the document, and the
// URL the page ended at, which differs from the shot's after a redirect.
type Captured struct {
	PNG    []byte
	Status int
	URL    string
}

// Capturer takes one PNG capture of a shot. Browser is the one
// implementation; tests use doubles.
type Capturer interface {
	Capture(ctx context.Context, shot Shot) (Captured, error)
}

// StorageState is a Playwright storageState document: the cookies and the
// localStorage of the origins a signed-in browser holds.
type StorageState struct {
	Cookies []StateCookie `json:"cookies"`
	Origins []StateOrigin `json:"origins"`
}

// StateCookie is one cookie of a storage state.
type StateCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"`
}

// StateOrigin is the localStorage of one origin of a storage state.
type StateOrigin struct {
	Origin       string      `json:"origin"`
	LocalStorage []StateItem `json:"localStorage"`
}

// StateItem is one localStorage entry.
type StateItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ReadStorageState reads a Playwright storageState file.
func ReadStorageState(path string) (*StorageState, error) {
	data, err := readBounded(path, 16<<20)
	if err != nil {
		return nil, fmt.Errorf("--storage-state %s: %w", path, err)
	}
	var state StorageState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("--storage-state %s is not a Playwright storageState document: %w", path, err)
	}
	for i, origin := range state.Origins {
		if _, err := url.Parse(origin.Origin); err != nil || origin.Origin == "" {
			return nil, fmt.Errorf("--storage-state %s: origins[%d] names no origin", path, i)
		}
	}
	return &state, nil
}

// cookieParams turns the storage state's cookies into Storage.setCookies
// parameters.
func (s *StorageState) cookieParams() []map[string]any {
	var cookies []map[string]any
	for _, cookie := range s.Cookies {
		param := map[string]any{"name": cookie.Name, "value": cookie.Value, "domain": cookie.Domain,
			"path": cookie.Path, "httpOnly": cookie.HTTPOnly, "secure": cookie.Secure}
		if cookie.Path == "" {
			param["path"] = "/"
		}
		if cookie.Expires > 0 {
			param["expires"] = cookie.Expires
		}
		switch cookie.SameSite {
		case "Strict", "Lax", "None":
			param["sameSite"] = cookie.SameSite
		}
		cookies = append(cookies, param)
	}
	return cookies
}

// storageScript returns the script that sets, before the page's own scripts
// run, the localStorage of every origin of the state on the host of target,
// whatever its port: the base and the head are usually served on two ports
// of one host. It returns "" when no origin matches.
func (s *StorageState) storageScript(target string) string {
	if s == nil {
		return ""
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return ""
	}
	items := map[string]string{}
	for _, origin := range s.Origins {
		o, err := url.Parse(origin.Origin)
		if err != nil || o.Hostname() != parsed.Hostname() {
			continue
		}
		for _, item := range origin.LocalStorage {
			items[item.Name] = item.Value
		}
	}
	if len(items) == 0 {
		return ""
	}
	host, _ := json.Marshal(parsed.Hostname())
	data, _ := json.Marshal(items)
	return fmt.Sprintf("(() => { if (location.hostname !== %s) return; try { for (const [k, v] of Object.entries(%s)) localStorage.setItem(k, v); } catch (e) {} })();", host, data)
}

// FindBrowser returns the Chromium executable: the --browser value, then
// PUTNAMI_BROWSER, then the Chromium a Playwright installation ships at
// /opt/pw-browsers/chromium, then a Chromium or Chrome on PATH.
func FindBrowser(flag string, getenv func(string) string, lookPath func(string) (string, error)) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := getenv("PUTNAMI_BROWSER"); env != "" {
		return env, nil
	}
	candidates := []string{"/opt/pw-browsers/chromium"}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium")
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "chrome"} {
		if path, err := lookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no Chromium found: pass --browser <path> or set PUTNAMI_BROWSER")
}

// Browser is one headless Chromium driven over the DevTools pipe.
type Browser struct {
	cmd     *exec.Cmd
	client  *client
	dataDir string
	state   *StorageState
	stderr  *boundedBuffer
	exited  chan struct{}
	// Timeout bounds one capture, from navigation to the PNG.
	Timeout time.Duration
}

// Launch starts the Chromium at path, headless, with a fresh profile, and
// sets the cookies of state when state is not nil.
func Launch(ctx context.Context, path string, state *StorageState) (*Browser, error) {
	dataDir, err := os.MkdirTemp("", "putnami-screens-")
	if err != nil {
		return nil, err
	}
	toBrowserR, toBrowserW, err := os.Pipe()
	if err != nil {
		_ = os.RemoveAll(dataDir)
		return nil, err
	}
	fromBrowserR, fromBrowserW, err := os.Pipe()
	if err != nil {
		_ = toBrowserR.Close()
		_ = toBrowserW.Close()
		_ = os.RemoveAll(dataDir)
		return nil, err
	}
	args := []string{"--headless=new", "--remote-debugging-pipe", "--user-data-dir=" + dataDir,
		"--no-first-run", "--no-default-browser-check", "--disable-gpu", "--hide-scrollbars", "--mute-audio",
		"--disable-extensions", "--disable-background-networking", "--disable-sync", "--disable-dev-shm-usage",
		"--force-color-profile=srgb", "--font-render-hinting=none"}
	if os.Geteuid() == 0 {
		// Chromium refuses to start its sandbox as root, as in a container.
		args = append(args, "--no-sandbox")
	}
	cmd := exec.Command(path, args...)
	// The browser reads its DevTools input on descriptor 3 and writes its
	// output on descriptor 4.
	cmd.ExtraFiles = []*os.File{toBrowserR, fromBrowserW}
	stderr := &boundedBuffer{limit: 8 << 10}
	cmd.Stderr = stderr
	// Wait returns at most this long after the browser exits, even when a
	// helper process it started still holds its standard error.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{toBrowserR, toBrowserW, fromBrowserR, fromBrowserW} {
			_ = f.Close()
		}
		_ = os.RemoveAll(dataDir)
		return nil, fmt.Errorf("start the browser %s: %w", path, err)
	}
	_ = toBrowserR.Close()
	_ = fromBrowserW.Close()
	b := &Browser{cmd: cmd, client: newClient(fromBrowserR, toBrowserW), dataDir: dataDir, state: state,
		stderr: stderr, exited: make(chan struct{}), Timeout: 60 * time.Second}
	go func() {
		_ = cmd.Wait()
		_ = toBrowserW.Close()
		_ = fromBrowserR.Close()
		close(b.exited)
	}()
	start, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.client.call(start, "", "Browser.getVersion", nil, nil); err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("the browser %s did not answer on its DevTools pipe: %w%s", path, err, b.stderrTail())
	}
	if err := b.setCookies(start); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

// setCookies sets the cookies of the browser's storage state, if any.
func (b *Browser) setCookies(ctx context.Context) error {
	if b.state == nil || len(b.state.Cookies) == 0 {
		return nil
	}
	if err := b.client.call(ctx, "", "Storage.setCookies", map[string]any{"cookies": b.state.cookieParams()}, nil); err != nil {
		return fmt.Errorf("set the storage state's cookies: %w", err)
	}
	return nil
}

func (b *Browser) stderrTail() string {
	text := strings.TrimSpace(b.stderr.String())
	if text == "" {
		return ""
	}
	return "; the browser wrote: " + text
}

// Close stops the browser and removes its profile.
func (b *Browser) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = b.client.call(ctx, "", "Browser.close", nil, nil)
	cancel()
	select {
	case <-b.exited:
	case <-time.After(5 * time.Second):
		_ = b.cmd.Process.Kill()
		<-b.exited
	}
	return os.RemoveAll(b.dataDir)
}

// Capture opens shot.URL in a new tab sized and colored as the shot says,
// waits for its load event and its fonts, and returns a full-page PNG and the
// HTTP status of the document, and the URL the page ended at.
func (b *Browser) Capture(ctx context.Context, shot Shot) (Captured, error) {
	ctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := b.client.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return Captured{}, err
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = b.client.call(closeCtx, "", "Target.closeTarget", map[string]any{"targetId": target.TargetID}, nil)
		closeCancel()
	}()
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.client.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		return Captured{}, err
	}
	session := attached.SessionID
	call := func(method string, params, result any) error {
		return b.client.call(ctx, session, method, params, result)
	}
	setup := []struct {
		method string
		params any
	}{
		{"Page.enable", nil},
		{"Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}},
		{"Network.enable", nil},
		{"Emulation.setDeviceMetricsOverride", map[string]any{"width": shot.Viewport.Width, "height": shot.Viewport.Height,
			"deviceScaleFactor": shot.Viewport.Scale, "mobile": shot.Viewport.Mobile}},
		{"Emulation.setTouchEmulationEnabled", map[string]any{"enabled": shot.Viewport.Mobile}},
		{"Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": shot.Scheme}}}},
	}
	for _, step := range setup {
		if err := call(step.method, step.params, nil); err != nil {
			return Captured{}, err
		}
	}
	if script := b.state.storageScript(shot.URL); script != "" {
		if err := call("Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": script}, nil); err != nil {
			return Captured{}, err
		}
	}
	nav := newNavigation(session)
	remove := b.client.listen(nav.observe)
	defer remove()
	var navigated struct {
		FrameID   string `json:"frameId"`
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	if err := call("Page.navigate", map[string]any{"url": shot.URL}, &navigated); err != nil {
		return Captured{}, err
	}
	if navigated.ErrorText != "" {
		return Captured{}, fmt.Errorf("navigate to %s: %s", shot.URL, navigated.ErrorText)
	}
	status, err := nav.wait(ctx, navigated.LoaderID, 3*time.Second)
	if err != nil {
		return Captured{}, fmt.Errorf("load %s: %w", shot.URL, err)
	}
	settle := "document.fonts ? document.fonts.ready.then(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r(true))))) : true"
	if err := call("Runtime.evaluate", map[string]any{"expression": settle, "awaitPromise": true}, nil); err != nil {
		return Captured{}, err
	}
	var location struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := call("Runtime.evaluate", map[string]any{"expression": "location.href", "returnByValue": true}, &location); err != nil {
		return Captured{}, err
	}
	var metrics struct {
		CSSContentSize struct {
			Width  float64 `json:"width"`
			Height float64 `json:"height"`
		} `json:"cssContentSize"`
	}
	if err := call("Page.getLayoutMetrics", nil, &metrics); err != nil {
		return Captured{}, err
	}
	width := clamp(metrics.CSSContentSize.Width, float64(shot.Viewport.Width), float64(3*shot.Viewport.Width))
	height := clamp(metrics.CSSContentSize.Height, float64(shot.Viewport.Height), maxPageHeight)
	var shotResult struct {
		Data string `json:"data"`
	}
	if err := call("Page.captureScreenshot", map[string]any{"format": "png", "captureBeyondViewport": true,
		"clip": map[string]any{"x": 0, "y": 0, "width": width, "height": height, "scale": 1}}, &shotResult); err != nil {
		return Captured{}, err
	}
	png, err := base64.StdEncoding.DecodeString(shotResult.Data)
	if err != nil {
		return Captured{}, fmt.Errorf("Page.captureScreenshot answered data that is not base64: %w", err)
	}
	return Captured{PNG: png, Status: status, URL: location.Result.Value}, nil
}

func clamp(value, low, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// navigation follows one page load in one session: the status of the
// document each loader receives and the lifecycle events of each loader.
type navigation struct {
	session string

	mu        sync.Mutex
	statuses  map[string]int
	lifecycle map[string]map[string]bool
	signal    chan struct{}
}

func newNavigation(session string) *navigation {
	return &navigation{session: session, statuses: map[string]int{}, lifecycle: map[string]map[string]bool{},
		signal: make(chan struct{}, 1)}
}

func (n *navigation) observe(ev event) bool {
	if ev.SessionID != n.session {
		return false
	}
	switch ev.Method {
	case "Network.responseReceived":
		var params struct {
			LoaderID string `json:"loaderId"`
			Type     string `json:"type"`
			Response struct {
				Status int `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal(ev.Params, &params) != nil || params.Type != "Document" {
			return false
		}
		n.mu.Lock()
		if _, seen := n.statuses[params.LoaderID]; !seen {
			n.statuses[params.LoaderID] = params.Response.Status
		}
		n.mu.Unlock()
	case "Page.lifecycleEvent":
		var params struct {
			LoaderID string `json:"loaderId"`
			Name     string `json:"name"`
		}
		if json.Unmarshal(ev.Params, &params) != nil {
			return false
		}
		n.mu.Lock()
		if n.lifecycle[params.LoaderID] == nil {
			n.lifecycle[params.LoaderID] = map[string]bool{}
		}
		n.lifecycle[params.LoaderID][params.Name] = true
		n.mu.Unlock()
	default:
		return false
	}
	select {
	case n.signal <- struct{}{}:
	default:
	}
	return false
}

// wait returns the status of the loader's document once the loader's load
// event fired, then waits at most idle for its network to go idle.
func (n *navigation) wait(ctx context.Context, loader string, idle time.Duration) (int, error) {
	if loader == "" {
		return 0, errors.New("the navigation started no document load")
	}
	var status int
	for {
		n.mu.Lock()
		known := false
		status, known = n.statuses[loader]
		loaded := n.lifecycle[loader]["load"]
		n.mu.Unlock()
		if loaded && known {
			break
		}
		select {
		case <-ctx.Done():
			if !known {
				return 0, errors.New("no document response before the deadline")
			}
			return 0, errors.New("no load event before the deadline")
		case <-n.signal:
		}
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		n.mu.Lock()
		quiet := n.lifecycle[loader]["networkIdle"]
		n.mu.Unlock()
		if quiet {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return status, nil
		case <-timer.C:
			return status, nil
		case <-n.signal:
		}
	}
}

// boundedBuffer keeps the first limit bytes written to it.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// readBounded reads a file of at most limit bytes.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("exceeds %d bytes", limit)
	}
	return data, nil
}
