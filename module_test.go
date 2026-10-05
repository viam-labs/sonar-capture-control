package sonarcapturecontrol

import (
	"context"
	"image"
	"math"
	"testing"
	"time"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/vision/objectdetection"
)

func testConfig() *Config {
	return &Config{
		Resources: []ResourceMethod{
			{ResourceName: "HDMICapture-filtered", Method: "GetImages"},
			{ResourceName: "garmin", Method: "Position"},
			{ResourceName: "garmin", Method: "CompassHeading"},
			{ResourceName: "depth", Method: "Readings"},
			{ResourceName: "seatemp", Method: "Readings"},
		},
	}
}

// sessionFreq is the capture frequency every resource gets during a session:
// once per captureInterval.
const sessionFreq = 1.0 / 6

func isSessionFreq(f interface{}) bool {
	return math.Abs(float64(f.(float32))-sessionFreq) < 1e-6
}

func newTestSensor(t *testing.T, cfg *Config, now func() time.Time) *sonarCaptureControlCaptureControl {
	t.Helper()
	logger := logging.NewTestLogger(t)
	s, err := NewCaptureControl(context.Background(), nil, sensor.Named("capture-sensor"), cfg, logger)
	if err != nil {
		t.Fatalf("NewCaptureControl: %v", err)
	}
	impl := s.(*sonarCaptureControlCaptureControl)
	if now != nil {
		impl.now = now
	}
	return impl
}

func TestConfigValidate(t *testing.T) {
	cfg := &Config{}
	if _, _, err := cfg.Validate("components.0"); err == nil {
		t.Fatal("expected error for empty resources")
	}

	cfg = testConfig()
	cfg.Resources[0].Method = ""
	if _, _, err := cfg.Validate("components.0"); err == nil {
		t.Fatal("expected error for empty method")
	}

	cfg = testConfig()
	if _, _, err := cfg.Validate("components.0"); err != nil {
		t.Fatalf("unexpected validate error: %v", err)
	}

	cfg = testConfig()
	cfg.Resources = []ResourceMethod{{ResourceName: "depth", Method: "Readings"}}
	if _, _, err := cfg.Validate("components.0"); err == nil {
		t.Fatal("expected error without an image resource")
	}

	cfg = testConfig()
	cfg.Vision = "sonar-finder-service"
	if _, _, err := cfg.Validate("components.0"); err == nil {
		t.Fatal("expected error when vision set without camera")
	}

	cfg.Camera = "HDMICapture"
	deps, _, err := cfg.Validate("components.0")
	if err != nil {
		t.Fatalf("unexpected validate error: %v", err)
	}
	if len(deps) != 2 || deps[0] != "sonar-finder-service" || deps[1] != "HDMICapture" {
		t.Fatalf("deps=%v", deps)
	}
}

func TestIdleReadings(t *testing.T) {
	s := newTestSensor(t, testConfig(), nil)
	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	overrides := readings["overrides"].([]interface{})
	if len(overrides) != 5 {
		t.Fatalf("got %d overrides, want 5", len(overrides))
	}
	first := overrides[0].(map[string]interface{})
	if first["capture_frequency_hz"].(float32) != 0 {
		t.Fatalf("idle freq want 0, got %v", first["capture_frequency_hz"])
	}
	if _, ok := readings["sequences"]; ok {
		t.Fatal("readings must not carry a sequences key")
	}
}

func TestStartStopCapture(t *testing.T) {
	fixed := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	s := newTestSensor(t, testConfig(), func() time.Time { return fixed })

	resp, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
		"tags":          []interface{}{"boat-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tags := asStrings(resp["tags"].([]interface{})); len(tags) != 1 || tags[0] != "boat-a" {
		t.Fatalf("start response tags=%v", tags)
	}

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	overrides := readings["overrides"].([]interface{})
	o0 := overrides[0].(map[string]interface{})
	if !isSessionFreq(o0["capture_frequency_hz"]) {
		t.Fatalf("freq=%v", o0["capture_frequency_hz"])
	}
	// Only the requested tags, and no sequence before the first capture.
	if tags := asStrings(o0["tags"].([]interface{})); len(tags) != 1 || tags[0] != "boat-a" {
		t.Fatalf("tags=%v, want [boat-a]", tags)
	}
	if _, ok := readings["sequences"]; ok {
		t.Fatal("no sequence before the first capture")
	}

	if _, err := s.DoCommand(context.Background(), map[string]interface{}{"stop_capture": true}); err != nil {
		t.Fatal(err)
	}
	readings, err = s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	o0 = readings["overrides"].([]interface{})[0].(map[string]interface{})
	if o0["capture_frequency_hz"].(float32) != 0 {
		t.Fatalf("after stop freq=%v", o0["capture_frequency_hz"])
	}
}

