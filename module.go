package sonarcapturecontrol

import (
	"context"
	"fmt"
	"math"
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
	defaultInactivitySeconds      = 10
	defaultDetectionLabel         = "downsonar"
	defaultDetectionConfidence    = 0.8
	defaultDetectionPollHz        = 1.0
	defaultAutoCaptureFrequencyHz = 1.0
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

// BandTag turns a numeric sensor reading into a coarse bucketed tag such as
// "depth_band:90-100fm".
//
// The bucketing is the point. Tagging raw readings would mint a distinct tag
// per frame -- one 60-second window of this fleet's data carries 59 distinct
// compass headings and 59 distinct positions -- which leaves the organization
// with hundreds of thousands of single-use tags and a tag vocabulary that can
// no longer be used to filter anything. Bands stay in the tens, so they remain
// useful for browsing and for stratifying annotation batches. Precise values
// belong in tabular data, which is captured alongside the images.
type BandTag struct {
	// Sensor is the resource name to read.
	Sensor string `json:"sensor"`
	// Reading is the key within that sensor's Readings map.
	Reading string `json:"reading"`
	// Tag is the emitted prefix, e.g. "depth_band".
	Tag string `json:"tag"`
	// Width is the bucket size in the reading's own units.
	Width float64 `json:"width"`
	// Unit is an optional suffix appended to the range, e.g. "fm".
	Unit string `json:"unit,omitempty"`
}

// format buckets v and renders the tag. 93.01 with width 10 becomes "90-100".
func (b BandTag) format(v float64) string {
	lo := math.Floor(v/b.Width) * b.Width
	return fmt.Sprintf("%s:%g-%g%s", b.Tag, lo, lo+b.Width, b.Unit)
}

// Config configures which resources to capture, how captured data is tagged,
// and optional vision-driven auto start/stop.
type Config struct {
	// Resources are the resource/method pairs this sensor controls. Each pair
	// is emitted under "overrides" every poll, carrying the current capture
	// frequency and tags.
	Resources []ResourceMethod `json:"resources"`

	// DefaultCaptureFrequencyHz is the capture frequency emitted until a
	// start_capture DoCommand changes it. 0 disables capture.
	DefaultCaptureFrequencyHz float32 `json:"default_capture_frequency_hz"`

	// DefaultTags are base tags applied to everything captured, alongside any
	// band tags while a session is active.
	DefaultTags []string `json:"default_tags,omitempty"`

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

	// BandTags are optional sensor readings added to captured data as bucketed
	// tags while a session is active. Empty disables the feature.
	BandTags []BandTag `json:"band_tags,omitempty"`
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

	for i, b := range cfg.BandTags {
		if b.Sensor == "" {
			return nil, nil, fmt.Errorf("%s.band_tags[%d].sensor is required", path, i)
		}
		if b.Reading == "" {
			return nil, nil, fmt.Errorf("%s.band_tags[%d].reading is required", path, i)
		}
		if b.Tag == "" {
			return nil, nil, fmt.Errorf("%s.band_tags[%d].tag is required", path, i)
		}
		if b.Width <= 0 {
			return nil, nil, fmt.Errorf("%s.band_tags[%d].width must be positive", path, i)
		}
	}

	var deps []string
	if cfg.Vision != "" {
		if cfg.Camera == "" {
			return nil, nil, fmt.Errorf("%s.camera is required when vision is set", path)
		}
		deps = append(deps, cfg.Vision, cfg.Camera)
	}
	seen := map[string]bool{}
	for _, b := range cfg.BandTags {
		if !seen[b.Sensor] {
			seen[b.Sensor] = true
			deps = append(deps, b.Sensor)
		}
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

func (cfg *Config) autoCaptureFrequency() float32 {
	if cfg.AutoCaptureFrequencyHz == 0 {
		return defaultAutoCaptureFrequencyHz
	}
	return cfg.AutoCaptureFrequencyHz
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
	captureFrequencyHz       float32
	extraTags                []string
	sessionActive            bool
	lastDetectionAt          time.Time
	seenDetectionThisSession bool
	// lastPoll* tracks the most recent successful detection poll for stop logs.
	lastPollAt        time.Time
	lastPollBestScore float64
	lastPollSawLabel  bool

	// bandSensors is nil when band_tags is unconfigured, and every use is
	// guarded so the module keeps its original behaviour without it.
	bandSensors map[string]sensor.Sensor

	// bandTags holds the most recently computed bucket tags, refreshed off the
	// poll loop so Readings never blocks on another resource.
	bandTags []string

	// manualSequenceActive is set only by the start_sequence / stop_sequence
	// commands. Capture sessions never open a Sequence on their own.
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

	if len(conf.BandTags) > 0 {
		s.bandSensors = make(map[string]sensor.Sensor, len(conf.BandTags))
		for _, b := range conf.BandTags {
			if _, ok := s.bandSensors[b.Sensor]; ok {
				continue
			}
			sens, err := sensor.FromDependencies(deps, b.Sensor)
			if err != nil {
				cancelFunc()
				return nil, fmt.Errorf("band_tags sensor %q: %w", b.Sensor, err)
			}
			s.bandSensors[b.Sensor] = sens
		}
	}

	if s.bandSensors != nil {
		go s.pollBandTags()
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
	freq := s.captureFrequencyHz
	tags := toAny(s.currentTagsLocked())
	seqActive := s.manualSequenceActive
	seqTags := toAny(s.sequenceTags)
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

	out := map[string]interface{}{"overrides": overrides}
	// The sequences key is sent only while a manual sequence is open. The Data
	// Manager treats a missing key exactly like an empty list -- it closes any
	// open Sequence -- so leaving it out is also how stop_sequence takes effect.
	if seqActive {
		resources := make([]interface{}, 0, len(s.cfg.Resources))
		for _, r := range s.cfg.Resources {
			resources = append(resources, map[string]interface{}{
				"resource_name": r.ResourceName,
				"method":        r.Method,
			})
		}
		out["sequences"] = []interface{}{map[string]interface{}{
			"resources":     resources,
			"sequence_tags": seqTags,
		}}
	}
	return out, nil
}

// DoCommand exposes four commands. Each is selected by setting its key to true;
// optional arguments are read from sibling keys on the same map.
//
//	{"start_capture": true, "frequency_hz": 2.0, "tags": ["boat-a"]}  // opens a session
//	{"stop_capture":  true}                                           // closes it, freq=0
//	{"start_sequence": true, "tags": ["boat-a"]}  // manual only; capture is unchanged
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
		s.mu.Unlock()

		s.logger.Infof("start_capture frequency_hz=%v tags=%v", freq, tags)
		return map[string]interface{}{
			"capture_frequency_hz": freq,
			"tags":                 toAny(tags),
		}, nil

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
func (s *sonarCaptureControlCaptureControl) startSessionLocked(freq float32, extraTags []string, fromDetection bool) {
	now := s.now()
	s.captureFrequencyHz = freq
	s.extraTags = append([]string(nil), extraTags...)
	s.sessionActive = true
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
	s.captureFrequencyHz = 0
	s.sessionActive = false
	s.lastDetectionAt = time.Time{}
	s.seenDetectionThisSession = false
	s.extraTags = append([]string(nil), s.cfg.DefaultTags...)
	if wasActive {
		s.logger.Infof("stop_capture reason=%s", reason)
	} else {
		s.logger.Infof("stop_capture reason=%s (already idle)", reason)
	}
}

// pollBandTags refreshes band tags on its own loop. Readings() is called by the
// data manager on every capture tick and must not block on other resources, so
// the tags it reports are whatever this loop last cached. A read that fails
// leaves the previous tags in place rather than clearing them: a transient
// failure should not strip tags mid-session.
func (s *sonarCaptureControlCaptureControl) pollBandTags() {
	ticker := time.NewTicker(s.cfg.detectionPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-s.cancelCtx.Done():
			return
		case <-ticker.C:
			if tags, ok := s.readBandTags(s.cancelCtx); ok {
				s.mu.Lock()
				s.bandTags = tags
				s.mu.Unlock()
			}
		}
	}
}

// readBandTags reads each configured sensor once and buckets the named
// readings. Returns false only when every sensor failed, so a partial read
// still updates what it can.
func (s *sonarCaptureControlCaptureControl) readBandTags(ctx context.Context) ([]string, bool) {
	cache := make(map[string]map[string]interface{}, len(s.bandSensors))
	anyOK := false
	for name, sens := range s.bandSensors {
		r, err := sens.Readings(ctx, nil)
		if err != nil {
			s.logger.Debugf("band_tags sensor %q read failed: %v", name, err)
			continue
		}
		cache[name] = r
		anyOK = true
	}
	if !anyOK {
		return nil, false
	}
	out := make([]string, 0, len(s.cfg.BandTags))
	for _, b := range s.cfg.BandTags {
		r, ok := cache[b.Sensor]
		if !ok {
			continue
		}
		v, ok := readingFloat(r, b.Reading)
		if !ok {
			continue
		}
		out = append(out, b.format(v))
	}
	return out, true
}

// readingFloat pulls a numeric value out of a Readings map, tolerating the
// nested {"readings": {...}} shape that several sensors emit.
func readingFloat(r map[string]interface{}, key string) (float64, bool) {
	if v, ok := asFloat64(r[key]); ok {
		return v, true
	}
	if nested, ok := r["readings"].(map[string]interface{}); ok {
		if v, ok := asFloat64(nested[key]); ok {
			return v, true
		}
	}
	return 0, false
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
			freq := s.cfg.autoCaptureFrequency()
			s.startSessionLocked(freq, append([]string(nil), s.cfg.DefaultTags...), true)
			s.logger.Infof("auto start_capture frequency_hz=%v score=%.3f label=%s",
				freq, out.bestScore, s.cfg.detectionLabel())
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

func matchingDetection(dets []objectdetection.Detection, label string, minScore float64) bool {
	return evaluateDetections(dets, label, minScore).found
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
	scoreInfo := "none (label not present on last poll)"
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

// currentTagsLocked returns the merged tags applied to captured data.
// Caller must hold s.mu.
func (s *sonarCaptureControlCaptureControl) currentTagsLocked() []string {
	if !s.sessionActive {
		return append([]string(nil), s.extraTags...)
	}
	out := make([]string, 0, len(s.extraTags)+len(s.bandTags))
	out = append(out, s.extraTags...)
	out = append(out, s.bandTags...)
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
