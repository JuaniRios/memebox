# memebox

A self-hosted library for your own memes, GIFs, stickers and clips. It
converts each one, on demand, into the format a chat app wants. It is a
web app (PWA), so it runs on iPhone, Mac, Windows, Android and Linux, and
you can install it to the home screen or dock.

- **Add** by dropping files, pasting an image, choosing files, sharing to it
  (Android, and Chrome or Edge on desktop once installed), pasting a GIPHY,
  Tenor or direct link, or with an iOS Shortcut. HEIC photos and stickers
  lifted from iOS Photos work too.
- **Find** by name, tag, and type. The memes you send most often stay on top.
- **Send as** any of these formats:

| Format | Output | Use it for |
| --- | --- | --- |
| WhatsApp sticker | 512×512 WebP, ≤100 KB still / ≤500 KB animated, ≤8 s | WhatsApp |
| Telegram sticker | WebP (still) or VP9 WebM ≤3 s, ≤256 KB, 512 px side | Telegram, @Stickers |
| GIF | ≤480 px, ≤8 MB | Zulip, Slack, Discord, iMessage, anything |
| Video (MP4) | H.264, keeps sound | WhatsApp and Telegram "GIFs", clips |
| PNG image | still frame | copy and paste anywhere |
| Custom emoji | 128×128 PNG/GIF ≤256 KB | Zulip, Slack, Discord emoji |
| Discord sticker | 320×320 PNG/APNG ≤512 KB | Discord server stickers |

  Each export can add classic top and bottom captions, fit inside or crop to
  fill a square, and trim moving ones. **Save as new meme** keeps the
  captioned version in the library.

- **Share** from the result with the system share sheet (iPhone, Mac Safari,
  Windows Edge and Chrome), **Copy image** (PNG, pastes into Zulip, Slack,
  WhatsApp Web, Telegram Desktop), or **Download**.

## Getting a meme into each app

- **Zulip:** use **GIF** or **PNG**, then paste (PNG) or drop or upload the
  file in the compose box. For a custom emoji, use **Custom emoji**, then go
  to *Settings → Custom emoji → Add*.
- **WhatsApp:** on WhatsApp Web or Desktop, drop or paste a **WhatsApp
  sticker** into the chat and it is sent as a sticker. On a phone, share the
  WebP to a sticker-maker app to build a pack. Use **Video (MP4)** to send a
  GIF.
- **Telegram:** share or drop the **Telegram sticker** file. To add it to
  your own pack, send it to [@Stickers](https://t.me/stickers) (`/newpack`
  or `/newvideo`).
- **iMessage, Slack, Discord, anything else:** **GIF** or **PNG** through
  Share, Copy or Download.

## Running it

It needs `ffmpeg` (with libwebp, libvpx and x264) and ImageMagick on
`PATH`. For captions, also set `MEMEBOX_FONT` to a bold `.ttf` file.

| Variable | Default | Meaning |
| --- | --- | --- |
| `MEMEBOX_ADDR` | `127.0.0.1:8790` | listen address |
| `MEMEBOX_DATA` | `./data` | library directory (index.json + media) |
| `MEMEBOX_PASSWORD` / `MEMEBOX_PASSWORD_FILE` | unset | shared login password; unset means open |
| `MEMEBOX_FONT` | unset | caption font |
| `MEMEBOX_MAX_UPLOAD_MB` | `200` | per-file upload limit |

The Web Share and Clipboard APIs only work over **HTTPS** (or localhost),
so put it behind Tailscale Serve, Caddy, or another TLS proxy.

### Nix / NixOS

```nix
# flake inputs
memebox.url = "github:JuaniRios/memebox";

# NixOS configuration
imports = [ inputs.memebox.nixosModules.default ];
services.memebox = {
  enable = true;
  # passwordFile = "/run/secrets/memebox";  # optional
  tailscaleServe = { enable = true; httpsPort = 10000; };  # https://<host>.<tailnet>.ts.net:10000
};
```

To try it without installing: `nix run github:JuaniRios/memebox`. For
development, `nix develop` gives you Go, ffmpeg, ImageMagick and the
caption font, then run `go run .`.

### Docker

```sh
docker build -t memebox .
docker run -d -p 8790:8790 -v memebox-data:/data -e MEMEBOX_PASSWORD=change-me memebox
```

Put a TLS proxy in front, or run a `tailscale/tailscale` sidecar with
`tailscale serve`.

## Installing the app

- **iPhone / iPad:** open the URL in Safari, then *Share → Add to Home
  Screen*.
- **Mac:** Safari *File → Add to Dock*, or the install icon in Chrome's
  address bar.
- **Windows:** Edge or Chrome, then *Install memebox* from the address bar
  menu. Once installed, memebox shows up in the Windows share sheet.

### iOS Shortcut: "Save to memebox" from the share sheet

iOS does not let web apps receive shares, so a Shortcut does it:

1. In Shortcuts, create a new shortcut. In its details, turn on **Show in
   Share Sheet** and accept **Images, Media and Files**.
2. Add **Get Contents of URL**:
   - URL: `https://<your-host>/api/items?name=shared`
   - Method: **POST**, Request Body: **File**, File: **Shortcut Input**
   - Only with a password: add the header `Authorization` = `Bearer <password>`
3. Optionally add **Show Notification** "Saved to memebox".

Now *Share → Save to memebox* works from Photos, Safari, WhatsApp and
Telegram.

## API

| Method & path | Does |
| --- | --- |
| `GET /api/items` | list items (most recently used first) |
| `POST /api/items` | multipart upload (`file` parts, optional `tags`), or a raw body with `?name=&tags=` |
| `POST /api/import` | `{"url": "...", "tags": [...]}` |
| `PATCH /api/items/{id}` | `{"name": "...", "tags": [...]}` |
| `DELETE /api/items/{id}` | delete |
| `GET /api/items/{id}/export/{preset}` | convert; query `fit`, `top`, `bottom`, `start`, `dur`, `download=1` |
| `POST /api/items/{id}/export/{preset}` | convert and save the result as a new item |

Presets: `whatsapp`, `telegram`, `gif`, `mp4`, `png`, `emoji`,
`discord-sticker`. Send `Authorization: Bearer <password>` when a password
is set.
