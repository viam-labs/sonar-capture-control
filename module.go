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
	defaultInactivitySeconds   = 10
	defaultDetectionLabel      = "downsonar"
	defaultDetectionConfidence = 0.8
	defaultDetectionPollHz     = 1.0
)

// While a session runs, the camera and every sensor capture together once per
// captureInterval, and each capture is wrapped in its own sequence lasting
// 2*sequenceHalfWidth: one image plus one set of readings.
const (
	captureInterval   = 6 * time.Second
	sequenceHalfWidth = 750 * time.Millisecond
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

// Config configures which resources to capture, how captured data is tagged,
// and optional vision-driven auto start/stop.
type Config struct {
	// Resources are the resource/method pairs this sensor controls. Each pair
	// is emitted under "overrides" every poll, carrying the current capture
	// frequency and tags. At least one must be an image (GetImages/GetImage).
	Resources []ResourceMethod `json:"resources"`

	// DefaultTags are base tags applied to everything captured and to every
	// sequence.
	DefaultTags []string `json:"default_tags,omitempty"`

	// InactivitySeconds ends an auto/detection session after this many seconds
	// without a matching detection. Only applies after at least one detection
	// has been seen in the session. Default 10. Manual-only sessions (zero
	// detections) are not auto-stopped.
	InactivitySeconds float64 `json:"inactivity_seconds,omitempty"`

	// Vision is the vision service used for auto start/stop. When empty, auto
	// detection is disabled and sessions start and stop only by DoCommand.
	Vision string `json:"vision,omitempty"`

	// Camera is passed to DetectionsFromCamera. Required when Vision is set.
	Camera string `json:"camera,omitempty"`

	// DetectionLabel is the object class to match (case-insensitive). Default downsonar.
	DetectionLabel string `json:"detection_label,omitempty"`

	// DetectionConfidence is the minimum score. Default 0.8.
	DetectionConfidence float64 `json:"detection_confidence,omitempty"`

	// DetectionPollHz is how often to poll vision. Default 1.
	DetectionPollHz float64 `json:"detection_poll_hz,omitempty"`
}

// isImageMethod reports whether a resource/method pair produces images.
func isImageMethod(method string) bool {
	return method == "GetImages" || method == "GetImage"
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
	if cfg.InactivitySeconds < 0 {
		return nil, nil, fmt.Errorf("%s.inactivity_seconds cannot be negative", path)
	}
	if cfg.DetectionConfidence < 0 || cfg.DetectionConfidence > 1 {
		return nil, nil, fmt.Errorf("%s.detection_confidence must be between 0 and 1", path)
	}
	if cfg.DetectionPollHz < 0 {
		return nil, nil, fmt.Errorf("%s.detection_poll_hz cannot be negative", path)
	}

	hasImage := false
	for _, r := range cfg.Resources {
		hasImage = hasImage || isImageMethod(r.Method)
	}
	if !hasImage {
		return nil, nil, fmt.Errorf("%s.resources needs an image resource (method GetImages or GetImage)", path)
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

// detectOutcome is the result of one vision poll for the configured label.
type detectOutcome struct {
	// found is true when the label is present at or above detection_confidence.
	found bool
	// bestScore is the highest score for the configured label on this poll.
	bestScore float64
	// sawLabel is true when any detection had the configured label (any score).
	sawLabel bool
}

// detectFunc is injectable for tests.
type detectFunc func(ctx context.Context) (detectOutcome, error)

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
	extraTags                []string
	sessionActive            bool
	sessionStartedAt         time.Time
	lastDetectionAt          time.Time
	seenDetectionThisSession bool
	// lastPoll* tracks the most recent successful detection poll for stop logs.
	lastPollAt        time.Time
	lastPollBestScore float64
	lastPollSawLabel  bool

	// manualSequenceActive is set only by the start_sequence / stop_sequence
	// commands: one extra sequence on top of the per-capture ones.
	manualSequenceActive bool
	sequenceTags         []string
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
		name:       name,
		logger:     logger,
		cfg:        conf,
		now:        time.Now,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
		extraTags:  append([]string(nil), conf.DefaultTags...),
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
	tags := toAny(s.extraTags)
	seqActive := s.manualSequenceActive
	seqTags := toAny(s.sequenceTags)
	sessionActive := s.sessionActive
	sessionStart := s.sessionStartedAt
	now := s.now()
	s.mu.Unlock()

	// Every resource captures once per interval while a session runs. They are
	// all switched on in the same Data Manager update, so their timers start
	// together and each image and its readings are taken at the same moment.
	var freq float32
	if sessionActive {
		freq = float32(time.Second) / float32(captureInterval)
	}
	overrides := make([]interface{}, 0, len(s.cfg.Resources))
	for _, r := range s.cfg.Resources {
		overrides = append(overrides, map[string]interface{}{
			"resource_name":        r.ResourceName,
			"method":               r.Method,
			"capture_frequency_hz": freq,
			"tags":                 tags,
		})
	}

	// The sequences key is sent only while a sequence should be open. The Data
	// Manager treats a missing key exactly like an empty list -- it closes any
	// open Sequence -- so leaving it out is also how a sequence ends.
	var sequences []interface{}
	if seqActive {
		sequences = append(sequences, s.sequenceEntry(seqTags))
	}
	if sessionActive && aroundCapture(now, sessionStart) {
		sequences = append(sequences, s.sequenceEntry(tags))
	}

	out := map[string]interface{}{"overrides": overrides}
	if len(sequences) > 0 {
		out["sequences"] = sequences
	}
	return out, nil
}

// sequenceEntry is one sequence covering every configured resource.
func (s *sonarCaptureControlCaptureControl) sequenceEntry(tags []interface{}) map[string]interface{} {
	resources := make([]interface{}, 0, len(s.cfg.Resources))
	for _, r := range s.cfg.Resources {
		resources = append(resources, map[string]interface{}{
			"resource_name": r.ResourceName,
			"method":        r.Method,
		})
	}
	return map[string]interface{}{"resources": resources, "sequence_tags": tags}
}

// aroundCapture reports whether now falls inside the sequence wrapped around
// one of the session's captures.
//
// Collectors fire one full interval after they start, and every resource is
// switched on in the same Data Manager update, so captures land together at
// session start + k*captureInterval for k = 1, 2, 3 ... The sequence is open
// from sequenceHalfWidth before each capture to sequenceHalfWidth after it and
// closed in between; the Data Manager closes a sequence on the first check
// (every 100 ms) where it is no longer listed, so each one holds exactly that
// capture.
func aroundCapture(now, sessionStart time.Time) bool {
	elapsed := now.Sub(sessionStart)
	k := (elapsed + captureInterval/2) / captureInterval // nearest capture
	if k < 1 {
		return false
	}
	offset := elapsed - k*captureInterval
	return offset >= -sequenceHalfWidth && offset <= sequenceHalfWidth
}

// DoCommand exposes four commands. Each is selected by setting its key to true;
// optional arguments are read from sibling keys on the same map.
//
//	{"start_capture": true, "tags": ["boat-a"]}   // opens a session
//	{"stop_capture":  true}                       // closes it
//	{"start_sequence": true, "tags": ["boat-a"]}  // one extra sequence; capture is unchanged
//	{"stop_sequence":  true}
func (s *sonarCaptureControlCaptureControl) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	switch {
	case cmd["start_capture"] == true:
		extra := append([]string(nil), s.cfg.DefaultTags...)
		if v, ok := stringList(cmd["tags"]); ok {
			extra = append(extra, v...)
		}

		s.mu.Lock()
		s.startSessionLocked(extra, false /* fromDetection */)
		s.mu.Unlock()

		s.logger.Infof("start_capture tags=%v", extra)
		return map[string]interface{}{"tags": toAny(extra)}, nil

	case cmd["stop_capture"] == true:
		s.mu.Lock()
		s.stopSessionLocked("manual DoCommand stop_capture")
		s.mu.Unlock()
		return nil, nil

	case cmd["start_sequence"] == true:
		tags := append([]string(nil), s.cfg.DefaultTags...)
		if v, ok := stringList(cmd["tags"]); ok {
			tags = append(tags, v...)
		}
		s.mu.Lock()
		s.manualSequenceActive = true
		s.sequenceTags = tags
		s.mu.Unlock()
		s.logger.Infof("start_sequence tags=%v", tags)
		return map[string]interface{}{"sequence_tags": toAny(tags)}, nil

	case cmd["stop_sequence"] == true:
		s.mu.Lock()
		wasActive := s.manualSequenceActive
		s.manualSequenceActive = false
		s.sequenceTags = nil
		s.mu.Unlock()
		if wasActive {
			s.logger.Info("stop_sequence")
		} else {
			s.logger.Info("stop_sequence (already idle)")
		}
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
func (s *sonarCaptureControlCaptureControl) startSessionLocked(extraTags []string, fromDetection bool) {
	now := s.now()
	s.extraTags = append([]string(nil), extraTags...)
	s.sessionActive = true
	s.sessionStartedAt = now
	if fromDetection {
		s.lastDetectionAt = now
		s.seenDetectionThisSession = true
	} else {
		s.lastDetectionAt = time.Time{}
		s.seenDetectionThisSession = false
	}
}

// stopSessionLocked ends the current session and clears detection state.
// reason is logged so operators can tell manual vs inactivity stops apart.
// Caller must hold s.mu.
func (s *sonarCaptureControlCaptureControl) stopSessionLocked(reason string) {
	wasActive := s.sessionActive
	s.sessionActive = false
	s.sessionStartedAt = time.Time{}
	s.lastDetectionAt = time.Time{}
	s.seenDetectionThisSession = false
	s.extraTags = append([]string(nil), s.cfg.DefaultTags...)
	if wasActive {
		s.logger.Infof("stop_capture reason=%s", reason)
	} else {
		s.logger.Infof("stop_capture reason=%s (already idle)", reason)
	}
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
	out, err := s.detect(ctx)
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
	s.lastPollAt = now
	s.lastPollBestScore = out.bestScore
	s.lastPollSawLabel = out.sawLabel

	if out.found {
		if !s.sessionActive {
			s.startSessionLocked(append([]string(nil), s.cfg.DefaultTags...), true)
			s.logger.Infof("auto start_capture score=%.3f label=%s",
				out.bestScore, s.cfg.detectionLabel())
		} else {
			s.lastDetectionAt = now
			s.seenDetectionThisSession = true
		}
		return
	}
	s.maybeInactivityStopLocked()
}

func (s *sonarCaptureControlCaptureControl) detectFromVision(ctx context.Context) (detectOutcome, error) {
	if s.vision == nil {
		return detectOutcome{}, fmt.Errorf("vision service not configured")
	}
	dets, err := s.vision.DetectionsFromCamera(ctx, s.cfg.Camera, nil)
	if err != nil {
		return detectOutcome{}, err
	}
	return evaluateDetections(dets, s.cfg.detectionLabel(), s.cfg.detectionConfidence()), nil
}

func evaluateDetections(dets []objectdetection.Detection, label string, minScore float64) detectOutcome {
	best, saw := bestLabelScore(dets, label)
	return detectOutcome{
		found:     saw && best >= minScore,
		bestScore: best,
		sawLabel:  saw,
	}
}

func bestLabelScore(dets []objectdetection.Detection, label string) (best float64, saw bool) {
	want := strings.ToLower(label)
	for _, d := range dets {
		if d == nil {
			continue
		}
		if strings.ToLower(d.Label()) != want {
			continue
		}
		saw = true
		if d.Score() > best {
			best = d.Score()
		}
	}
	return best, saw
}

// maybeInactivityStopLocked stops the session when inactivity has elapsed after
// at least one detection this session. Caller must hold s.mu.
func (s *sonarCaptureControlCaptureControl) maybeInactivityStopLocked() {
	if !s.sessionActive || !s.seenDetectionThisSession || s.lastDetectionAt.IsZero() {
		return
	}
	now := s.now()
	since := now.Sub(s.lastDetectionAt)
	if since < s.cfg.inactivity() {
		return
	}
	label := s.cfg.detectionLabel()
	conf := s.cfg.detectionConfidence()
	var scoreInfo string
	if s.lastPollSawLabel {
		scoreInfo = fmt.Sprintf("%.3f (below confidence %.2f)", s.lastPollBestScore, conf)
		if s.lastPollBestScore >= conf {
			// Qualifying score on last poll but still stopping means polls
			// failed or clock skew; still report the number.
			scoreInfo = fmt.Sprintf("%.3f", s.lastPollBestScore)
		}
	} else if !s.lastPollAt.IsZero() {
		scoreInfo = "none (label not present on last poll)"
	} else {
		scoreInfo = "unknown (no successful poll yet)"
	}
	reason := fmt.Sprintf(
		"inactivity: no %s >= %.2f for %v (last qualifying detection %v ago); last_poll_%s_score=%s",
		label, conf, s.cfg.inactivity(), since.Round(time.Millisecond), label, scoreInfo,
	)
	s.stopSessionLocked(reason)
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
