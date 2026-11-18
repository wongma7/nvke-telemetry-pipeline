# Chart documentation tools

Requires Go as declared in `go.mod`, Helm 4, and Task. Run the following from
the repository root. Task builds the locked chart dependencies before rendering;
the Go command itself requires those dependencies to be present.

After changing images or dependencies, refresh the README chart versions, image
table, and versioned Operator links together:

```bash
task docs:release
task docs:release -- -check
```

The inventory uses effective images in rendered workloads and install hooks.
The dependency table's version column comes from `nktp/Chart.yaml`; chart versions
and image tags remain distinct. Dependency policy, `Chart.lock`, and the
workflow-maintained `THIRD_PARTY_NOTICES.md` remain maintained separately.
Hooks that do not run during installation, including tests and uninstall jobs,
are excluded. It resolves unset Collector and Target Allocator images from the
Operator's rendered image arguments, rather than reading any pinning helper or
assuming versions.
It also renders with metrics-server and blackbox-exporter enabled to include
optional component images. Unused Operator image defaults are not listed.

To also update chart-version references, first set the intended chart version
separately, then supply both `-from` and `-to`. Edit both versions before running:

```bash
OLD_VERSION=0.2.0
NEW_VERSION=0.3.0
task docs:release -- -from "$OLD_VERSION" -to "$NEW_VERSION"
```

The command first resolves the images from chart renders, then uses the resolved
Operator image tag to update versioned Operator source, documentation, and
release-note links in the root and chart READMEs and all Markdown files under
`docs/`. Matching versions in
link labels are updated too; branch names, commit hashes, paths, and anchors are
preserved. A missing, ambiguous, or non-versioned Operator image causes release
preparation to fail before writing. Chart-version references in the root README,
chart README, and blueprint guide change only when `-from` and `-to` are supplied.
Add `-check` to either invocation to report pending changes without writing.

Each run rejects malformed image references, unresolved Operator versions, and
missing or duplicate dependency rows or table markers before writing. It also
rechecks the source files before applying updates to avoid overwriting edits
made during generation.

It preserves release-process examples, dependency versions, and generated image
references. It does not edit `Chart.yaml`, stage files, create commits, or tag a
release.

To run the Go command directly, change to `tools/` and use `go run ./release-docs`,
optionally adding `-from "$OLD_VERSION" -to "$NEW_VERSION"`.
Use `go run ./release-docs -print` to preview only the image table without writing.
