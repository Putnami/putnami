package deliverycli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

const (
	packagesRequirement = "packages-go-in-parallel"
	packageToken        = "run-secret"
	packageSession      = "20261127-130000-abc123"
	// packageWait bounds every wait of these tests on the reporter or the
	// receiver; none is reached when the reporter behaves.
	packageWait = 10 * time.Second
	// packageQuiet is how long a test watches for a request that must not
	// leave yet.
	packageQuiet = 200 * time.Millisecond
)

// packageReceiver is a Delivery session receiver of ADR 0135 section 4 on the
// hosted route /ingest/sessions (or on route): every answer names contract
// and the ADR 0131 cap when contract is set, the package routes when packages
// is set, and reads gzip. A frame POST is stored as ADR 0131's receiver does;
// a POST of <route>/packages is a bundle, 413 when its decoded bytes, its
// lines included, pass 8 MiB, else parsed (protocolcli.ParseSessionReportingBundle)
// and its packages stored when each one's SHA-256 is its address, 400
// otherwise; a manifest POST answers 409 with the missing addresses, or 200.
type packageReceiver struct {
	token    string
	contract string
	// packages makes every answer advertise the package routes.
	packages bool
	// delay holds every answer this long.
	delay time.Duration
	// route is the session route; "" is the hosted /ingest/sessions.
	route string
	// hold makes every bundle wait until its put's address is released.
	hold bool
	// frameStatuses answers the next frame POSTs with these statuses.
	frameStatuses []int
	// putStatus answers the bundle of a put's address with this status,
	// unstored.
	putStatus map[string]int
	// arrived receives the put's address of every bundle as it reaches the
	// receiver: the SHA-256 of its packages' addresses, each followed by LF.
	arrived chan string

	mu          sync.Mutex
	gates       map[string]chan struct{}
	stored      map[string][]byte
	encodings   []string
	bundles     map[string][]string
	kindHeaders int
	malformed   int
	tooLarge    int
	inFlight    int
	maxInFlight int
	answered    int
	frames      int
	manifests   []seenManifest
}

// seenManifest is one manifest the receiver got, and the puts in flight and
// answered when it arrived.
type seenManifest struct {
	addresses    []string
	final        bool
	putsInFlight int
	putsAnswered int
}

// receiverState is what a packageReceiver received.
type receiverState struct {
	// stored is every package stored, by address.
	stored map[string][]byte
	// encodings is the Content-Encoding of every bundle.
	encodings []string
	// bundles is the package addresses of every bundle, by its put's address.
	bundles map[string][]string
	// kindHeaders counts the bundles that carried a package kind header.
	kindHeaders int
	malformed   int
	// tooLarge counts the bundles refused 413.
	tooLarge    int
	maxInFlight int
	frames      int
	manifests   []seenManifest
}

func newPackageReceiver(t *testing.T, receiver *packageReceiver) string {
	t.Helper()
	receiver.arrived = make(chan string, 64)
	receiver.gates = map[string]chan struct{}{}
	receiver.stored = map[string][]byte{}
	receiver.bundles = map[string][]string{}
	if receiver.route == "" {
		receiver.route = "/ingest/sessions"
	}
	srv := httptest.NewServer(receiver)
	t.Cleanup(func() {
		receiver.releaseAll()
		srv.Close()
	})
	return srv.URL
}

