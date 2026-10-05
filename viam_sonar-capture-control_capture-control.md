# Model viam:sonar-capture-control:capture-control

Sensor that drives Data Manager capture via `overrides`. While a capture session runs, the camera and every sensor capture together once every 6 seconds, and each capture is wrapped in its own 1.5-second sequence: one image plus one set of readings. Sessions start when `downsonar` (or another label) is detected, or by DoCommand.

## Configuration

```json
{
  "resources": [
    {"resource_name": "HDMICapture-filtered", "method": "GetImages"},
    {"resource_name": "garmin", "method": "Position"},
    {"resource_name": "garmin", "method": "CompassHeading"},
    {"resource_name": "depth", "method": "Readings"},
    {"resource_name": "seatemp", "method": "Readings"}
  ],
  "default_tags": [],
  "inactivity_seconds": 10,
  "vision": "sonar-finder-service",
  "camera": "HDMICapture",
  "detection_label": "downsonar",
  "detection_confidence": 0.8,
  "detection_poll_hz": 1
}
```

### Attributes

| Name | Type | Inclusion | Description |
|------|------|-----------|-------------|
| `resources` | array | Required | Resource/method pairs to control under overrides. At least one must be an image (`GetImages` or `GetImage`) |
| `default_tags` | string[] | Optional | Tags added to every captured image and every sequence |
| `inactivity_seconds` | float | Optional | Auto-stop after this gap **only if** the session has seen ≥1 detection (default 10). Manual-only sessions are not auto-stopped |
| `vision` | string | Optional | Vision service for auto start/stop. Omit for manual-only |
| `camera` | string | Required if `vision` set | Camera name passed to `DetectionsFromCamera` |
| `detection_label` | string | Optional | Class label to match, case-insensitive (default `downsonar`) |
| `detection_confidence` | float | Optional | Minimum score (default `0.8`) |
| `detection_poll_hz` | float | Optional | Background vision poll rate (default `1`) |

When `vision` is set, Validate requires `camera` and both are returned as required dependencies. Ensure they exist on the same part as `capture-sensor`. No change to Data Manager `capture_control_sensor` wiring.

## Behavior

- Idle → nothing is captured
- Idle + matching detection → session starts
- During a session, every resource captures once every 6 s, all at the same moment (session start + 6 s, 12 s, ...)
- Each capture gets its own sequence, open from 0.75 s before it to 0.75 s after: one image plus one set of readings. No sequence is open in between
- Active + detections → refreshes last-seen
- After ≥1 detection, gap ≥ `inactivity_seconds` → auto-stop
- Manual `start_capture` with zero detections → stays on until `stop_capture`
- Manual start, then a detection, then inactivity → stops

## DoCommand

Start and stop a session by hand. Same behavior as a detection-started session.

```json
{"start_capture": true, "tags": ["boat-a"]}
```

```json
{"stop_capture": true}
```

Open and close one extra sequence by hand, on top of the per-capture ones. Doesn't change capture.

```json
{"start_sequence": true, "tags": ["boat-a"]}
```

```json
{"stop_sequence": true}
```

## Publishing 0.3.0

Publish for the boat (typically `linux/arm64`):

```bash
viam module build start --version 0.3.0 --from-source --wait --platforms linux/arm64
```

Then pin the machine’s module version to `0.3.0`. On `capture-sensor`, remove `band_tags`, `auto_capture_frequency_hz` and `default_capture_frequency_hz` if present; they no longer do anything.
