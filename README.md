# Module sonar-capture-control

Drives Viam Data Manager capture (via `capture_control_sensor`). While `downsonar` is detected (or after a manual DoCommand start), it captures one image and one set of sensor readings every 6 seconds, each wrapped in its own 1.5-second sequence.

## Models

- [`viam:sonar-capture-control:capture-control`](viam_sonar-capture-control_capture-control.md) — capture-control sensor with capture overrides, per-capture sequences and optional vision poller

## Quick config (auto start/stop on downsonar)

```json
"vision": "sonar-finder-service",
"camera": "HDMICapture",
"detection_label": "downsonar",
"detection_confidence": 0.8,
"detection_poll_hz": 1,
"inactivity_seconds": 10
```

Omit `vision` to keep manual-only behavior. Publish with e.g. `viam module build start --version 0.3.0 --from-source --wait --platforms linux/arm64`.
