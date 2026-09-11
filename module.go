package sonarcapturecontrol

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/vision"
	"go.viam.com/rdk/vision/objectdetection"
)

var (
	CaptureControl = resource.NewModel("viam", "sonar-capture-control", "capture-control")
)

const (
	defaultSequenceMaxSeconds     = 60
	defaultInactivitySeconds      = 10
	defaultDetectionLabel         = "downsonar"
	defaultDetectionConfidence    = 0.8
	defaultDetectionPollHz        = 1.0
	defaultAutoCaptureFrequencyHz = 1.0
	sessionTagPrefix              = "session:"
	chunkTagPrefix                = "chunk:"
	sessionTimestampLayout        = "20060102_150405"
)

func init() {
	resource.RegisterComponent(sensor.API, CaptureControl,
		resource.Registration[sensor.Sensor, *Config]{
			Constructor: newSonarCaptureControlCaptureControl,
		},
	)
}

// ResourceMethod is a resource/method pair controlled by this sensor.
type ResourceMethod struct {
	ResourceName string `json:"resource_name"`
	Method       string `json:"method"`
}

// Config configures which resources to capture, sequence timing, and optional
// vision-driven auto start/stop.
type Config struct {
	// Resources are the resource/method pairs this sensor controls. Each pair
	// is emitted under "overrides" every poll, and under "sequences" when a
	// sequence is open.
	Resources []ResourceMethod `json:"resources"`

	// DefaultCaptureFrequencyHz is the capture frequency emitted until a
	// start_capture DoCommand changes it. 0 disables capture.
	DefaultCaptureFrequencyHz float32 `json:"default_capture_frequency_hz"`

	// DefaultTags are base tags merged into overrides/sequences (in addition to
	// auto session:/chunk: tags while a sonar event is active).
	DefaultTags []string `json:"default_tags,omitempty"`

	// SequenceMaxSeconds is how long one Sequence stays open before rolling to
	// the next chunk while the same session remains active. Default 60.
	SequenceMaxSeconds float64 `json:"sequence_max_seconds,omitempty"`

	// InactivitySeconds ends an auto/detection session after this many seconds
	// without a matching detection. Only applies after at least one detection
	// has been seen in the session. Default 10. Manual-only sessions (zero
	// detections) are not auto-stopped.
	InactivitySeconds float64 `json:"inactivity_seconds,omitempty"`

	// Vision is the vision service used for auto start/stop. When empty, auto
	// detection is disabled (Phase 1 manual-only behavior).
	Vision string `json:"vision,omitempty"`

	// Camera is passed to DetectionsFromCamera. Required when Vision is set.
	Camera string `json:"camera,omitempty"`

	// DetectionLabel is the object class to match (case-insensitive). Default downsonar.
	DetectionLabel string `json:"detection_label,omitempty"`

	// DetectionConfidence is the minimum score. Default 0.8.
	DetectionConfidence float64 `json:"detection_confidence,omitempty"`

	// DetectionPollHz is how often to poll vision. Default 1.
	DetectionPollHz float64 `json:"detection_poll_hz,omitempty"`

	// AutoCaptureFrequencyHz is the capture frequency used when auto-starting
	// from a detection. Default 1.
	AutoCaptureFrequencyHz float32 `json:"auto_capture_frequency_hz,omitempty"`
}

func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if len(cfg.Resources) == 0 {
		return nil, nil, fmt.Errorf("%s: resources is required and must be non-empty", path)
	}
	for i, r := range cfg.Resources {
		if r.ResourceName == "" {
			return nil, nil, fmt.Errorf("%s.resources[%d].resource_name is required", path, i)
		}
		if r.Method == "" {
			return nil, nil, fmt.Errorf("%s.resources[%d].method is required", path, i)
		}
	}
	if cfg.SequenceMaxSeconds < 0 {
		return nil, nil, fmt.Errorf("%s.sequence_max_seconds cannot be negative", path)
	}
	if cfg.InactivitySeconds < 0 {
		return nil, nil, fmt.Errorf("%s.inactivity_seconds cannot be negative", path)
	}
	if cfg.DetectionConfidence < 0 || cfg.DetectionConfidence > 1 {
		return nil, nil, fmt.Errorf("%s.detection_confidence must be between 0 and 1", path)
	}
	if cfg.DetectionPollHz < 0 {
		return nil, nil, fmt.Errorf("%s.detection_poll_hz cannot be negative", path)
	}

	var deps []string
	if cfg.Vision != "" {
		if cfg.Camera == "" {
			return nil, nil, fmt.Errorf("%s.camera is required when vision is set", path)
		}
		deps = append(deps, cfg.Vision, cfg.Camera)
	}
	return deps, nil, nil
}

