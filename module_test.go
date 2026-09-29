package sonarcapturecontrol

import (
	"context"
	"image"
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
		DefaultCaptureFrequencyHz: 0,
	}
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
		"frequency_hz":  1.0,
		"tags":          []interface{}{"boat-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp["capture_frequency_hz"] != float32(1) {
		t.Fatalf("start response freq=%v", resp["capture_frequency_hz"])
	}

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	overrides := readings["overrides"].([]interface{})
	o0 := overrides[0].(map[string]interface{})
	if o0["capture_frequency_hz"].(float32) != 1 {
		t.Fatalf("freq=%v", o0["capture_frequency_hz"])
	}
	// Only the requested tags: no session:/chunk: tags, and no Sequence.
	if tags := asStrings(o0["tags"].([]interface{})); len(tags) != 1 || tags[0] != "boat-a" {
		t.Fatalf("tags=%v, want [boat-a]", tags)
	}
	if _, ok := readings["sequences"]; ok {
		t.Fatal("an active session must not open a sequence")
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
		"frequency_hz":  1.0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DoCommand(context.Background(), map[string]interface{}{"stop_capture": true}); err != nil {
		t.Fatal(err)
	}

	now = t2
	resp, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
		"frequency_hz":  1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp["capture_frequency_hz"] != float32(1) || !s.sessionActive {
		t.Fatalf("restart after stop: freq=%v active=%v", resp["capture_frequency_hz"], s.sessionActive)
	}
}

func TestManualSequenceOnly(t *testing.T) {
	s := newTestSensor(t, testConfig(), nil)

	// Capturing does not open a Sequence on its own.
	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true, "frequency_hz": 1.0,
	}); err != nil {
		t.Fatal(err)
	}
	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readings["sequences"]; ok {
		t.Fatal("start_capture must not open a sequence")
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
	if o0["capture_frequency_hz"].(float32) != 1 {
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
	cfg := testConfig()
	cfg.AutoCaptureFrequencyHz = 1
	s := newTestSensor(t, cfg, func() time.Time { return fixed })
	s.detect = func(ctx context.Context) (detectOutcome, error) {
		return detectOutcome{found: true, bestScore: 0.91, sawLabel: true}, nil
	}

	s.tickDetection(context.Background())

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	o0 := readings["overrides"].([]interface{})[0].(map[string]interface{})
	if o0["capture_frequency_hz"].(float32) != 1 {
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
		"frequency_hz":  1.0,
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
		"frequency_hz":  1.0,
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

func TestBandTagFormat(t *testing.T) {
	b := BandTag{Tag: "depth_band", Width: 10, Unit: "fm"}
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{93.01, "depth_band:90-100fm"},
		{90, "depth_band:90-100fm"},
		{99.99, "depth_band:90-100fm"},
		{100, "depth_band:100-110fm"},
		{4.7, "depth_band:0-10fm"},
	} {
		if got := b.format(tc.in); got != tc.want {
			t.Errorf("format(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadingFloatAcceptsNestedShape(t *testing.T) {
	flat := map[string]interface{}{"Water Temperature": 27.5}
	if v, ok := readingFloat(flat, "Water Temperature"); !ok || v != 27.5 {
		t.Errorf("flat: got %v %v", v, ok)
	}
	nested := map[string]interface{}{"readings": map[string]interface{}{"Depth": 93.01}}
	if v, ok := readingFloat(nested, "Depth"); !ok || v != 93.01 {
		t.Errorf("nested: got %v %v", v, ok)
	}
	if _, ok := readingFloat(flat, "missing"); ok {
		t.Error("missing key should not resolve")
	}
}

func TestBandTagsAppearInReadingsOnlyDuringSession(t *testing.T) {
	cfg := testConfig()
	s := newTestSensor(t, cfg, nil)
	s.bandTags = []string{"depth_band:90-100fm"}

	// Outside a session the module emits only its base tags.
	if tags := s.currentTagsLocked(); len(tags) != 0 {
		t.Fatalf("idle tags = %v, want none", tags)
	}

	s.startSessionLocked(1, nil, false)
	tags := s.currentTagsLocked()
	var found bool
	for _, tag := range tags {
		if tag == "depth_band:90-100fm" {
			found = true
		}
	}
	if !found {
		t.Fatalf("band tag missing from %v", tags)
	}
}