func TestNewSessionAfterStop(t *testing.T) {
	t1 := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	t2 := time.Date(2026, 9, 4, 21, 0, 0, 0, time.Local)
	now := t1
	s := newTestSensor(t, testConfig(), func() time.Time { return now })

	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DoCommand(context.Background(), map[string]interface{}{"stop_capture": true}); err != nil {
		t.Fatal(err)
	}

	now = t2
	resp, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.sessionActive || !s.sessionStartedAt.Equal(t2) {
		t.Fatalf("restart after stop: active=%v started=%v", s.sessionActive, s.sessionStartedAt)
	}
	_ = resp
}

func TestManualSequenceOnly(t *testing.T) {
	s := newTestSensor(t, testConfig(), nil)

	// Right after start_capture there is no sequence: the first capture is
	// 6 s away.
	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
	}); err != nil {
		t.Fatal(err)
	}
	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readings["sequences"]; ok {
		t.Fatal("no sequence before the first capture")
	}

	// start_sequence opens exactly one, and leaves capture as it was.
	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_sequence": true, "tags": []interface{}{"boat-a"},
	}); err != nil {
		t.Fatal(err)
	}
	readings, err = s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seqs, ok := readings["sequences"].([]interface{})
	if !ok || len(seqs) != 1 {
		t.Fatalf("want one sequence after start_sequence, got %v", readings["sequences"])
	}
	seq := seqs[0].(map[string]interface{})
	if tags := asStrings(seq["sequence_tags"].([]interface{})); len(tags) != 1 || tags[0] != "boat-a" {
		t.Fatalf("sequence_tags=%v", tags)
	}
	if n := len(seq["resources"].([]interface{})); n != 5 {
		t.Fatalf("want 5 sequence resources, got %d", n)
	}
	o0 := readings["overrides"].([]interface{})[0].(map[string]interface{})
	if !isSessionFreq(o0["capture_frequency_hz"]) {
		t.Fatalf("start_sequence changed capture frequency to %v", o0["capture_frequency_hz"])
	}

	// stop_sequence removes the key entirely -- not even an empty list.
	if _, err := s.DoCommand(context.Background(), map[string]interface{}{"stop_sequence": true}); err != nil {
		t.Fatal(err)
	}
	readings, err = s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readings["sequences"]; ok {
		t.Fatal("stop_sequence must leave no sequences key")
	}
}