func (cfg *Config) sequenceMax() time.Duration {
	sec := cfg.SequenceMaxSeconds
	if sec == 0 {
		sec = defaultSequenceMaxSeconds
	}
	return time.Duration(sec * float64(time.Second))
}

func (cfg *Config) inactivity() time.Duration {
	sec := cfg.InactivitySeconds
	if sec == 0 {
		sec = defaultInactivitySeconds
	}
	return time.Duration(sec * float64(time.Second))
}

func (cfg *Config) detectionLabel() string {
	if cfg.DetectionLabel == "" {
		return defaultDetectionLabel
	}
	return cfg.DetectionLabel
}

func (cfg *Config) detectionConfidence() float64 {
	if cfg.DetectionConfidence == 0 {
		return defaultDetectionConfidence
	}
	return cfg.DetectionConfidence
}

func (cfg *Config) detectionPollInterval() time.Duration {
	hz := cfg.DetectionPollHz
	if hz == 0 {
		hz = defaultDetectionPollHz
	}
	return time.Duration(float64(time.Second) / hz)
}

func (cfg *Config) autoCaptureFrequency() float32 {
	if cfg.AutoCaptureFrequencyHz == 0 {
		return defaultAutoCaptureFrequencyHz
	}
	return cfg.AutoCaptureFrequencyHz
}

// detectFunc is injectable for tests. Returns whether a matching detection was seen.
type detectFunc func(ctx context.Context) (bool, error)

type sonarCaptureControlCaptureControl struct {
	resource.AlwaysRebuild

	name   resource.Name
	logger logging.Logger
	cfg    *Config

	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
	// detect is injectable for tests; when nil, uses vision service.
	detect detectFunc

	vision vision.Service

	cancelCtx  context.Context
	cancelFunc context.CancelFunc

	mu                       sync.Mutex
	captureFrequencyHz       float32
	extraTags                []string
	sequenceActive           bool
	sessionTag               string
	chunkIndex               int
	chunkStartedAt           time.Time
	lastDetectionAt          time.Time
	seenDetectionThisSession bool
}

func newSonarCaptureControlCaptureControl(
	ctx context.Context,
	deps resource.Dependencies,
	rawConf resource.Config,
	logger logging.Logger,
) (sensor.Sensor, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}
	return NewCaptureControl(ctx, deps, rawConf.ResourceName(), conf, logger)
}

func NewCaptureControl(
	ctx context.Context,
	deps resource.Dependencies,
	name resource.Name,
	conf *Config,
	logger logging.Logger,
) (sensor.Sensor, error) {
	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	s := &sonarCaptureControlCaptureControl{
		name:               name,
		logger:             logger,
		cfg:                conf,
		now:                time.Now,
		cancelCtx:          cancelCtx,
		cancelFunc:         cancelFunc,
		captureFrequencyHz: conf.DefaultCaptureFrequencyHz,
		extraTags:          append([]string(nil), conf.DefaultTags...),
	}

	if conf.Vision != "" {
		vis, err := vision.FromProvider(deps, conf.Vision)
		if err != nil {
			cancelFunc()
			return nil, fmt.Errorf("vision service %q: %w", conf.Vision, err)
		}
		s.vision = vis
		s.detect = s.detectFromVision
		go s.pollDetections()
	}

	return s, nil
}

func (s *sonarCaptureControlCaptureControl) Name() resource.Name {
	return s.name
}

func (s *sonarCaptureControlCaptureControl) Status(ctx context.Context) (map[string]interface{}, error) {
	return nil, nil
}

