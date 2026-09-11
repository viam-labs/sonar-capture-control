# Module sonar-capture-control

Drives Viam Data Manager capture (via `capture_control_sensor`) with session/chunk Sequences. Manual DoCommand in Phase 1; Phase 2 adds optional vision auto start/stop on `downsonar` detections.

## Models

- [`viam:sonar-capture-control:capture-control`](viam_sonar-capture-control_capture-control.md) — capture-control sensor with overrides/sequences and optional vision poller

## Quick config (Phase 2 auto)

```json
"vision": "sonar-finder-service",
"camera": "HDMICapture",
"detection_label": "downsonar",
"detection_confidence": 0.8,
"detection_poll_hz": 1,
"auto_capture_frequency_hz": 1,
"inactivity_seconds": 10
```

Omit `vision` to keep manual-only behavior. Publish with e.g. `viam module build start --version 0.2.0 --from-source --wait --platforms linux/arm64`.
