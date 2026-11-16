# Screenshots Protocol

`go.putnami.dev/protocol/screenshots` is the contract of `screenshots.json`,
the manifest `putnami change screenshots` writes beside the PNG captures of a
web change. A consumer that stores or reads the manifest decodes it against
this module, not against a copy of the writer's types.

The normative artifacts are the JSON Schema
[`schemas/screenshots.json`](schemas/screenshots.json) (its `$id` is
`https://putnami.dev/schemas/putnami-screenshots-v1.json`), the Go decoder,
and the shared corpus under [`fixtures/`](fixtures/).

## Producers and consumers

| Role | Implementation |
| --- | --- |
| Producer | `putnami change screenshots`, served by `@putnami/contributor`, writes the manifest with `Marshal` (`tooling/contributor/internal/screens`). |
| Consumer (in repo) | `putnami change finalize` reads the manifest of each touched web project with `Decode`, then checks it against the head and hashes each capture file. |
| Consumer (out of repo) | A service that stores or shows the screenshots of a change. |

## Wire shape

```json
{
  "protocolVersion": 1,
  "project": "apps/web",
  "baseSHA": "1f0e2d3c4b5a69788796a5b4c3d2e1f001234567",
  "headSHA": "89abcdef0123456789abcdef0123456789abcdef",
  "pages": [
    {
      "file": "src/app/tasks/page.tsx",
      "route": "/tasks",
      "path": "/tasks",
      "before": true,
      "after": true
    }
  ],
  "captures": [
    {
      "file": "01-tasks-before-desktop-light.png",
      "route": "/tasks",
      "side": "before",
      "viewport": "desktop",
      "scheme": "light",
      "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
      "status": 200,
      "url": "http://127.0.0.1:3001/tasks"
    }
  ]
}
```

The example lists one of the eight captures of its page. Every member is
required, and no other member is allowed.

| Member | Rule |
| --- | --- |
| `protocolVersion` | `1`. |
| `project` | The project directory, relative to the workspace root: a normalized slash-separated path, or `.` for a project at the root. |
| `baseSHA`, `headSHA` | The merge base and the head the captures were taken at: full lowercase commit shas of 40 or 64 hexadecimal characters. |
| `pages[].file` | The page file, a normalized path relative to the project. |
| `pages[].route` | The route the page is captured at. It starts with a slash, and no two pages share it. |
| `pages[].path` | The path visited for the route: the route itself, or a template with its parameters filled. It starts with a slash. |
| `pages[].before`, `pages[].after` | Whether the base and the head hold the page. At least one is true. |
| `captures[].file` | A `.png` file name in the manifest's directory, with no directory part. No two captures share it. |
| `captures[].route` | The route of one of the pages. |
| `captures[].side` | `before` or `after`, a side that holds the page. |
| `captures[].viewport` | `desktop`, 1440x900 CSS pixels at device scale 1, or `mobile`, 390x844 at device scale 2. |
| `captures[].scheme` | `light` or `dark`, the preferred color scheme. |
| `captures[].sha256` | The sha256 of the file, 64 lowercase hexadecimal characters. |
| `captures[].status` | The HTTP status of the captured document, from 100 to 599. |
| `captures[].url` | Where the page ended, after any redirect: an absolute `http` or `https` URL, its scheme in lowercase. |

A manifest holds exactly one capture per route, side, viewport and scheme,
for each side that holds the page: four captures for a page one side holds,
eight for a page both hold. A manifest with no page holds no capture.

## Bounds

A writer stays within these bounds, and a consumer may apply them as they
are. The decoder refuses a manifest past one, and `Marshal` refuses to write
one.

| Bound | Value | Go constant |
| --- | --- | --- |
| Pages per manifest | 512 | `MaxPages` |
| Captures per manifest | 4096, eight per page | `MaxCaptures` |
| Bytes of the manifest file | 4 MiB (4 194 304) | `MaxManifestBytes` |
| Bytes of a capture file | 64 MiB (67 108 864) | `MaxCaptureBytes` |
| Bytes of a text member, in UTF-8 | 2048 | `MaxTextBytes` |
| Bytes of a capture file name | 255 | `MaxFileNameBytes` |

The manifest cannot prove the size of a capture file. A reader applies
`MaxCaptureBytes` to each file before it hashes the bytes, and
`putnami change screenshots` fails a run whose capture is larger.

## Decoding

```go
manifest, diags := screenshots.Decode(data)
if manifest == nil {
    return errors.New(diag.ErrorText(diags))
}
```

`Decode` parses strictly, then validates. It returns the manifest only when no
rule refuses it; otherwise each finding carries a stable code and the path of
its member. `Parse` alone applies the shape and the list bounds: one JSON object,
every member present, no unknown member, no member twice, and no more than
`MaxPages` pages or `MaxCaptures` captures, counted before an item is read. Member names match by their
exact case, and a value that nests deeper than a manifest does is refused
before it is read. `Marshal` validates before it
serializes, so a writer that goes through the protocol cannot write a manifest
the decoder refuses.

| Code | Rule |
| --- | --- |
| `screenshots.parse_error` | The file is not one JSON object of the expected types. |
| `screenshots.too_large` | The file is more than `MaxManifestBytes`. |
| `screenshots.unknown_field`, `screenshots.duplicate_field`, `screenshots.missing_field` | A member is unknown, listed twice, or absent or null. |
| `screenshots.text_too_long` | A text member is more than `MaxTextBytes`. |
| `screenshots.invalid_protocol_version` | `protocolVersion` is not 1. |
| `screenshots.invalid_project`, `screenshots.invalid_sha` | `project`, `baseSHA` or `headSHA` breaks its rule. |
| `screenshots.too_many_pages`, `screenshots.too_many_captures` | A list is past its bound. |
| `screenshots.invalid_page_file`, `screenshots.invalid_route`, `screenshots.invalid_path`, `screenshots.invalid_page_sides` | A page member breaks its rule, or neither side holds the page. |
| `screenshots.duplicate_route` | Two pages share a route. |
| `screenshots.invalid_capture_file`, `screenshots.duplicate_capture_file` | A capture file is no `.png` name, or two captures share it. |
| `screenshots.invalid_side`, `screenshots.invalid_viewport`, `screenshots.invalid_scheme` | A value is outside its closed set. |
| `screenshots.invalid_sha256`, `screenshots.invalid_status`, `screenshots.invalid_url` | A capture member breaks its rule. |
| `screenshots.unknown_route`, `screenshots.unexpected_side` | A capture names no page, or a side that does not hold its page. |
| `screenshots.duplicate_capture`, `screenshots.missing_capture` | A route, side, viewport and scheme has two captures, or none. |

## Schema and decoder

The schema and the decoder accept and refuse the same fixtures, except for
the rules a schema cannot express, which the schema description lists: a
member listed twice, trailing data, the byte bounds, a route or a file listed
twice, and the rules that relate a capture to its page. A consumer that
validates with the schema alone applies those rules itself.

`schematest.Check` judges a document against the published schema in a Go
test, without a JSON Schema library. `putnami change screenshots` uses it to
prove that its output validates.

## Support

`preview`, as recorded in the [reviewed support catalog](../../putnami.support.json).
`@putnami/contributor` produces the manifest through `putnami change
screenshots` and reads it in `putnami change finalize`. Stable support still
requires a consumer outside this repository, such as a service that stores the
screenshots of a change, and an explicit compatibility window.

## Versioning

A change to a member, a closed set, a rule or a bound needs a new
`protocolVersion`, a new schema URL and a new corpus. A reader refuses a
version it does not know.