func (s *sonarCaptureControlCaptureControl) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	s.mu.Lock()
	s.maybeInactivityStopLocked()
	s.maybeRolloverLocked()
	freq := s.captureFrequencyHz
	tags := toAny(s.currentTagsLocked())
	seqActive := s.sequenceActive
	s.mu.Unlock()

	overrides := make([]interface{}, 0, len(s.cfg.Resources))
	for _, r := range s.cfg.Resources {
		overrides = append(overrides, map[string]interface{}{
			"resource_name":        r.ResourceName,
			"method":               r.Method,
			"capture_frequency_hz": freq,
			"tags":                 tags,
		})
	}

	sequences := []interface{}{}
	if seqActive {
		resources := make([]interface{}, 0, len(s.cfg.Resources))
		for _, r := range s.cfg.Resources {
			resources = append(resources, map[string]interface{}{
				"resource_name": r.ResourceName,
				"method":        r.Method,
			})
		}
		sequences = append(sequences, map[string]interface{}{
			"resources":     resources,
			"sequence_tags": tags,
		})
	}

	return map[string]interface{}{
		"overrides": overrides,
		"sequences": sequences,
	}, nil
}

// DoCommand exposes four commands. Each is selected by setting its key to true;
// optional arguments are read from sibling keys on the same map.
//
//	{"start_capture": true, "frequency_hz": 2.0, "tags": ["boat-a"]}  // opens session+sequence
//	{"stop_capture":  true}                                           // closes sequence, freq=0
//	{"start_sequence": true, "tags": ["boat-a"]}
//	{"stop_sequence":  true}
func (s *sonarCaptureControlCaptureControl) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	switch {
	case cmd["start_capture"] == true:
		freq := s.cfg.DefaultCaptureFrequencyHz
		if v, ok := asFloat64(cmd["frequency_hz"]); ok {
			freq = float32(v)
		}
		extra := append([]string(nil), s.cfg.DefaultTags...)
		if v, ok := stringList(cmd["tags"]); ok {
			extra = append(extra, v...)
		}

		s.mu.Lock()
		s.startSessionLocked(freq, extra, false /* fromDetection */)
		tags := s.currentTagsLocked()
		sessionTag := s.sessionTag
		s.mu.Unlock()

		s.logger.Infof("start_capture frequency_hz=%v session=%v tags=%v", freq, sessionTag, tags)
		return map[string]interface{}{
			"capture_frequency_hz": freq,
			"tags":                 toAny(tags),
			"sequence_tags":        toAny(tags),
			"session":              sessionTag,
			"chunk":                1,
		}, nil

	case cmd["stop_capture"] == true:
		s.mu.Lock()
		s.stopSessionLocked()
		s.mu.Unlock()
		s.logger.Info("stop_capture (sequence closed)")
		return nil, nil

	case cmd["start_sequence"] == true:
		extra := append([]string(nil), s.cfg.DefaultTags...)
		if v, ok := stringList(cmd["tags"]); ok {
			extra = append(extra, v...)
		}

		s.mu.Lock()
		// Sequence-only: keep capture frequency as-is (typically 0).
		freq := s.captureFrequencyHz
		s.startSessionLocked(freq, extra, false)
		tags := s.currentTagsLocked()
		sessionTag := s.sessionTag
		s.mu.Unlock()

		s.logger.Infof("start_sequence session=%v tags=%v", sessionTag, tags)
		return map[string]interface{}{
			"sequence_tags": toAny(tags),
			"session":       sessionTag,
			"chunk":         1,
		}, nil

	case cmd["stop_sequence"] == true:
		s.mu.Lock()
		s.sequenceActive = false
		s.sessionTag = ""
		s.chunkIndex = 0
		s.chunkStartedAt = time.Time{}
		s.lastDetectionAt = time.Time{}
		s.seenDetectionThisSession = false
		s.mu.Unlock()
		s.logger.Info("stop_sequence")
		return nil, nil
	}
	return nil, fmt.Errorf("unknown command: %v", cmd)
}

func (s *sonarCaptureControlCaptureControl) Close(context.Context) error {
	if s.cancelFunc != nil {
		s.cancelFunc()
	}
	return nil
}

// startSessionLocked opens a new capture session. If fromDetection is true,
// marks that a detection has been seen (enables inactivity timeout).
// Caller must hold s.mu.
func (s *sonarCaptureControlCaptureControl) startSessionLocked(freq float32, extraTags []string, fromDetection bool) {
	now := s.now()
	s.captureFrequencyHz = freq
	s.extraTags = append([]string(nil), extraTags...)
	s.sequenceActive = true
	s.sessionTag = sessionTagPrefix + now.Format(sessionTimestampLayout)
	s.chunkIndex = 1
	s.chunkStartedAt = now
	if fromDetection {
		s.lastDetectionAt = now
		s.seenDetectionThisSession = true
	} else {
		s.lastDetectionAt = time.Time{}
		s.seenDetectionThisSession = false
	}
}

