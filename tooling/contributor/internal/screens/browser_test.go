package screens

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"
)

const (
	screensFeature = "tooling/change-screenshots"
	touchedPages   = "touched-pages"
	visitedPath    = "visited-path"
	frozenMotion   = "frozen-motion"
	capturesReq    = "captures"
	finalizeReq    = "finalize-asks-for-screenshots"
	agentReq       = "agent-reviews-its-screens"
)

// TestTheClientFramesCallsAndDispatchesAnswersAndEvents runs the client
// against a peer that answers each call out of order and sends an event in
// between.
func TestTheClientFramesCallsAndDispatchesAnswersAndEvents(t *testing.T) {
	t.Parallel()
	toPeerR, toPeerW := io.Pipe()
	fromPeerR, fromPeerW := io.Pipe()
	c := newClient(fromPeerR, toPeerW)
	events := make(chan event, 4)
	remove := c.listen(func(ev event) bool { events <- ev; return false })
	defer remove()

	go func() {
		reader := newFrameReader(toPeerR)
		var calls []message
		for len(calls) < 2 {
			data, err := reader()
			if err != nil {
				return
			}
			var msg message
			_ = json.Unmarshal(data, &msg)
			calls = append(calls, msg)
		}
		write := func(v any) {
			data, _ := json.Marshal(v)
			_, _ = fromPeerW.Write(append(data, 0))
		}
		write(map[string]any{"method": "Page.lifecycleEvent", "sessionId": "s", "params": map[string]string{"name": "load"}})
		for i := len(calls) - 1; i >= 0; i-- {
			if calls[i].Method == "Broken.call" {
				write(map[string]any{"id": calls[i].ID, "error": map[string]any{"code": -32601, "message": "not found"}})
				continue
			}
			write(map[string]any{"id": calls[i].ID, "sessionId": calls[i].SessionID, "result": map[string]string{"echo": calls[i].Method}})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	var got struct {
		Echo string `json:"echo"`
	}
	var okErr, brokenErr error
	go func() { defer wg.Done(); okErr = c.call(ctx, "s", "Page.enable", nil, &got) }()
	go func() { defer wg.Done(); brokenErr = c.call(ctx, "", "Broken.call", map[string]int{"a": 1}, nil) }()
	wg.Wait()
	if okErr != nil || got.Echo != "Page.enable" {
		t.Fatalf("Page.enable = %q, %v", got.Echo, okErr)
	}
	if brokenErr == nil || !strings.Contains(brokenErr.Error(), "not found (-32601)") {
		t.Fatalf("Broken.call error = %v, want the browser's error", brokenErr)
	}
	select {
	case ev := <-events:
		if ev.Method != "Page.lifecycleEvent" || ev.SessionID != "s" {
			t.Fatalf("event = %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("no event dispatched")
	}
	_ = fromPeerW.Close()
	<-c.done
	if err := c.call(ctx, "", "Browser.close", nil, nil); err == nil || !strings.Contains(err.Error(), "closed its DevTools pipe") {
		t.Fatalf("call after the pipe closed = %v", err)
	}
}

// newFrameReader reads NUL-terminated frames from r.
func newFrameReader(r io.Reader) func() ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, 4096)
	return func() ([]byte, error) {
		for {
			if i := bytes.IndexByte(buf.Bytes(), 0); i >= 0 {
				frame := append([]byte(nil), buf.Bytes()[:i]...)
				buf.Next(i + 1)
				return frame, nil
			}
			n, err := r.Read(chunk)
			buf.Write(chunk[:n])
			if err != nil {
				return nil, err
			}
		}
	}
}

// TestFindBrowserPrefersTheFlagThenTheEnvironmentThenPath walks the browser
// lookup order.
func TestFindBrowserPrefersTheFlagThenTheEnvironmentThenPath(t *testing.T) {
	t.Parallel()
	env := func(value string) func(string) string {
		return func(name string) string {
			if name == "PUTNAMI_BROWSER" {
				return value
			}
			return ""
		}
	}
	none := func(string) (string, error) { return "", exec.ErrNotFound }
	if got, _ := FindBrowser("/x/chrome", env("/y/chrome"), none); got != "/x/chrome" {
		t.Fatalf("flag: %q", got)
	}
	if got, _ := FindBrowser("", env("/y/chrome"), none); got != "/y/chrome" {
		t.Fatalf("environment: %q", got)
	}
	// A Chromium installed at a well-known place wins over PATH: walk PATH only without one.
	if installed := installedBrowser(); installed != "" {
		t.Logf("PATH lookup not walked: %s is installed", installed)
	} else {
		onPath := func(name string) (string, error) {
			if name == "google-chrome" {
				return "/usr/bin/google-chrome", nil
			}
			return "", exec.ErrNotFound
		}
		if got, _ := FindBrowser("", env(""), onPath); got != "/usr/bin/google-chrome" {
			t.Fatalf("path: %q", got)
		}
		if _, err := FindBrowser("", env(""), none); err == nil || !strings.Contains(err.Error(), "--browser") {
			t.Fatalf("none: %v", err)
		}
	}
}

// TestTheStorageScriptMatchesTheHostWhateverThePort sets the localStorage of
// an origin on another port of the same host, and of no other host.
func TestTheStorageScriptMatchesTheHostWhateverThePort(t *testing.T) {
	t.Parallel()
	state := &StorageState{Origins: []StateOrigin{
		{Origin: "http://127.0.0.1:3000", LocalStorage: []StateItem{{Name: "token", Value: "t"}}},
		{Origin: "http://example.test", LocalStorage: []StateItem{{Name: "other", Value: "o"}}},
	}}
	script := state.storageScript("http://127.0.0.1:4000/tasks")
	if !strings.Contains(script, `"token":"t"`) || strings.Contains(script, "other") {
		t.Fatalf("script = %s", script)
	}
	if state.storageScript("http://localhost:4000/") != "" {
		t.Fatal("another host got a script")
	}
	var nilState *StorageState
	if nilState.storageScript("http://127.0.0.1/") != "" {
		t.Fatal("a nil state wrote a script")
	}
}

// testBrowser launches the Chromium this machine has. The capture tests drive
// a real browser; a machine without one cannot run them.
func testBrowser(t *testing.T, state *StorageState) *Browser {
	t.Helper()
	path, err := FindBrowser("", os.Getenv, exec.LookPath)
	if err != nil {
		t.Skipf("no Chromium on this machine: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	browser, err := Launch(ctx, path, state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.Close() })
	return browser
}

// colorPage serves a page whose background follows the color scheme, a 404,
// and a page that shows the session cookie and the localStorage token it
// receives.
func colorPage(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width"><style>
html,body{margin:0;background:#ffffff;height:100%}
@media (prefers-color-scheme: dark){html,body{background:#000000}}
</style></head><body></body></html>`)
	})
	mux.HandleFunc("/animated", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width"><style>
@keyframes fade{from{background:#ffffff}to{background:#000000}}
html,body{margin:0;height:100%;background:#ffffff;animation:fade 1s linear infinite}
</style></head><body></body></html>`)
	})
	mux.HandleFunc("/entering", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width"><style>
@keyframes appear{from{opacity:0}to{opacity:1}}
html,body{margin:0;height:100%;background:#ffffff}
.quick,.slow{position:fixed;top:0;bottom:0;width:50%;background:#000000;opacity:0;animation:appear forwards}
.quick{left:0;animation-duration:50ms}
.slow{right:0;animation-duration:20s}
</style></head><body><div class="quick"></div><div class="slow"></div>
<script>document.querySelector(".slow").addEventListener("animationend", () => { location.href = "/missing" })</script></body></html>`)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie("session")
		value := ""
		if cookie != nil {
			value = cookie.Value
		}
		fmt.Fprintf(w, `<!doctype html><html><body data-cookie=%q><script>document.body.dataset.token = localStorage.getItem("token") || "";</script></body></html>`, value)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func centerLuma(t *testing.T, data []byte) uint32 {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Dx()/2, b.Dy()/2).RGBA()
	return (r + g + bl) / 3 >> 8
}

// TestTheBrowserCapturesEachViewportAndScheme captures one page at both
// viewports and both schemes, and reads the size and the background of each
// PNG.
func TestTheBrowserCapturesEachViewportAndScheme(t *testing.T) {
	browser := testBrowser(t, nil)
	server := colorPage(t)
	for _, viewport := range Viewports {
		for _, scheme := range Schemes {
			got, err := browser.Capture(context.Background(), Shot{URL: server.URL + "/", Viewport: viewport, Scheme: scheme})
			if err != nil {
				t.Fatalf("%s %s: %v", viewport.Name, scheme, err)
			}
			data, status := got.PNG, got.Status
			if got.URL != server.URL+"/" {
				t.Fatalf("%s %s: ended at %s", viewport.Name, scheme, got.URL)
			}
			if status != 200 {
				t.Fatalf("%s %s: status %d", viewport.Name, scheme, status)
			}
			img, err := png.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			wantWidth := int(float64(viewport.Width) * viewport.Scale)
			if img.Width != wantWidth {
				t.Fatalf("%s %s: width %d, want %d", viewport.Name, scheme, img.Width, wantWidth)
			}
			luma := centerLuma(t, data)
			if scheme == "light" && luma < 200 || scheme == "dark" && luma > 50 {
				t.Fatalf("%s %s: center luma %d", viewport.Name, scheme, luma)
			}
		}
	}
	spectest.Proves(t, screensFeature, capturesReq, "one-page-gets-eight-captures")
}

// TestAnimationsAndTransitionsAreFrozenBeforeCapture captures a page whose
// background fades from white to black every second, at several moments: each
// capture shows the first frame of the animation.
func TestAnimationsAndTransitionsAreFrozenBeforeCapture(t *testing.T) {
	browser := testBrowser(t, nil)
	server := colorPage(t)
	for i := range 3 {
		time.Sleep(time.Duration(i) * 330 * time.Millisecond)
		got, err := browser.Capture(context.Background(), Shot{URL: server.URL + "/animated", Viewport: Viewports[0], Scheme: "light"})
		if err != nil {
			t.Fatal(err)
		}
		if luma := centerLuma(t, got.PNG); luma < 250 {
			t.Fatalf("capture %d shows a frame of luma %d, not the first frame of the animation", i, luma)
		}
	}
	// An entrance, ended or still running, shows the content it brings in,
	// and the freeze runs no handler of its end, here a navigation.
	got, err := browser.Capture(context.Background(), Shot{URL: server.URL + "/entering", Viewport: Viewports[0], Scheme: "light"})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(got.PNG))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	for _, x := range []int{b.Dx() / 4, 3 * b.Dx() / 4} {
		r, g, bl, _ := img.At(x, b.Dy()/2).RGBA()
		if luma := (r + g + bl) / 3 >> 8; luma > 5 {
			t.Fatalf("the entrance at x=%d shows luma %d, not its end", x, luma)
		}
	}
	spectest.Proves(t, screensFeature, frozenMotion, "animations-and-transitions-are-frozen-before-capture")
}

// TestTheBrowserReportsTheDocumentStatus reads a 404 as the status of its
// document.
func TestTheBrowserReportsTheDocumentStatus(t *testing.T) {
	browser := testBrowser(t, nil)
	server := colorPage(t)
	got, err := browser.Capture(context.Background(), Shot{URL: server.URL + "/missing", Viewport: Viewports[0], Scheme: "light"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != http.StatusNotFound {
		t.Fatalf("status %d, want 404", got.Status)
	}
	got, err = browser.Capture(context.Background(), Shot{URL: server.URL + "/elsewhere", Viewport: Viewports[0], Scheme: "light"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != http.StatusOK || got.URL != server.URL+"/" {
		t.Fatalf("a redirect captured status %d at %s", got.Status, got.URL)
	}
}

// TestTheStorageStateSignsTheBrowserIn launches the browser with a storage
// state and reads the cookie and the localStorage token the page received.
func TestTheStorageStateSignsTheBrowserIn(t *testing.T) {
	server := colorPage(t)
	state := &StorageState{
		Cookies: []StateCookie{{Name: "session", Value: "abc", Domain: "127.0.0.1", Path: "/", Expires: -1, SameSite: "Lax"}},
		Origins: []StateOrigin{{Origin: "http://127.0.0.1:1", LocalStorage: []StateItem{{Name: "token", Value: "xyz"}}}},
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "state.json")
	data, _ := json.Marshal(state)
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := ReadStorageState(file)
	if err != nil {
		t.Fatal(err)
	}
	browser := testBrowser(t, read)
	ctx := context.Background()
	if _, err := browser.Capture(ctx, Shot{URL: server.URL + "/whoami", Viewport: Viewports[0], Scheme: "light"}); err != nil {
		t.Fatal(err)
	}
	// A capture leaves no handle on the page, so read what the page saw in a
	// tab of its own.
	got := readBodyData(t, browser, server.URL+"/whoami")
	if got["cookie"] != "abc" || got["token"] != "xyz" {
		t.Fatalf("the page saw %v", got)
	}
	spectest.Proves(t, screensFeature, capturesReq, "the-storage-state-signs-the-browser-in")
}

// readBodyData opens url in a new tab with the storage script and returns
// the body's data attributes.
func readBodyData(t *testing.T, b *Browser, url string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := b.client.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		t.Fatal(err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.client.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
		t.Fatal(err)
	}
	s := attached.SessionID
	_ = b.client.call(ctx, s, "Page.enable", nil, nil)
	_ = b.client.call(ctx, s, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}, nil)
	_ = b.client.call(ctx, s, "Network.enable", nil, nil)
	if script := b.state.storageScript(url); script != "" {
		_ = b.client.call(ctx, s, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": script}, nil)
	}
	nav := newNavigation(s)
	remove := b.client.listen(nav.observe)
	defer remove()
	var navigated struct {
		LoaderID string `json:"loaderId"`
	}
	if err := b.client.call(ctx, s, "Page.navigate", map[string]any{"url": url}, &navigated); err != nil {
		t.Fatal(err)
	}
	if _, err := nav.wait(ctx, navigated.LoaderID, time.Second); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Result struct {
			Value map[string]string `json:"value"`
		} `json:"result"`
	}
	if err := b.client.call(ctx, s, "Runtime.evaluate", map[string]any{"expression": "({...document.body.dataset})", "returnByValue": true}, &result); err != nil {
		t.Fatal(err)
	}
	return result.Result.Value
}

// scriptedBrowser answers the DevTools calls a capture makes on the far end
// of two pipes, without a browser: it creates one target and session, emits
// the document response and the load events after Page.navigate, and returns
// a one-pixel PNG. It records each call.
type scriptedBrowser struct {
	mu    sync.Mutex
	calls []message
}

func (s *scriptedBrowser) serve(t *testing.T, in io.Reader, out io.Writer) {
	read := newFrameReader(in)
	write := func(v any) {
		data, _ := json.Marshal(v)
		_, _ = out.Write(append(data, 0))
	}
	var pixel bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	_ = png.Encode(&pixel, img)
	for {
		data, err := read()
		if err != nil {
			return
		}
		var msg message
		_ = json.Unmarshal(data, &msg)
		s.mu.Lock()
		s.calls = append(s.calls, msg)
		s.mu.Unlock()
		result := map[string]any{}
		switch msg.Method {
		case "Target.createTarget":
			result["targetId"] = "T"
		case "Target.attachToTarget":
			result["sessionId"] = "S"
		case "Page.navigate":
			result["frameId"], result["loaderId"] = "F", "L"
			write(map[string]any{"id": msg.ID, "sessionId": msg.SessionID, "result": result})
			write(map[string]any{"sessionId": "S", "method": "Network.responseReceived",
				"params": map[string]any{"loaderId": "L", "type": "Document", "response": map[string]any{"status": 200}}})
			for _, name := range []string{"load", "networkIdle"} {
				write(map[string]any{"sessionId": "S", "method": "Page.lifecycleEvent", "params": map[string]any{"loaderId": "L", "name": name}})
			}
			continue
		case "Runtime.evaluate":
			result["result"] = map[string]any{"value": "http://127.0.0.1:4000/whoami"}
		case "Page.getLayoutMetrics":
			result["cssContentSize"] = map[string]any{"width": 1440, "height": 900}
		case "Page.captureScreenshot":
			result["data"] = base64.StdEncoding.EncodeToString(pixel.Bytes())
		}
		write(map[string]any{"id": msg.ID, "sessionId": msg.SessionID, "result": result})
	}
}

func (s *scriptedBrowser) params(method string) []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var params []json.RawMessage
	for _, call := range s.calls {
		if call.Method == method {
			params = append(params, call.Params)
		}
	}
	return params
}

// TestTheStorageStateReachesTheBrowserWithoutChromium drives a capture
// against a scripted DevTools peer: the storage state's cookies go to
// Storage.setCookies, and its localStorage to a script that runs before the
// page's own, on the host of the shot whatever its port.
func TestTheStorageStateReachesTheBrowserWithoutChromium(t *testing.T) {
	t.Parallel()
	toPeerR, toPeerW := io.Pipe()
	fromPeerR, fromPeerW := io.Pipe()
	peer := &scriptedBrowser{}
	go peer.serve(t, toPeerR, fromPeerW)
	defer func() { _ = toPeerW.Close(); _ = fromPeerW.Close() }()
	state := &StorageState{
		Cookies: []StateCookie{{Name: "session", Value: "abc", Domain: "127.0.0.1", Expires: -1, SameSite: "Lax", HTTPOnly: true}},
		Origins: []StateOrigin{{Origin: "http://127.0.0.1:3000", LocalStorage: []StateItem{{Name: "token", Value: "xyz"}}}},
	}
	b := &Browser{client: newClient(fromPeerR, toPeerW), state: state, Timeout: 10 * time.Second}
	ctx := context.Background()
	if err := b.setCookies(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := b.Capture(ctx, Shot{URL: "http://127.0.0.1:4000/whoami", Viewport: Viewports[1], Scheme: "dark"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != 200 || got.URL != "http://127.0.0.1:4000/whoami" || len(got.PNG) == 0 {
		t.Fatalf("captured %+v", got)
	}
	cookies := peer.params("Storage.setCookies")
	if len(cookies) != 1 || !strings.Contains(string(cookies[0]), `"name":"session"`) || !strings.Contains(string(cookies[0]), `"path":"/"`) ||
		strings.Contains(string(cookies[0]), "expires") || !strings.Contains(string(cookies[0]), `"sameSite":"Lax"`) {
		t.Fatalf("Storage.setCookies %s", cookies)
	}
	scripts := peer.params("Page.addScriptToEvaluateOnNewDocument")
	if len(scripts) != 1 || !strings.Contains(string(scripts[0]), `\"token\":\"xyz\"`) {
		t.Fatalf("init scripts %s", scripts)
	}
	var frozen bool
	for _, params := range peer.params("Runtime.evaluate") {
		frozen = frozen || strings.Contains(string(params), "getAnimations") && strings.Contains(string(params), "transition: none")
	}
	if !frozen {
		t.Fatal("no evaluation froze the animations and transitions before the capture")
	}
	media := peer.params("Emulation.setEmulatedMedia")
	metrics := peer.params("Emulation.setDeviceMetricsOverride")
	if len(media) != 1 || !strings.Contains(string(media[0]), `"value":"dark"`) || len(metrics) != 1 || !strings.Contains(string(metrics[0]), `"mobile":true`) {
		t.Fatalf("emulation %s %s", media, metrics)
	}
	spectest.Proves(t, screensFeature, capturesReq, "the-storage-state-signs-the-browser-in")
}
