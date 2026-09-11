# Model viam:sonar-capture-control:capture-control

Sensor that drives Data Manager capture via `overrides` and `sequences`. Supports manual DoCommand start/stop and optional vision-driven auto start/stop when `downsonar` (or another label) is detected.

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
  "default_capture_frequency_hz": 0,
  "default_tags": [],
  "sequence_max_seconds": 60,
  "inactivity_seconds": 10,
  "vision": "sonar-finder-service",
  "camera": "HDMICapture",
  "detection_label": "downsonar",
  "detection_confidence": 0.8,
  "detection_poll_hz": 1,
  "auto_capture_frequency_hz": 1
}
```

### Attributes

| Name | Type | Inclusion | Description |
|------|------|-----------|-------------|
| `resources` | array | Required | Resource/method pairs to control under overrides/sequences |
| `default_capture_frequency_hz` | float | Optional | Frequency until `start_capture` / auto-start (0 = off) |
| `default_tags` | string[] | Optional | Base tags merged into overrides/sequences |
| `sequence_max_seconds` | float | Optional | Chunk rollover while session stays active (default 60) |
| `inactivity_seconds` | float | Optional | Auto-stop after this gap **only if** the session has seen ≥1 detection (default 10). Manual-only sessions are not auto-stopped |
| `vision` | string | Optional | Vision service for auto start/stop. Omit for Phase 1 manual-only |
| `camera` | string | Required if `vision` set | Camera name passed to `DetectionsFromCamera` |
| `detection_label` | string | Optional | Class label to match, case-insensitive (default `downsonar`) |
| `detection_confidence` | float | Optional | Minimum score (default `0.8`) |
| `detection_poll_hz` | float | Optional | Background vision poll rate (default `1`) |
| `auto_capture_frequency_hz` | float | Optional | Frequency used when auto-starting (default `1`) |

When `vision` is set, Validate requires `camera` and both are returned as required dependencies. Ensure they exist on the same part as `capture-sensor`. No change to Data Manager `capture_control_sensor` wiring.

## Behavior

- Idle + matching detection → starts session (`session:` / `chunk:1`) at `auto_capture_frequency_hz`
- Active + detections → refreshes last-seen; 60s chunk rollover unchanged
- After ≥1 detection, gap ≥ `inactivity_seconds` → auto-stop
- Manual `start_capture` with zero detections → stays on until `stop_capture`
- Manual start, then a detection, then inactivity → stops

## DoCommand

```json
{"start_capture": true, "frequency_hz": 1.0, "tags": ["boat-a"]}
```

```json
{"stop_capture": true}
```

```json
{"start_sequence": true, "tags": ["boat-a"]}
```

```json
{"stop_sequence": true}
```

## Publishing 0.2.0

After merging Phase 2, publish for the boat (typically `linux/arm64`):

```bash
viam module build start --version 0.2.0 --from-source --wait --platforms linux/arm64
```

Then pin the machine’s module version to `0.2.0` and add the vision/camera attributes above on `capture-sensor`.
