[![ci](https://github.com/whiskydumb/yawn/actions/workflows/main.yml/badge.svg)](https://github.com/whiskydumb/yawn/actions/workflows/main.yml)
[![release](https://img.shields.io/github/v/release/whiskydumb/yawn)](https://github.com/whiskydumb/yawn/releases)
[![license](https://img.shields.io/github/license/whiskydumb/yawn)](LICENSE)

# yawn

A YouTube channel archiver that moves videos into storage you control. Not a
downloader but a pipeline with state: it downloads, uploads and deletes as it
goes, so it can mirror an archive far larger than the disk it runs on. Stop it
at any point and the next run picks up where it left off.

## Install

```bash
go install github.com/whiskydumb/yawn/cmd/yawn@latest
```

Or grab a binary from [releases](https://github.com/whiskydumb/yawn/releases).
`yt-dlp` and `ffmpeg` have to be on PATH; the [container image](#docker) ships
with both.

## Usage

```bash
yawn init                                    # write yawn.yaml
yawn add https://www.youtube.com/@channel    # expand a channel into the queue
yawn run                                     # download, upload, free the space
```

`yawn tui` does the same with a live dashboard. Ctrl+C, or `q` in the
dashboard, stops cleanly: workers finish the current step and hand their tasks
back.

### Commands

| | |
|---|---|
| `add <url\|file>` | queue a channel, playlist, video or link list |
| `sync` | catch up on every source, then work the queue |
| `run` | work the queue |
| `tui` | the same with a dashboard |
| `import <dir>` | register videos already on disk as downloaded |
| `status [--failed]` | queue counters and failure reasons |
| `retry [id]` | put failed tasks back |
| `stores` / `auth [name]` | check and authorise storage |

### Storage

A video can go to several stores at once, and the local copy is removed only
once every one of them has it.

```yaml
stores:
  - name: archive
    type: s3                       # local, s3 or drive
    layout: "{uploader}/{upload_year}/{upload_date} - {title} [{id}]"
    options:
      endpoint: https://<account>.r2.cloudflarestorage.com
      bucket: yawn-archive
      access_key_id: ${R2_ACCESS_KEY_ID}
      secret_access_key: ${R2_SECRET_ACCESS_KEY}
```

`s3` covers AWS, Cloudflare R2, Backblaze B2, Wasabi and MinIO. Credentials
take a `${VAR}` reference so keys stay out of the file. Google Drive needs one
interactive login:

```bash
yawn auth gdrive
```

### Fitting a large archive on a small disk

`max_disk` caps what may sit on disk at once. Downloads pause when the ceiling
is reached and resume as uploads free room, which is what lets 400 GB pass
through 40 GB of space.

```yaml
download:
  quality: "1080"       # a ceiling: the next lower format wins if it is missing
  workers: 2            # more than 2 or 3 raises the ban risk
  rate_limit: "3M"      # for the whole tool, not per worker
  max_disk: 40G
```

### Already have the files

```bash
yawn import --meta-dir ./meta ./archive
```

Finds videos by the `[id]` in their names, checks each one with `ffprobe` and
queues them as downloaded. Nothing is moved: a later run uploads them and
frees the space.

### On a schedule

`sync` is a single pass, so scheduling belongs to the host. Ready made units
live in [`deploy/`](deploy):

```bash
sudo cp deploy/yawn.{service,timer} /etc/systemd/system/
sudo systemctl enable --now yawn.timer
```

### Docker

```bash
docker compose run --rm yawn init
docker compose run --rm --service-ports yawn auth --port 8765
docker compose run --rm yawn add https://www.youtube.com/@channel
docker compose run --rm yawn sync
```

Everything lives in `./data`. The image is rebuilt weekly because YouTube
breaks `yt-dlp` every few weeks.

## Acknowledgements

- [yt-dlp](https://github.com/yt-dlp/yt-dlp) does the extraction, behind an
  interface so it can be replaced later
- [ffmpeg](https://ffmpeg.org) muxes, converts and verifies

## License

MIT