// stopSessionLocked ends the current session and clears detection state.
// Caller must hold s.mu.
func (s *sonarCaptureControlCaptureControl) stopSessionLocked() {
	s.captureFrequencyHz = 0
	s.sequenceActive = false
	s.sessionTag = ""
	s.chunkIndex = 0
	s.chunkStartedAt = time.Time{}
	s.lastDetectionAt = time.Time{}
	s.seenDetectionThisSession = false
	s.extraTags = append([]string(nil), s.cfg.DefaultTags...)
}

func (s *sonarCaptureControlCaptureControl) pollDetections() {
	ticker := time.NewTicker(s.cfg.detectionPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-s.cancelCtx.Done():
			return
		case <-ticker.C:
			s.tickDetection(s.cancelCtx)
		}
	}
}

func (s *sonarCaptureControlCaptureControl) tickDetection(ctx context.Context) {
	if s.detect == nil {
		return
	}
	found, err := s.detect(ctx)
	if err != nil {
		s.logger.Warnf("detection poll failed: %v", err)
		s.mu.Lock()
		s.maybeInactivityStopLocked()
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if found {
		if !s.sequenceActive {
			freq := s.cfg.autoCaptureFrequency()
			s.startSessionLocked(freq, append([]string(nil), s.cfg.DefaultTags...), true)
			s.logger.Infof("auto start_capture frequency_hz=%v session=%v (detection)", freq, s.sessionTag)
		} else {
			s.lastDetectionAt = now
			s.seenDetectionThisSession = true
		}
		return
	}
	s.maybeInactivityStopLocked()
}

func (s *sonarCaptureControlCaptureControl) detectFromVision(ctx context.Context) (bool, error) {
	if s.vision == nil {
		return false, fmt.Errorf("vision service not configured")
	}
	dets, err := s.vision.DetectionsFromCamera(ctx, s.cfg.Camera, nil)
	if err != nil {
		return false, err
	}
	return matchingDetection(dets, s.cfg.detectionLabel(), s.cfg.detectionConfidence()), nil
}

func matchingDetection(dets []objectdetection.Detection, label string, minScore float64) bool {
	want := strings.ToLower(label)
	for _, d := range dets {
		if d == nil {
			continue
		}
		if strings.ToLower(d.Label()) == want && d.Score() >= minScore {
			return true
		}
	}
	return false
}

// maybeInactivityStopLocked stops the session when inactivity has elapsed after
// at least one detection this session. Caller must hold s.mu.
func (s *sonarCaptureControlCaptureControl) maybeInactivityStopLocked() {
	if !s.sequenceActive || !s.seenDetectionThisSession || s.lastDetectionAt.IsZero() {
		return
	}
	if s.now().Sub(s.lastDetectionAt) < s.cfg.inactivity() {
		return
	}
	s.logger.Infof("auto stop_capture session=%v (inactivity %v)", s.sessionTag, s.cfg.inactivity())
	s.stopSessionLocked()
}

// maybeRolloverLocked advances the chunk when the current Sequence has been open
// long enough. Caller must hold s.mu.
func (s *sonarCaptureControlCaptureControl) maybeRolloverLocked() {
	if !s.sequenceActive || s.chunkStartedAt.IsZero() {
		return
	}
	max := s.cfg.sequenceMax()
	now := s.now()
	if now.Sub(s.chunkStartedAt) < max {
		return
	}
	elapsed := now.Sub(s.chunkStartedAt)
	steps := int(elapsed / max)
	if steps < 1 {
		return
	}
	s.chunkIndex += steps
	s.chunkStartedAt = s.chunkStartedAt.Add(time.Duration(steps) * max)
	s.logger.Infof("sequence rollover session=%v chunk=%d", s.sessionTag, s.chunkIndex)
}

// currentTagsLocked returns merged tags for overrides and sequences.
// Caller must hold s.mu.
func (s *sonarCaptureControlCaptureControl) currentTagsLocked() []string {
	if !s.sequenceActive {
		return append([]string(nil), s.extraTags...)
	}
	out := make([]string, 0, len(s.extraTags)+2)
	out = append(out, s.extraTags...)
	out = append(out, s.sessionTag, fmt.Sprintf("%s%d", chunkTagPrefix, s.chunkIndex))
	return out
}

func stringList(v interface{}) ([]string, bool) {
	raw, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, x := range raw {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func toAny(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func asFloat64(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	default:
		return 0, false
	}
}
