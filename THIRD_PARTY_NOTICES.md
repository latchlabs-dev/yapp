# Third-party notices

Yapp's own project license is pending. These notices preserve existing dependency
licenses; they do not grant a license to Yapp as a whole.

## Vendored Duffel source

`third_party/duffel` is a subset of the Duffel Go module, maintained at
https://github.com/jibs/duffel. It retains its MIT license and copyright notice in
[third_party/duffel/LICENSE](third_party/duffel/LICENSE). The copy includes local
adaptations; it is not represented as an unmodified upstream release.

## Dependencies and assets

Go dependencies are declared in `go.mod`/`go.sum`. The admin UI, optional web console
and terminal extension use their own `package.json` and pnpm lockfiles. The optional
query service uses `pyproject.toml` and `uv.lock`. Dependency license terms continue
to apply to their respective components. Release rehearsals include an inventory
and copies of locally available dependency license notices.
The inventory includes locked npm and Python packages even when their license
texts are unavailable locally; these are marked `license_text_status=not_collected`.
Completing the dependency license audit remains a distribution gate.

UI icons come from the declared `lucide-react` dependency. The UI requests Inter,
Space Grotesk and JetBrains Mono from Google Fonts at runtime; font binaries are not
vendored in this source tree. Operators can replace those external font requests
with their own appropriately licensed assets or system-font fallbacks.

The README screenshots were generated from this application's isolated synthetic
fixture; they contain no third-party customer data or stock photographs.