func (r *packageReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	time.Sleep(r.delay)
	w.Header().Set(acceptEncodingHeader, gzipCoding)
	if r.contract != "" {
		w.Header().Set(sessionContractHeader, r.contract)
		w.Header().Set(sessionChunkBytesHeader, strconv.Itoa(protocolcli.SessionReportingMaxChunkBytes))
	}
	if r.packages {
		w.Header().Set("X-Putnami-Session-Packages", "1")
	}
	if req.Header.Get("Authorization") != "Bearer "+r.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := req.URL.EscapedPath()
	switch {
	case req.Method == http.MethodPost && path == r.route:
		r.frame(w, req)
	case req.Method == http.MethodPost && path == r.route+sessionPackagesRoute:
		r.bundle(w, req)
	case req.Method == http.MethodPost && path == r.route+sessionManifestRoute:
		r.manifest(w, req)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (r *packageReceiver) frame(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames++
	if len(r.frameStatuses) > 0 {
		status := r.frameStatuses[0]
		r.frameStatuses = r.frameStatuses[1:]
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
	}
	seq, _ := strconv.Atoi(req.Header.Get(sessionSeqHeader))
	_ = json.NewEncoder(w).Encode(map[string]any{"part": req.Header.Get(sessionPartHeader), "seq": seq})
}

// bundle stores the packages of one bundle once its put is released.
func (r *packageReceiver) bundle(w http.ResponseWriter, req *http.Request) {
	body, ok := receivedChunkBody(req, true)
	tooLarge := ok && len(body) > protocolcli.SessionReportingMaxPackageBytes
	var packages []protocolcli.SessionReportingPackage
	var err error
	if ok && !tooLarge {
		packages, err = protocolcli.ParseSessionReportingBundle(body)
	}
	r.mu.Lock()
	if req.Header.Get("X-Putnami-Session-Package-Kind") != "" {
		r.kindHeaders++
	}
	if tooLarge {
		r.tooLarge++
		r.mu.Unlock()
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if !ok || err != nil {
		r.malformed++
		r.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	addresses := make([]string, 0, len(packages))
	for _, p := range packages {
		addresses = append(addresses, p.Address)
	}
	put := putAddress(addresses)
	r.encodings = append(r.encodings, req.Header.Get(contentEncodingHeader))
	r.bundles[put] = addresses
	r.inFlight++
	r.maxInFlight = max(r.maxInFlight, r.inFlight)
	gate := r.gate(put)
	r.mu.Unlock()
	r.arrived <- put
	if r.hold {
		select {
		case <-gate:
		case <-time.After(packageWait):
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight--
	r.answered++
	if status := r.putStatus[put]; status != 0 {
		w.WriteHeader(status)
		return
	}
	for _, p := range packages {
		r.stored[p.Address] = append([]byte(nil), p.Data...)
	}
	w.WriteHeader(http.StatusOK)
}

// putAddress is the address of a put of packages addressed so: the SHA-256 of
// the addresses, each followed by LF.
func putAddress(addresses []string) string {
	sum := sha256.Sum256([]byte(strings.Join(addresses, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

func (r *packageReceiver) manifest(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Addresses []string `json:"addresses"`
		Final     bool     `json:"final"`
	}
	d := json.NewDecoder(req.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&body); err != nil || req.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests = append(r.manifests, seenManifest{addresses: body.Addresses, final: body.Final, putsInFlight: r.inFlight, putsAnswered: r.answered})
	missing := []string{}
	for _, address := range body.Addresses {
		if _, held := r.stored[address]; !held {
			missing = append(missing, address)
		}
	}
	if len(missing) > 0 {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"missing": missing})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// gate is the channel that releases the put of address. r.mu is held.
func (r *packageReceiver) gate(address string) chan struct{} {
	if r.gates[address] == nil {
		r.gates[address] = make(chan struct{})
	}
	return r.gates[address]
}

// release lets the held put of address answer.
func (r *packageReceiver) release(address string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	gate := r.gate(address)
	select {
	case <-gate:
	default:
		close(gate)
	}
}

func (r *packageReceiver) releaseAll() {
	r.mu.Lock()
	addresses := make([]string, 0, len(r.gates))
	for address := range r.gates {
		addresses = append(addresses, address)
	}
	r.mu.Unlock()
	for _, address := range addresses {
		r.release(address)
	}
}

// awaitArrivals waits for n puts to reach the receiver and returns their
// addresses in arrival order.
func (r *packageReceiver) awaitArrivals(t *testing.T, n int) []string {
	t.Helper()
	addresses := make([]string, 0, n)
	for len(addresses) < n {
		select {
		case address := <-r.arrived:
			addresses = append(addresses, address)
		case <-time.After(packageWait):
			t.Fatalf("%d puts reached the receiver, want %d", len(addresses), n)
		}
	}
	return addresses
}

// requireNoArrival fails when a put reaches the receiver within packageQuiet.
func (r *packageReceiver) requireNoArrival(t *testing.T) {
	t.Helper()
	select {
	case address := <-r.arrived:
		t.Fatalf("put %s reached the receiver, want none yet", address)
	case <-time.After(packageQuiet):
	}
}

func (r *packageReceiver) state() receiverState {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := receiverState{stored: map[string][]byte{}, bundles: map[string][]string{}, encodings: slices.Clone(r.encodings), kindHeaders: r.kindHeaders,
		malformed: r.malformed, tooLarge: r.tooLarge, maxInFlight: r.maxInFlight, frames: r.frames, manifests: slices.Clone(r.manifests)}
	for address, body := range r.stored {
		state.stored[address] = body
	}
	for put, addresses := range r.bundles {
		state.bundles[put] = addresses
	}
	return state
}

// liveReporter runs the session reporter on pipes, as the engine does: the
// test writes lines while puts are in flight and reads each answer as the
// reporter writes it.
type liveReporter struct {
	queue chan string
	lines chan []byte
	done  chan error
}

func startLiveReporter(t *testing.T, cfg sessionReporterConfig) *liveReporter {
	t.Helper()
	return startLiveReporterWith(t, cfg, &http.Client{Timeout: sessionReporterRequestTimeout})
}

// startLiveReporterWith is startLiveReporter on client.
func startLiveReporterWith(t *testing.T, cfg sessionReporterConfig, client *http.Client) *liveReporter {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	live := &liveReporter{queue: make(chan string, 64), lines: make(chan []byte, 64), done: make(chan error, 1)}
	go func() {
		for line := range live.queue {
			if _, err := io.WriteString(inW, line); err != nil {
				break
			}
		}
		_ = inW.Close()
	}()
	go func() {
		err := runSessionReporter(context.Background(), inR, outW, io.Discard, cfg, client)
		_ = outW.Close()
		_ = inR.Close()
		live.done <- err
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			live.lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(live.lines)
	}()
	return live
}

func (l *liveReporter) send(lines ...string) {
	for _, line := range lines {
		l.queue <- line
	}
}

// next is the reporter's next answer line.
func (l *liveReporter) next(t *testing.T) []byte {
	t.Helper()
	select {
	case line, ok := <-l.lines:
		if !ok {
			t.Fatal("the reporter closed its output, want another answer")
		}
		return line
	case <-time.After(packageWait):
		t.Fatal("no answer from the reporter")
	}
	return nil
}

// requireQuiet fails when the reporter answers within packageQuiet.
func (l *liveReporter) requireQuiet(t *testing.T) {
	t.Helper()
	select {
	case line := <-l.lines:
		t.Fatalf("the reporter answered %s, want no answer yet", line)
	case <-time.After(packageQuiet):
	}
}

// close ends stdin, as the engine does, and returns the reporter's error once
// it wrote every remaining answer.
func (l *liveReporter) close(t *testing.T) error {
	t.Helper()
	close(l.queue)
	for range l.lines {
		t.Error("the reporter wrote an answer nobody asked for")
	}
	select {
	case err := <-l.done:
		return err
	case <-time.After(packageWait):
		t.Fatal("the reporter did not stop after stdin closed")
	}
	return nil
}

func (l *liveReporter) ack(t *testing.T) protocolcli.SessionReportingAck {
	t.Helper()
	line := l.next(t)
	ack, err := protocolcli.ParseSessionReportingAck(line)
	if err != nil {
		t.Fatalf("ack %s: %v", line, err)
	}
	return *ack
}

func (l *liveReporter) result(t *testing.T) protocolcli.SessionReportingResult {
	t.Helper()
	line := l.next(t)
	result, err := protocolcli.ParseSessionReportingResult(line)
	if err != nil {
		t.Fatalf("result %s: %v", line, err)
	}
	return *result
}

// taskPackage is the package of n compressible records of the task key.
func taskPackage(key string, n int) []byte {
	records := eventLines(n)
	width := int64(len(records) / n)
	offsets := make([]int64, n)
	for i := range offsets {
		offsets[i] = int64(i) * width
	}
	return protocolcli.EncodeSessionReportingPackage(protocolcli.SessionReportingPackageHeader{Kind: protocolcli.SessionReportingPackageTask, Key: &key, Offsets: offsets}, []byte(records))
}

// putLine is the engine's put line of a batch of packages and the put's
// address.
func putLine(t *testing.T, session string, packages ...protocolcli.SessionReportingPackage) (string, string) {
	t.Helper()
	put := protocolcli.NewSessionReportingPut(session, packages)
	if err := put.Validate(); err != nil {
		t.Fatalf("fixture put: %v", err)
	}
	encoded, err := json.Marshal(put)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n", put.Address
}

// taskPuts is the lines of n puts, each a batch of per distinct task
// packages, the puts' addresses, and the batch of each put.
func taskPuts(t *testing.T, session string, n, per int) (lines, puts []string, batches [][]protocolcli.SessionReportingPackage) {
	t.Helper()
	for i := 0; i < n; i++ {
		batch := make([]protocolcli.SessionReportingPackage, 0, per)
		for j := 0; j < per; j++ {
			batch = append(batch, protocolcli.NewSessionReportingPackage(protocolcli.SessionReportingPackageTask, taskPackage("/a:test~test-"+strconv.Itoa(i)+"-"+strconv.Itoa(j), 40)))
		}
		line, put := putLine(t, session, batch...)
		lines, puts, batches = append(lines, line), append(puts, put), append(batches, batch)
	}
	return lines, puts, batches
}

// packageAddresses is the addresses of every package of batches, in order.
func packageAddresses(batches ...[]protocolcli.SessionReportingPackage) []string {
	var addresses []string
	for _, batch := range batches {
		for _, p := range batch {
			addresses = append(addresses, p.Address)
		}
	}
	return addresses
}

// onePackage is one task package of key, alone in its batch.
func onePackage(key string) protocolcli.SessionReportingPackage {
	return protocolcli.NewSessionReportingPackage(protocolcli.SessionReportingPackageTask, taskPackage(key, 2))
}

func manifestLine(t *testing.T, session string, addresses []string) (string, protocolcli.SessionReportingManifest) {
	t.Helper()
	manifest := protocolcli.NewSessionReportingManifest(session, addresses)
	if err := manifest.Validate(); err != nil {
		t.Fatalf("fixture manifest: %v", err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n", manifest
}

func packageConfig(url string) sessionReporterConfig {
	return sessionReporterConfig{ingestURL: url + "/ingest", token: packageToken, framing: true, packages: true}
}

func requireResult(t *testing.T, result protocolcli.SessionReportingResult, op, address string, ok bool, code string, retryable bool) {
	t.Helper()
	if result.Op != op || result.Address != address || result.SessionID != packageSession || result.OK != ok || result.Code != code || result.Retryable != retryable {
		t.Fatalf("result = %+v, want op %s address %s ok=%v code=%q retryable=%v", result, op, address, ok, code, retryable)
	}
}

// The engine names framing version 4 in the environment and the receiver's
// latest decisive answer carries X-Putnami-Session-Packages: 1: every
// acknowledgement, a refusal included, offers packages. An engine of version
// 3, a receiver without the header, or an answer a gateway may have given
// (503) offers nothing. The packages header and the contract header of ADR
// 0131 are read apart: contract 2 alone gives the cap and no offer, packages
// alone the offer and no large frames, and contract 3 neither.
func TestAcksOfferPackagesOnlyToAPackageEngineAndReceiver(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "acks-offer-packages-only-to-a-package-engine-and-receiver")
	stubReporterDenyInspection(t, func() error { return nil })
	const largeCap, smallCap = protocolcli.SessionReportingMaxChunkBytes, protocolcli.SessionReportingChunkBytes
	for _, tc := range []struct {
		name     string
		framing  string
		contract string
		packages bool
		statuses []int
		want     []bool
		// chunkBytes is the cap the first acknowledgement names.
		chunkBytes int
	}{
		{"engine 4, contract 2 and packages 1", "4", "2", true, nil, []bool{true, true}, largeCap},
		{"engine 3", "3", "2", true, nil, []bool{false, false}, largeCap},
		{"no framing named", "", "2", true, nil, []bool{false, false}, 0},
		{"contract 2 alone", "4", "2", false, nil, []bool{false, false}, largeCap},
		{"packages 1 alone", "4", "", true, nil, []bool{true, true}, smallCap},
		{"contract 3 alone", "4", "3", false, nil, []bool{false, false}, smallCap},
		{"neither", "4", "", false, nil, []bool{false, false}, smallCap},
		{"a gateway answer first", "4", "2", true, []int{http.StatusServiceUnavailable, http.StatusOK}, []bool{false, true}, smallCap},
		{"a refusal after a decisive answer", "4", "2", true, []int{http.StatusOK, http.StatusServiceUnavailable}, []bool{true, true}, largeCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := newPackageReceiver(t, &packageReceiver{token: packageToken, contract: tc.contract, packages: tc.packages, frameStatuses: tc.statuses})
			env := map[string]string{
				SessionReporterIngestURLEnv:            url + "/ingest",
				protocolcli.SessionReporterTokenEnv:    packageToken,
				protocolcli.SessionReportingFramingEnv: tc.framing,
			}
			attempt := frame(t, packageSession, "plan.json", 0, 0, "{}", false)
			var out strings.Builder
			if err := serveSessionReporter(env, nil, strings.NewReader(attempt+attempt), &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if len(lines) != len(tc.want) {
				t.Fatalf("acks %q, want %d", lines, len(tc.want))
			}
			for i, line := range lines {
				ack, err := protocolcli.ParseSessionReportingAck([]byte(line))
				if err != nil {
					t.Fatalf("ack %s: %v", line, err)
				}
				if ack.Packages != tc.want[i] {
					t.Fatalf("ack %d = %+v, want packages %v", i, ack, tc.want[i])
				}
				if ok := i >= len(tc.statuses) || tc.statuses[i] == http.StatusOK; ack.OK != ok {
					t.Fatalf("ack %d = %+v, want ok %v", i, ack, ok)
				}
				if i == 0 && ack.ChunkBytes != tc.chunkBytes {
					t.Fatalf("ack %d = %+v, want chunkBytes %d", i, ack, tc.chunkBytes)
				}
			}
		})
	}

	// A receiver that advertises packages alone takes no version 3 frame.
	receiver := &packageReceiver{token: packageToken, packages: true}
	cfg := packageConfig(newPackageReceiver(t, receiver))
	acks := runReporter(t, cfg,
		frame(t, packageSession, "plan.json", 0, 0, "{}", false),
		frame(t, packageSession, "session.json", 0, 0, large(100<<10), false),
	)
	requireLargeAck(t, acks[1], false, ackCodeFrameUnsupported, false, smallCap)
	if !acks[0].Packages || !acks[1].Packages || receiver.state().frames != 1 {
		t.Fatalf("acks %+v, frames %d: want both offering packages and only the plan frame posted", acks, receiver.state().frames)
	}

	for value, want := range map[string]bool{"4": true, "\t4\n": true, "5": true, "3": false, "": false, "four": false} {
		if got := packagesRequested(map[string]string{protocolcli.SessionReportingFramingEnv: value}); got != want {
			t.Errorf("packagesRequested(%q) = %v, want %v", value, got, want)
		}
	}
	for value, want := range map[string]bool{"1": true, "\t1\n": true, "2": false, "": false, "11": false, "true": false} {
		header := http.Header{}
		header.Set("X-Putnami-Session-Packages", value)
		if got := advertisesPackages(header); got != want {
			t.Errorf("advertisesPackages(%q) = %v, want %v", value, got, want)
		}
	}
	for contract, want := range map[string]int{"2": 4 << 20, "3": 0} {
		header := http.Header{}
		header.Set(sessionContractHeader, contract)
		header.Set(sessionChunkBytesHeader, "4194304")
		header.Set("X-Putnami-Session-Packages", "1")
		if got := advertisedChunkBytes(header); got != want {
			t.Errorf("advertisedChunkBytes(contract %s) = %d, want %d", contract, got, want)
		}
	}
	header := http.Header{}
	header.Set(sessionContractHeader, "3")
	if advertisesPackages(header) {
		t.Error("contract 3 advertises packages, want only X-Putnami-Session-Packages to")
	}
}

// Four puts of three packages each are in flight at once against a receiver
// that holds them, before any is answered: each is one bundle POST of its
// batch, compressed once the receiver advertised gzip, with no package kind
// header. Released out of order, each result names its put's address as it
// completes, and a 503 is a retryable receiver_unavailable.
func TestPutsGoConcurrentlyAndAnswerByAddress(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true, hold: true}
	url := newPackageReceiver(t, receiver)
	lines, puts, batches := taskPuts(t, packageSession, 4, 3)
	receiver.putStatus = map[string]int{puts[2]: http.StatusServiceUnavailable}
	live := startLiveReporter(t, packageConfig(url))
	live.send(frame(t, packageSession, "plan.json", 0, 0, "{}", false))
	if ack := live.ack(t); !ack.OK || !ack.Packages {
		t.Fatalf("plan ack = %+v, want accepted with packages", ack)
	}
	live.send(lines...)
	arrived := receiver.awaitArrivals(t, 4)
	live.requireQuiet(t)
	for _, k := range []int{3, 1, 2, 0} {
		receiver.release(puts[k])
		result := live.result(t)
		if k == 2 {
			requireResult(t, result, protocolcli.SessionReportingOpPut, puts[k], false, ackCodeReceiverUnavailable, true)
			continue
		}
		requireResult(t, result, protocolcli.SessionReportingOpPut, puts[k], true, "", false)
	}
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}
	state := receiver.state()
	if state.maxInFlight != 4 || len(arrived) != 4 || len(state.bundles) != 4 || state.malformed != 0 || state.kindHeaders != 0 {
		t.Fatalf("max in flight %d, bundles %d, malformed %d, kind headers %d: want four bundles in flight at once, well formed, without a kind header",
			state.maxInFlight, len(state.bundles), state.malformed, state.kindHeaders)
	}
	for k, put := range puts {
		if !slices.Equal(state.bundles[put], packageAddresses(batches[k])) {
			t.Fatalf("bundle of put %d holds %v, want its batch %v", k, state.bundles[put], packageAddresses(batches[k]))
		}
		for _, p := range batches[k] {
			if stored := state.stored[p.Address]; (k == 2) != (stored == nil) || k != 2 && string(stored) != string(p.Data) {
				t.Fatalf("package of put %d stored %d bytes, want its %d bytes stored unless its put was refused", k, len(stored), len(p.Data))
			}
		}
	}
	if strings.Join(state.encodings, ",") != "gzip,gzip,gzip,gzip" {
		t.Fatalf("encodings %q: want every bundle gzip", state.encodings)
	}
}

// At most eight puts are in flight: the ninth leaves once one completes.
func TestPutsInFlightAreBoundedAtEight(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true, hold: true}
	live := startLiveReporter(t, packageConfig(newPackageReceiver(t, receiver)))
	lines, puts, _ := taskPuts(t, packageSession, 10, 1)
	live.send(lines...)
	first := receiver.awaitArrivals(t, sessionReporterPutsInFlight)
	receiver.requireNoArrival(t)
	receiver.release(first[0])
	requireResult(t, live.result(t), protocolcli.SessionReportingOpPut, first[0], true, "", false)
	receiver.awaitArrivals(t, 1)
	receiver.requireNoArrival(t)
	for _, put := range puts {
		receiver.release(put)
	}
	answered := map[string]bool{first[0]: true}
	for range puts[1:] {
		result := live.result(t)
		if !result.OK || answered[result.Address] || !slices.Contains(puts, result.Address) {
			t.Fatalf("result = %+v, want one acceptance per put", result)
		}
		answered[result.Address] = true
	}
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}
	if state := receiver.state(); state.maxInFlight != sessionReporterPutsInFlight {
		t.Fatalf("max puts in flight %d, want %d", state.maxInFlight, sessionReporterPutsInFlight)
	}
}

// Every status of a put maps onto one refusal, as a frame's does, and a put
// that gets no answer is a retryable receiver_unavailable.
func TestAPutAnswerMapsOntoItsResult(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	statuses := []struct {
		status    int
		code      string
		retryable bool
	}{
		{http.StatusCreated, "", false},
		{http.StatusNoContent, "", false},
		{http.StatusBadRequest, ackCodeInvalidChunk, false},
		{http.StatusUnauthorized, ackCodeUnauthorized, false},
		{http.StatusForbidden, ackCodeUnauthorized, false},
		{http.StatusNotFound, ackCodeIngestUnavailable, false},
		{http.StatusMethodNotAllowed, ackCodeInvalidChunk, false},
		{http.StatusRequestTimeout, ackCodeReceiverUnavailable, true},
		{http.StatusRequestEntityTooLarge, ackCodeChunkTooLarge, false},
		{http.StatusTooManyRequests, ackCodeReceiverUnavailable, true},
		{http.StatusInternalServerError, ackCodeReceiverUnavailable, true},
		{http.StatusBadGateway, ackCodeReceiverUnavailable, true},
	}
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true, putStatus: map[string]int{}}
	live := startLiveReporter(t, packageConfig(newPackageReceiver(t, receiver)))
	lines, puts, _ := taskPuts(t, packageSession, len(statuses), 2)
	want := map[string]int{}
	for i, put := range puts {
		receiver.putStatus[put] = statuses[i].status
		want[put] = i
	}
	live.send(lines...)
	for range puts {
		result := live.result(t)
		i, known := want[result.Address]
		if !known {
			t.Fatalf("result %+v names no put", result)
		}
		delete(want, result.Address)
		requireResult(t, result, protocolcli.SessionReportingOpPut, result.Address, statuses[i].code == "", statuses[i].code, statuses[i].retryable)
	}
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unanswered := startLiveReporter(t, packageConfig(closed.URL))
	line, put := putLine(t, packageSession, protocolcli.NewSessionReportingPackage(protocolcli.SessionReportingPackagePlan, []byte("{}")))
	unanswered.send(line)
	requireResult(t, unanswered.result(t), protocolcli.SessionReportingOpPut, put, false, ackCodeReceiverUnavailable, true)
	if err := unanswered.close(t); err != nil {
		t.Fatal(err)
	}
}

// bundleOf is a session package whose bundle, its line included, is n
// decoded bytes.
func bundleOf(t *testing.T, n int) protocolcli.SessionReportingPackage {
	t.Helper()
	for size := n; ; {
		p := protocolcli.NewSessionReportingPackage(protocolcli.SessionReportingPackageSession, bytes.Repeat([]byte("s"), size))
		got := len(protocolcli.EncodeSessionReportingBundle([]protocolcli.SessionReportingPackage{p}))
		if got == n {
			return p
		}
		if size += n - got; size <= 0 {
			t.Fatalf("no package has a bundle of %d bytes", n)
		}
	}
}

// The receiver bounds a bundle's decoded bytes, its lines included, at
// 8 MiB, as ADR 0135 states: a put whose bundle is exactly 8 MiB is stored,
// and one a byte larger is refused chunk_too_large without retry.
func TestABundlePastEightMiBIsTooLarge(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true}
	live := startLiveReporter(t, packageConfig(newPackageReceiver(t, receiver)))
	fits, over := bundleOf(t, protocolcli.SessionReportingMaxPackageBytes), bundleOf(t, protocolcli.SessionReportingMaxPackageBytes+1)
	fitsLine, fitsPut := putLine(t, packageSession, fits)
	overLine, overPut := putLine(t, packageSession, over)
	live.send(fitsLine)
	requireResult(t, live.result(t), protocolcli.SessionReportingOpPut, fitsPut, true, "", false)
	live.send(overLine)
	requireResult(t, live.result(t), protocolcli.SessionReportingOpPut, overPut, false, ackCodeChunkTooLarge, false)
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}
	if state := receiver.state(); state.tooLarge != 1 || len(state.stored) != 1 || state.stored[fits.Address] == nil {
		t.Fatalf("too large %d, stored %d: want the 8 MiB bundle stored and the larger one refused", state.tooLarge, len(state.stored))
	}
}

// A put's deadline is 4 s plus 2 s per MiB of its decoded bundle, a part of a
// MiB counting whole.
func TestAPutDeadlineGrowsWithItsBundle(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	for n, want := range map[int]time.Duration{
		1: 6 * time.Second, 100 << 10: 6 * time.Second, 1 << 20: 6 * time.Second, 1<<20 + 1: 8 * time.Second,
		3 << 20: 10 * time.Second, 8 << 20: 20 * time.Second, 8<<20 + 1: 22 * time.Second,
	} {
		if got := putDeadline(n); got != want {
			t.Errorf("putDeadline(%d) = %v, want %v", n, got, want)
		}
	}
}

// A put is bounded by its own deadline, not by the client's timeout, which a
// frame and the manifest keep; a put past its deadline is a retryable
// receiver_unavailable.
func TestAPutIsBoundedByItsDeadline(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true, delay: 300 * time.Millisecond}
	url := newPackageReceiver(t, receiver)
	live := startLiveReporterWith(t, packageConfig(url), &http.Client{Timeout: 100 * time.Millisecond})
	pkg := onePackage("/a:slow")
	put, address := putLine(t, packageSession, pkg)
	manifest, m := manifestLine(t, packageSession, []string{pkg.Address})
	live.send(put)
	requireResult(t, live.result(t), protocolcli.SessionReportingOpPut, address, true, "", false)
	live.send(manifest)
	requireResult(t, live.result(t), protocolcli.SessionReportingOpManifest, m.Address(), false, ackCodeReceiverUnavailable, true)
	live.send(frame(t, packageSession, "session.json", 0, 0, "{}", false))
	if ack := live.ack(t); ack.OK || ack.Code != ackCodeReceiverUnavailable || !ack.Retryable {
		t.Fatalf("frame ack = %+v, want the client's timeout as a retryable receiver_unavailable", ack)
	}
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := runSessionReporter(ctx, strings.NewReader(put), &out, io.Discard, packageConfig(url), &http.Client{}); err != nil {
		t.Fatal(err)
	}
	result, err := protocolcli.ParseSessionReportingResult(bytes.TrimSpace(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	requireResult(t, *result, protocolcli.SessionReportingOpPut, address, false, ackCodeReceiverUnavailable, true)
}

// failingOutput is a stdout the engine stopped reading.
type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A put's result that cannot be written fails the reporter once its puts
// completed, as a frame's acknowledgement does.
func TestAResultThatCannotBeWrittenFailsTheReporter(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true}
	url := newPackageReceiver(t, receiver)
	put, _ := putLine(t, packageSession, onePackage("/a:x"))
	err := runSessionReporter(context.Background(), strings.NewReader(put), failingOutput{}, io.Discard, packageConfig(url), &http.Client{Timeout: sessionReporterRequestTimeout})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("reporter error %v, want the write failure", err)
	}
	if state := receiver.state(); len(state.stored) != 1 {
		t.Fatalf("stored %d packages, want the put sent before its result", len(state.stored))
	}
}

// A put or a manifest of another session than the stream's is refused and
// never sent; the log reporter refuses every package line without a request.
func TestPackageLinesOfAnotherSessionOrReporterAreRefused(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true}
	url := newPackageReceiver(t, receiver)
	const other = "20261127-130000-ffffff"
	pkg := onePackage("/a:x")
	put, address := putLine(t, other, pkg)
	manifest, m := manifestLine(t, other, []string{pkg.Address})
	live := startLiveReporter(t, packageConfig(url))
	live.send(frame(t, packageSession, "plan.json", 0, 0, "{}", false), put, manifest)
	live.ack(t)
	for _, want := range []protocolcli.SessionReportingResult{
		{Op: protocolcli.SessionReportingOpPut, Address: address},
		{Op: protocolcli.SessionReportingOpManifest, Address: m.Address()},
	} {
		result := live.result(t)
		if result.OK || result.Code != ackCodeSessionMismatch || result.Retryable || result.Op != want.Op || result.Address != want.Address || result.SessionID != other {
			t.Fatalf("result = %+v, want a session_mismatch of %s %s", result, want.Op, want.Address)
		}
	}
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}

	put, address = putLine(t, packageSession, pkg)
	manifest, m = manifestLine(t, packageSession, []string{pkg.Address})
	var out strings.Builder
	if err := runLogReporter(context.Background(), strings.NewReader(put+manifest), &out, io.Discard, packageConfig(url), http.DefaultClient); err != nil {
		t.Fatal(err)
	}
	results := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(results) != 2 {
		t.Fatalf("log reporter answered %q, want two results", results)
	}
	for i, wantAddress := range []string{address, m.Address()} {
		result, err := protocolcli.ParseSessionReportingResult([]byte(results[i]))
		if err != nil || result.OK || result.Code != ackCodeInvalidChunk || result.Retryable || result.Address != wantAddress {
			t.Fatalf("log reporter result %d = %+v (%v), want invalid_chunk of %s", i, result, err, wantAddress)
		}
	}
	if state := receiver.state(); len(state.bundles) != 0 || state.frames != 1 || len(state.manifests) != 0 {
		t.Fatalf("bundles %d, frames %d, manifests %d: want only the session reporter's plan frame sent", len(state.bundles), state.frames, len(state.manifests))
	}
}