func TestUnknownCommand(t *testing.T) {
	s := newTestSensor(t, testConfig(), nil)
	_, err := s.DoCommand(context.Background(), map[string]interface{}{"nope": true})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAutoStartFromDetection(t *testing.T) {
	fixed := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	s := newTestSensor(t, testConfig(), func() time.Time { return fixed })
	s.detect = func(ctx context.Context) (detectOutcome, error) {
		return detectOutcome{found: true, bestScore: 0.91, sawLabel: true}, nil
	}

	s.tickDetection(context.Background())

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	o0 := readings["overrides"].([]interface{})[0].(map[string]interface{})
	if !isSessionFreq(o0["capture_frequency_hz"]) {
		t.Fatalf("freq=%v", o0["capture_frequency_hz"])
	}
	if tags := asStrings(o0["tags"].([]interface{})); len(tags) != 0 {
		t.Fatalf("auto-start should add no tags of its own, got %v", tags)
	}
	if !s.sessionActive {
		t.Fatal("want active session after detection")
	}
	if !s.seenDetectionThisSession {
		t.Fatal("seenDetectionThisSession should be true")
	}
}

func TestDetectionRefreshesLastSeen(t *testing.T) {
	start := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	now := start
	cfg := testConfig()
	cfg.InactivitySeconds = 10
	s := newTestSensor(t, cfg, func() time.Time { return now })
	s.detect = func(ctx context.Context) (detectOutcome, error) {
		return detectOutcome{found: true, bestScore: 0.91, sawLabel: true}, nil
	}

	s.tickDetection(context.Background())
	first := s.lastDetectionAt

	now = start.Add(5 * time.Second)
	s.tickDetection(context.Background())
	if !s.lastDetectionAt.After(first) {
		t.Fatalf("lastDetectionAt should advance: was %v now %v", first, s.lastDetectionAt)
	}
	if !s.sessionActive {
		t.Fatal("session should stay active while detections continue")
	}
}

func TestInactivityStopsAfterDetection(t *testing.T) {
	start := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	now := start
	cfg := testConfig()
	cfg.InactivitySeconds = 10
	s := newTestSensor(t, cfg, func() time.Time { return now })
	s.detect = func(ctx context.Context) (detectOutcome, error) {
		return detectOutcome{found: true, bestScore: 0.91, sawLabel: true}, nil
	}

	s.tickDetection(context.Background())
	if !s.sessionActive {
		t.Fatal("expected active after detection")
	}

	s.detect = func(ctx context.Context) (detectOutcome, error) {
		return detectOutcome{found: false, bestScore: 0.42, sawLabel: true}, nil
	}
	now = start.Add(10 * time.Second)
	s.tickDetection(context.Background())
	if s.sessionActive {
		t.Fatal("expected auto-stop after inactivity with prior detection")
	}
	if !s.lastPollSawLabel || s.lastPollBestScore != 0.42 {
		t.Fatalf("expected last poll score 0.42, got saw=%v score=%v", s.lastPollSawLabel, s.lastPollBestScore)
	}
}

func TestManualOnlyNoAutoStop(t *testing.T) {
	start := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	now := start
	cfg := testConfig()
	cfg.InactivitySeconds = 10
	s := newTestSensor(t, cfg, func() time.Time { return now })

	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
	}); err != nil {
		t.Fatal(err)
	}

	now = start.Add(30 * time.Second)
	s.detect = func(ctx context.Context) (detectOutcome, error) {
		return detectOutcome{found: false, bestScore: 0.42, sawLabel: true}, nil
	}
	s.tickDetection(context.Background())
	// Readings is where the inactivity stop is evaluated, so it must run.
	if _, err := s.Readings(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !s.sessionActive {
		t.Fatal("manual-only session must stay active past inactivity")
	}
	if s.seenDetectionThisSession {
		t.Fatal("manual start should not set seenDetectionThisSession")
	}
}

func TestManualThenDetectionThenInactivityStops(t *testing.T) {
	start := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	now := start
	cfg := testConfig()
	cfg.InactivitySeconds = 10
	s := newTestSensor(t, cfg, func() time.Time { return now })

	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
	}); err != nil {
		t.Fatal(err)
	}

	now = start.Add(2 * time.Second)
	s.detect = func(ctx context.Context) (detectOutcome, error) {
		return detectOutcome{found: true, bestScore: 0.88, sawLabel: true}, nil
	}
	s.tickDetection(context.Background())
	if !s.seenDetectionThisSession {
		t.Fatal("detection after manual start should set seenDetectionThisSession")
	}

	s.detect = func(ctx context.Context) (detectOutcome, error) {
		return detectOutcome{found: false, bestScore: 0.55, sawLabel: true}, nil
	}
	now = start.Add(12 * time.Second)
	s.tickDetection(context.Background())
	if s.sessionActive {
		t.Fatal("expected auto-stop after detection then inactivity")
	}
}

