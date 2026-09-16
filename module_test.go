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
		SequenceMaxSeconds:        60,
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
	seqs := readings["sequences"].([]interface{})
	if len(seqs) != 0 {
		t.Fatalf("idle sequences want empty, got %v", seqs)
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
	if resp["session"] != "session:20260904_201120" {
		t.Fatalf("session tag: %v", resp["session"])
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
	tags := asStrings(o0["tags"].([]interface{}))
	assertContains(t, tags, "boat-a", "session:20260904_201120", "chunk:1")

	seqs := readings["sequences"].([]interface{})
	if len(seqs) != 1 {
		t.Fatalf("want 1 sequence, got %d", len(seqs))
	}
	seq := seqs[0].(map[string]interface{})
	seqTags := asStrings(seq["sequence_tags"].([]interface{}))
	assertContains(t, seqTags, "boat-a", "session:20260904_201120", "chunk:1")
	resources := seq["resources"].([]interface{})
	if len(resources) != 5 {
		t.Fatalf("want 5 sequence resources, got %d", len(resources))
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
	if len(readings["sequences"].([]interface{})) != 0 {
		t.Fatal("after stop sequences should be empty")
	}
}

func TestSequenceRolloverKeepsSession(t *testing.T) {
	start := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	now := start
	s := newTestSensor(t, testConfig(), func() time.Time { return now })

	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_capture": true,
		"frequency_hz":  1.0,
	}); err != nil {
		t.Fatal(err)
	}

	// Still within first minute.
	now = start.Add(59 * time.Second)
	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tags := overrideTags(readings)
	assertContains(t, tags, "session:20260904_201120", "chunk:1")
	if contains(tags, "chunk:2") {
		t.Fatal("should not have rolled yet")
	}

	// Cross 60s boundary → chunk 2, same session.
	now = start.Add(60 * time.Second)
	readings, err = s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tags = overrideTags(readings)
	assertContains(t, tags, "session:20260904_201120", "chunk:2")
	if contains(tags, "chunk:1") {
		t.Fatal("chunk:1 should be gone after rollover")
	}

	// Jump another 2 minutes without polling → chunk 4.
	now = start.Add(180 * time.Second)
	readings, err = s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tags = overrideTags(readings)
	assertContains(t, tags, "session:20260904_201120", "chunk:4")
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
	if resp["session"] != "session:20260904_210000" {
		t.Fatalf("new session want session:20260904_210000, got %v", resp["session"])
	}
	if resp["chunk"] != 1 {
		t.Fatalf("new session should start at chunk 1, got %v", resp["chunk"])
	}
}

func TestStartStopSequenceOnly(t *testing.T) {
	fixed := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	cfg := testConfig()
	cfg.DefaultCaptureFrequencyHz = 0
	s := newTestSensor(t, cfg, func() time.Time { return fixed })

	if _, err := s.DoCommand(context.Background(), map[string]interface{}{
		"start_sequence": true,
	}); err != nil {
		t.Fatal(err)
	}
	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	o0 := readings["overrides"].([]interface{})[0].(map[string]interface{})
	if o0["capture_frequency_hz"].(float32) != 0 {
		t.Fatal("start_sequence should not enable capture frequency")
	}
	if len(readings["sequences"].([]interface{})) != 1 {
		t.Fatal("start_sequence should open a sequence")
	}

	if _, err := s.DoCommand(context.Background(), map[string]interface{}{"stop_sequence": true}); err != nil {
		t.Fatal(err)
	}
	readings, err = s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(readings["sequences"].([]interface{})) != 0 {
		t.Fatal("stop_sequence should close sequence")
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
	s.detect = func(ctx context.Context) (bool, error) { return true, nil }

	s.tickDetection(context.Background())

	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	o0 := readings["overrides"].([]interface{})[0].(map[string]interface{})
	if o0["capture_frequency_hz"].(float32) != 1 {
		t.Fatalf("freq=%v", o0["capture_frequency_hz"])
	}
	tags := asStrings(o0["tags"].([]interface{}))
	assertContains(t, tags, "session:20260904_201120", "chunk:1")
	if len(readings["sequences"].([]interface{})) != 1 {
		t.Fatal("want open sequence after detection")
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
	s.detect = func(ctx context.Context) (bool, error) { return true, nil }

	s.tickDetection(context.Background())
	first := s.lastDetectionAt

	now = start.Add(5 * time.Second)
	s.tickDetection(context.Background())
	if !s.lastDetectionAt.After(first) {
		t.Fatalf("lastDetectionAt should advance: was %v now %v", first, s.lastDetectionAt)
	}
	if !s.sequenceActive {
		t.Fatal("session should stay active while detections continue")
	}
}

func TestInactivityStopsAfterDetection(t *testing.T) {
	start := time.Date(2026, 9, 4, 20, 11, 20, 0, time.Local)
	now := start
	cfg := testConfig()
	cfg.InactivitySeconds = 10
	s := newTestSensor(t, cfg, func() time.Time { return now })
	s.detect = func(ctx context.Context) (bool, error) { return true, nil }

	s.tickDetection(context.Background())
	if !s.sequenceActive {
		t.Fatal("expected active after detection")
	}

	s.detect = func(ctx context.Context) (bool, error) { return false, nil }
	now = start.Add(10 * time.Second)
	s.tickDetection(context.Background())
	if s.sequenceActive {
		t.Fatal("expected auto-stop after inactivity with prior detection")
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
	s.detect = func(ctx context.Context) (bool, error) { return false, nil }
	s.tickDetection(context.Background())
	readings, err := s.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(readings["sequences"].([]interface{})) != 1 {
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
	s.detect = func(ctx context.Context) (bool, error) { return true, nil }
	s.tickDetection(context.Background())
	if !s.seenDetectionThisSession {
		t.Fatal("detection after manual start should set seenDetectionThisSession")
	}

	s.detect = func(ctx context.Context) (bool, error) { return false, nil }
	now = start.Add(12 * time.Second)
	s.tickDetection(context.Background())
	if s.sequenceActive {
		t.Fatal("expected auto-stop after detection then inactivity")
	}
}

func TestMatchingDetection(t *testing.T) {
	dets := []objectdetection.Detection{
		&fakeDetection{label: "noise", score: 0.99},
		&fakeDetection{label: "DownSonar", score: 0.81},
	}
	if !matchingDetection(dets, "downsonar", 0.8) {
		t.Fatal("expected case-insensitive match")
	}
	if matchingDetection(dets, "downsonar", 0.9) {
		t.Fatal("score below threshold should not match")
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

func overrideTags(readings map[string]interface{}) []string {
	o0 := readings["overrides"].([]interface{})[0].(map[string]interface{})
	return asStrings(o0["tags"].([]interface{}))
}

func asStrings(in []interface{}) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = v.(string)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func assertContains(t *testing.T, got []string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !contains(got, w) {
			t.Fatalf("tags %v missing %q", got, w)
		}
	}
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