// A frame after puts waits for every put in flight: the frame path runs
// alone, as without packages.
func TestAFrameWaitsForThePutsInFlight(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true, hold: true}
	live := startLiveReporter(t, packageConfig(newPackageReceiver(t, receiver)))
	lines, puts, _ := taskPuts(t, packageSession, 2, 1)
	live.send(lines...)
	receiver.awaitArrivals(t, 2)
	live.send(frame(t, packageSession, "session.json", 0, 0, "{}", false))
	live.requireQuiet(t)
	if state := receiver.state(); state.frames != 0 {
		t.Fatalf("frames %d reached the receiver while puts were in flight, want 0", state.frames)
	}
	receiver.release(puts[1])
	receiver.release(puts[0])
	for range puts {
		if result := live.result(t); !result.OK {
			t.Fatalf("result = %+v, want accepted", result)
		}
	}
	if ack := live.ack(t); !ack.OK || !ack.Packages {
		t.Fatalf("frame ack = %+v, want accepted with packages", ack)
	}
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}
}

// In the local mode, puts and the manifest go under the session's
// local-sessions route, and concurrent puts refused 401 share one re-mint.
func TestLocalPackagesGoToTheLocalSessionRoute(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "puts-go-concurrently-and-answer-by-address")
	const workspace = "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b"
	receiver := &packageReceiver{token: "fresh", contract: "2", packages: true, route: "/v1/workspaces/" + workspace + localSessionsRoute + packageSession}
	url := newPackageReceiver(t, receiver)
	var mu sync.Mutex
	remints := 0
	session := &clicore.WorkspaceContext{WorkspaceID: workspace, ControlPlane: url, AuthToken: clicore.NewBearer("stale"), RefreshAuth: func() (clicore.Bearer, error) {
		mu.Lock()
		defer mu.Unlock()
		remints++
		return clicore.NewBearer("fresh"), nil
	}}
	cfg := sessionReporterConfig{framing: true, packages: true, local: func() (*clicore.WorkspaceContext, error) { return session, nil }}
	live := startLiveReporter(t, cfg)
	lines, puts, batches := taskPuts(t, packageSession, 4, 2)
	manifest, m := manifestLine(t, packageSession, packageAddresses(batches...))
	live.send(lines...)
	live.send(manifest)
	for range puts {
		if result := live.result(t); !result.OK {
			t.Fatalf("result = %+v, want accepted", result)
		}
	}
	requireResult(t, live.result(t), protocolcli.SessionReportingOpManifest, m.Address(), true, "", false)
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}
	state := receiver.state()
	mu.Lock()
	defer mu.Unlock()
	if len(state.stored) != 8 || len(state.manifests) != 1 || remints != 1 {
		t.Fatalf("stored %d, manifests %d, re-mints %d: want eight packages, one manifest, one re-mint", len(state.stored), len(state.manifests), remints)
	}
}

