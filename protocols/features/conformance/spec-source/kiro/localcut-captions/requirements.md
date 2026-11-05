# Requirements: Phase 30 — Animated Caption Styles

## R1 — Caption data model

- **R1.1** `CaptionTrack` extends the project model with an ordered list of `CaptionLine { id, range: CMTimeRange, text, words: [WordTiming]? }` plus a `CaptionStyle?` per line and a track-level default style.
- **R1.2** `WordTiming { range: CMTimeRange, word: String }` is OPTIONAL; absence means full-line rendering only.
- **R1.3** Caption tracks survive project save/load and bundle export (depends on `feature-project-persistence`).
- **R1.4** SRT and VTT importers populate line text + range; word timings remain `nil` (filled later by Phase 29).

## R2 — Style engine

- **R2.1** `CaptionStyle` covers font (PostScript name + size — weight is encoded in the name itself; `CTFontCreateWithName` doesn't take a separate weight), fill colour, stroke colour + width, shadow (offset + blur + colour), glow (colour + radius), background pill (colour + corner radius + padding), enter/exit animations + durations.
- **R2.2** Every style parameter has a documented default; missing values fall back to track defaults, then to a built-in identity style.
- **R2.3** A `CaptionStyle` is keyframable on the line for `fillColour`, `scale`, `offset`, `opacity`, `letterSpacing`.

## R3 — Animations

- **R3.1** Ship at least four enter animations (pop, bounce, slide, typewriter) and a matching exit animation set.
- **R3.2** Animations evaluate deterministically from `(currentTime - lineStart)`; identical inputs yield identical pixels.
- **R3.3** Word-level highlight activates only when `words` is non-nil; it must not desync with the underlying transcript by more than one frame at the project's fps.
- **R3.4** Typewriter reveal masks only glyph/text pixels; background pills remain visible for the whole line duration.

## R4 — Preset library

- **R4.1** Ship ≥10 built-in presets covering social, news/explainer, cinematic, and karaoke styles.
- **R4.2** Versioned JSON `CaptionPresetV1` with stable field names; future versions migrate forward.
- **R4.3** Users can import and export `.lccaption` files via `.fileImporter` and `NSSavePanel`; no network calls.

## R5 — Render path

- **R5.1** Caption rasters render through the same `EffectCompositor` used for clips; preview and export are pixel-identical.
- **R5.2** Idle frames cache by `(lineId, styleHash)`; cache evicts when the line, the style, or the project render size changes.
- **R5.3** 1080p preview stays realtime with up to two concurrent caption lines on an Apple Silicon Mac with hardware acceleration on; degrade gracefully at higher counts (frame drops, never a hang).

## R6 — Sidecar export

- **R6.1** Burn-in export is the default; styled captions appear in the rendered video.
- **R6.2** SRT/VTT sidecar export remains plain text; the UI states that styling is preview/burn-in only.

## R7 — Verification

- **R7.1** Unit tests for preset JSON round-trip, style hashing, animation curve evaluation, word-highlight time-to-index mapping, token-index text range mapping, and typewriter mask geometry.
- **R7.2** Snapshot tests for each built-in preset rendered at a fixed time and project size.
- **R7.3** Smoke test: import SRT → pick a preset → scrub → export `.mov` → exported frames match preview at sampled times.
