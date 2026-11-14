# media-import

A native Wayland GUI (built with [Gio](https://gioui.org)) that imports photos,
RAW files, videos and audio from a folder, SD card, camera or phone into an
organised destination tree, preserving file dates. It is made for Sway and other
wlroots compositors and runs without XWayland.

## Features

- **Detects connected media**: mounted volumes under `/run/media`, `/media`, `/mnt`
  and gvfs/MTP phones appear as one-click sources and refresh automatically.
- **All common media types**: JPEG/PNG/HEIC/WebP/AVIF…, RAW (CR2/CR3/NEF/ARW/DNG/ORF/RAF…),
  video (MP4/MOV/MKV/MTS/AVI…), audio (MP3/FLAC/WAV/M4A…). Each category can be toggled.
- **Capture date detection**, in order:
  1. EXIF `DateTimeOriginal` (JPEG and TIFF-based RAW)
  2. MP4/MOV/3GP `mvhd` creation time (read directly from the header)
  3. `ffprobe` creation-time tags (optional; for MKV, AVI, HEIC and other formats)
  4. A date in the filename (`IMG_20230514_183012`, `PXL_…`, `VID-20230514-WA…`, `2023-05-14 18.30.12`)
  5. The file's modification time
- **Folder structure presets** (`YYYY/MM/DD`, `YYYY/YYYY-MM-DD`, `YYYY-MM-DD`, `YYYY/MM`,
  `Type/YYYY/MM`, flat…) or a custom template.
- **File naming presets**: keep the original name, or use date/time-based names, or a custom template.
  - Tokens: `{YYYY} {YY} {MM} {MMM} {DD} {hh} {mm} {ss} {kind} {ext} {name} {source}`
- **Preserves timestamps**: the source modification and access times are copied, or the
  modification time can be set to the capture date.
- **Safe copying**: the app writes to a temporary `.part` file, fsyncs it, then renames it into place.
  It never overwrites existing files: a clashing name gets a suffix such as `_1`.
- Skips duplicates (same size and SHA-256), with optional checksum verification,
  **move** mode (the source is deleted only after a successful, verified copy), and a **dry run**.
- Shows progress, speed and ETA, keeps a log, can be cancelled, and sends a desktop notification when it finishes.
- Settings are remembered in `~/.config/media-import/settings.json`.

## Build, test, run, install

Requirements (Arch): `go`, `wayland`, `libxkbcommon`, `mesa` (EGL). `ffmpeg` is optional and provides `ffprobe`.

```sh
make            # build ./build/media-import (incremental)
make run        # build if needed and launch
make test       # unit tests with the race detector
make check      # gofmt check + go vet + tests
make install    # install the last compiled build to ~/.local (bin, .desktop, icon)
make uninstall
make help       # list all targets
```

To install system-wide, build first as your user and then install as root:
`make && sudo make install PREFIX=/usr/local`.

The build uses the Gio tags `nox11 novulkan` (Wayland-only, OpenGL/EGL renderer).
To enable the Vulkan backend, install `vulkan-headers` and run `make TAGS=nox11`.

Set `MEDIA_IMPORT_THEME=light` to use a light theme.

### Sway tip

The Wayland `app_id` is `media-import`, so you can float the window with:

```
for_window [app_id="media-import"] floating enable, resize set 980 900
```

## Layout

```
cmd/media-import      entry point
internal/media        type detection and capture-date extraction (EXIF, MP4, ffprobe, filename)
internal/organize     folder and file-name templates
internal/importer     scan, worker pool, copy/move, dedupe, verify, timestamps
internal/devices      mounted removable media / MTP discovery
internal/ui           Gio interface and in-app folder browser
```