// The manifest leaves once every put in flight completed, and its result
// follows theirs. A manifest that lists a package the receiver does not hold
// is refused packages_missing without retry.
func TestAManifestPostsLast(t *testing.T) {
	spectest.Proves(t, largeFramesFeature, packagesRequirement, "a-manifest-posts-last")
	receiver := &packageReceiver{token: packageToken, contract: "2", packages: true, hold: true}
	live := startLiveReporter(t, packageConfig(newPackageReceiver(t, receiver)))
	lines, puts, batches := taskPuts(t, packageSession, 3, 2)
	addresses := packageAddresses(batches...)
	manifest, m := manifestLine(t, packageSession, addresses)
	live.send(lines...)
	receiver.awaitArrivals(t, 3)
	live.send(manifest)
	live.requireQuiet(t)
	if state := receiver.state(); len(state.manifests) != 0 {
		t.Fatalf("the manifest reached the receiver while puts were in flight: %+v", state.manifests)
	}
	for _, put := range []string{puts[2], puts[0], puts[1]} {
		receiver.release(put)
	}
	for range puts {
		if result := live.result(t); result.Op != protocolcli.SessionReportingOpPut || !result.OK {
			t.Fatalf("result = %+v, want a put accepted before the manifest", result)
		}
	}
	requireResult(t, live.result(t), protocolcli.SessionReportingOpManifest, m.Address(), true, "", false)
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}
	manifests := receiver.state().manifests
	if len(manifests) != 1 || manifests[0].putsInFlight != 0 || manifests[0].putsAnswered != 3 || !manifests[0].final || !slices.Equal(manifests[0].addresses, addresses) {
		t.Fatalf("manifests = %+v, want one final manifest of every package, after the three puts", manifests)
	}

	missing := &packageReceiver{token: packageToken, contract: "2", packages: true}
	live = startLiveReporter(t, packageConfig(newPackageReceiver(t, missing)))
	document := protocolcli.NewSessionReportingPackage(protocolcli.SessionReportingPackageSession, []byte(`{"run":{}}`))
	absent := protocolcli.NewSessionReportingPackage(protocolcli.SessionReportingPackagePlan, []byte("{}"))
	put, address := putLine(t, packageSession, document)
	manifest, m = manifestLine(t, packageSession, []string{document.Address, absent.Address})
	live.send(put, manifest)
	requireResult(t, live.result(t), protocolcli.SessionReportingOpPut, address, true, "", false)
	requireResult(t, live.result(t), protocolcli.SessionReportingOpManifest, m.Address(), false, ackCodePackagesMissing, false)
	if err := live.close(t); err != nil {
		t.Fatal(err)
	}
}