func TestMatchingDetection(t *testing.T) {
	dets := []objectdetection.Detection{
		&fakeDetection{label: "noise", score: 0.99},
		&fakeDetection{label: "DownSonar", score: 0.81},
	}
	out := evaluateDetections(dets, "downsonar", 0.8)
	if !out.found || !out.sawLabel || out.bestScore != 0.81 {
		t.Fatalf("out=%+v", out)
	}
	out = evaluateDetections(dets, "downsonar", 0.9)
	if out.found || !out.sawLabel || out.bestScore != 0.81 {
		t.Fatalf("below threshold out=%+v", out)
	}
	out = evaluateDetections(dets, "missing", 0.8)
	if out.found || out.sawLabel {
		t.Fatalf("missing label out=%+v", out)
	}
}

type fakeDetection struct {
	label string
	score float64
}

func (f *fakeDetection) BoundingBox() *image.Rectangle    { return nil }
func (f *fakeDetection) NormalizedBoundingBox() []float64 { return nil }
func (f *fakeDetection) Score() float64                   { return f.score }
func (f *fakeDetection) Label() string                    { return f.label }

func asStrings(in []interface{}) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = v.(string)
	}
	return out
}

// Ensure the constructor type satisfies sensor.Sensor at compile time in tests.
var _ sensor.Sensor = (*sonarCaptureControlCaptureControl)(nil)

func TestResourceName(t *testing.T) {
	s := newTestSensor(t, testConfig(), nil)
	if s.Name() != sensor.Named("capture-sensor") {
		t.Fatalf("name=%v", s.Name())
	}
	_ = resource.Name{}
}

func TestPerCaptureSequences(t *testing.T) {
	cfg := testConfig()
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) // on a 6 s boundary
	now := t0
	s := newTestSensor(t, cfg, func() time.Time { return now })

	read := func() map[string]interface{} {
		t.Helper()
		r, err := s.Readings(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Idle: no session, so no sequence.
	if _, ok := read()["sequences"]; ok {
		t.Fatal("idle module must not open a sequence")
	}

	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
	}); err != nil {
		t.Fatal(err)
	}

	// Every resource -- image and readings alike -- captures once every 6 s.
	r := read()
	for _, o := range r["overrides"].([]interface{}) {
		o := o.(map[string]interface{})
		if !isSessionFreq(o["capture_frequency_hz"]) {
			t.Fatalf("%s/%s frequency %v, want 1/6", o["resource_name"], o["method"], o["capture_frequency_hz"])
		}
	}

	// A 1.5-second sequence is open around each capture (start + 6 s, 12 s, ...)
	// and closed in between.
	isOpen := func(at time.Duration) bool {
		now = t0.Add(at)
		seqs, ok := read()["sequences"].([]interface{})
		if ok && len(seqs) != 1 {
			t.Fatalf("at +%v: want at most one sequence, got %d", at, len(seqs))
		}
		if ok {
			seq := seqs[0].(map[string]interface{})
			if n := len(seq["resources"].([]interface{})); n != len(cfg.Resources) {
				t.Fatalf("sequence covers %d resources, want %d", n, len(cfg.Resources))
			}
		}
		return ok
	}
	if _, ok := r["sequences"]; ok {
		t.Fatal("sequence open at session start, before any capture")
	}
	for _, tc := range []struct {
		at   time.Duration
		open bool
	}{
		{3 * time.Second, false},
		{5100 * time.Millisecond, false},
		{5300 * time.Millisecond, true}, // 0.75 s before the first capture
		{6 * time.Second, true},         // the first capture
		{6700 * time.Millisecond, true},
		{6900 * time.Millisecond, false}, // closed straight after
		{9 * time.Second, false},
		{12 * time.Second, true}, // the second capture
		{12900 * time.Millisecond, false},
	} {
		if got := isOpen(tc.at); got != tc.open {
			t.Fatalf("at +%v: sequence open=%v, want %v", tc.at, got, tc.open)
		}
	}

	// Stopping capture ends the sequences and turns the camera off.
	if _, err := s.DoCommand(context.Background(), map[string]interface{}{"stop_capture": true}); err != nil {
		t.Fatal(err)
	}
	r = read()
	if _, ok := r["sequences"]; ok {
		t.Fatal("stopped session must not keep a sequence open")
	}
	for _, o := range r["overrides"].([]interface{}) {
		if f := o.(map[string]interface{})["capture_frequency_hz"].(float32); f != 0 {
			t.Fatalf("frequency %v after stop, want 0", f)
		}
	}
}
