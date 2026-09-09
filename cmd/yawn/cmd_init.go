package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/whiskydumb/yawn/internal/config"
)

// configTemplate is written by `yawn init`. It is a commented template
// rather than a serialised struct: configs are read by people, and a hint
// next to the line is worth more than generated output.
const configTemplate = `# yawn config

# work directory: files are downloaded here, uploaded from here, then removed
work_dir: downloads
database: yawn.db

extractor:
  # paths to the binaries; empty means look in PATH
  binary: ""
  ffprobe: ""
  # arguments mixed into every extractor call, for example on windows where
  # deno was installed through winget:
  # extra_args: ["--js-runtimes", "deno:C:/path/to/deno.exe"]
  extra_args: []

download:
  # how many videos are fetched at once; more than 2-3 raises the ban risk
  workers: 2

  # target height: 360, 480, 720, 1080, 1440, 2160, or best, or audio.
  # this is a ceiling: if the video has no such format, the next lower one wins
  quality: "1080"

  # raw format selector for when quality is not enough; overrides quality
  format: ""

  # prefer plain https streams over HLS: smaller files that resume by Range
  prefer_direct: true

  container: mp4
  fragments: 4

  # speed ceiling for the whole tool, not per worker; empty means no limit
  rate_limit: ""

  # how much may sit on disk waiting to be uploaded.
  # this is what lets yawn mirror an archive larger than the disk
  max_disk: 20G
  min_free: 10G

  retries: 5
  sleep_requests: 1.5

  subtitles:
    enabled: true
    auto: true
    langs: [en]

  metadata:
    info_json: true
    description: true
    thumbnail: true
    embed: true

# an optional ffmpeg pass between download and upload.
# remux changes the container without touching the streams: fast and lossless.
# encode re-encodes and costs days of CPU on a few hundred hours of video,
# which is why nothing is on by default
convert:
  enabled: false
  # remux | encode | audio
  mode: remux
  container: mkv
  # encode only; a codec this ffmpeg lacks is rejected at startup
  video_codec: ""
  audio_codec: ""
  crf: 0
  preset: ""
  bitrate: ""
  extra_args: []

upload:
  workers: 4

  # upload ceiling for the whole tool, not per worker. empty means no limit
  rate_limit: ""

  # probe the file with ffprobe before uploading.
  # without this a truncated download reaches the archive looking valid
  verify: true

  # remove the local copy once every store has it
  delete_after: true
  retries: 5

identity:
  # one identity, which is enough for most runs
  # exported cookies.txt in netscape format
  cookies_file: ""
  # or a browser name: firefox, chrome, edge
  cookies_from_browser: ""
  # http://user:pass@host:port or socks5://host:port
  proxy: ""
  user_agent: ""

  # or a rotation, which replaces the fields above. an identity that gets a
  # 429 rests and the next video goes out as somebody else. note that cookies
  # alone only spread the per account limits: the address stays the same
  # unless the identity carries a proxy too
  # pool:
  #   - name: home
  #     cookies_file: cookies-home.txt
  #   - name: vps
  #     cookies_file: cookies-vps.txt
  #     proxy: socks5://10.0.0.5:1080

  # how long a refused identity rests, doubling on each refusal
  base_backoff: 1m
  max_backoff: 30m
  # refusals in a row before an identity is retired for this run
  max_strikes: 5

# stores. with an empty list yawn behaves as a plain downloader and files
# stay in work_dir. one video can go to several stores at once, and the local
# copy is removed only once every one of them has it.
stores:
  - name: local
    type: local
    layout: "{uploader}/{upload_year}/{upload_date} - {title} [{id}]"
    options:
      path: D:/yawn-archive
      # refuse to upload once the destination drops below this
      min_free: 5G

  # any S3 compatible bucket: AWS, Cloudflare R2, Backblaze B2, Wasabi, MinIO.
  # never put keys in the file: ${VAR} reads them from the environment, and
  # leaving them empty falls back to AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
  # - name: r2
  #   type: s3
  #   layout: "{uploader}/{upload_year}/{upload_date} - {title} [{id}]"
  #   options:
  #     endpoint: https://<account>.r2.cloudflarestorage.com
  #     region: auto
  #     bucket: yawn-archive
  #     prefix: thenakuname
  #     access_key_id: ${R2_ACCESS_KEY_ID}
  #     secret_access_key: ${R2_SECRET_ACCESS_KEY}
  #     part_size: 64M

  # google drive. create an OAuth client of type "Desktop app" in Google Cloud
  # Console, enable the Drive API, download client_secret.json, then run
  # 'yawn auth drive'. the default scope only grants access to files yawn
  # itself created, which is why it uploads into its own root_folder; set
  # scope: full only if you need to write into a folder somebody else made
  # - name: gdrive
  #   type: drive
  #   layout: "{uploader}/{upload_year}/{upload_date} - {title} [{id}]"
  #   options:
  #     credentials_file: client_secret.json
  #     token_file: .yawn-drive-token.json
  #     root_folder: yawn-archive
  #     chunk_size: 16M
  #     # scope: full
  #     # folder_id: ""
  #     # shared_drive_id: ""

# layout placeholders: {id} {title} {uploader} {upload_date} {upload_year}
# {upload_month} {upload_day}. every slash becomes a directory level, and a
# missing value falls back to "unknown" rather than dropping the level
`

func newInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "write a default config",
		RunE: func(cmd *cobra.Command, args []string) error {
			path := flagConfig
			if path == "" {
				path = config.DefaultFileName
			}
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists; --force overwrites it", path)
			}
			if err := os.WriteFile(path, []byte(configTemplate), 0o644); err != nil {
				return fmt.Errorf("cannot write %s: %w", path, err)
			}
			fmt.Printf("wrote %s, adjust the store paths and run `yawn add <url>`\n", path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")
	return cmd
}
