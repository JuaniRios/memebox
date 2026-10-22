package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type probeResult struct {
	Kind     Kind
	Width    int
	Height   int
	Duration float64
	HasAudio bool
}

// imageFormats are ffprobe demuxers that hold pictures rather than video
// containers. Anything with more than one frame in them is "animated".
var imageFormats = map[string]bool{
	"gif": true, "apng": true, "image2": true, "webp_anim": true,
}

func isImageFormat(name string) bool {
	return imageFormats[name] || strings.HasSuffix(name, "_pipe")
}

func probe(ctx context.Context, path string) (probeResult, error) {
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-count_packets",
		"-show_streams", "-show_format", "-of", "json", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return probeResult{}, fmt.Errorf("ffprobe: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	var data struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Packets   string `json:"nb_read_packets"`
			Duration  string `json:"duration"`
		} `json:"streams"`
		Format struct {
			Name     string `json:"format_name"`
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return probeResult{}, err
	}
	var r probeResult
	packets := 0
	found := false
	for _, st := range data.Streams {
		switch st.CodecType {
		case "video":
			if found {
				continue
			}
			found = true
			r.Width, r.Height = st.Width, st.Height
			packets, _ = strconv.Atoi(st.Packets)
		case "audio":
			r.HasAudio = true
		}
	}
	if !found || r.Width == 0 || r.Height == 0 {
		return probeResult{}, errors.New("no picture found in file")
	}
	r.Duration, _ = strconv.ParseFloat(data.Format.Duration, 64)
	format := strings.Split(data.Format.Name, ",")[0]
	switch {
	case isImageFormat(format) && packets <= 1:
		r.Kind = KindStill
		r.Duration = 0
	case isImageFormat(format):
		r.Kind = KindAnimated
	case packets <= 1:
		r.Kind = KindStill
		r.Duration = 0
	default:
		r.Kind = KindVideo
	}
	if r.Kind != KindVideo {
		r.HasAudio = false
	}
	return r, nil
}

// needsMagick lists formats ffmpeg reads poorly or not at all. iPhones hand
// these out (HEIC photos, stickers lifted from Photos), so they are turned
// into PNG on the way in.
var needsMagick = map[string]bool{"heic": true, "heif": true, "avif": true, "tif": true, "tiff": true, "bmp": true, "svg": true}

var mimeExt = map[string]string{
	"image/gif": "gif", "image/png": "png", "image/apng": "png", "image/jpeg": "jpg",
	"image/webp": "webp", "image/heic": "heic", "image/heif": "heif", "image/avif": "avif",
	"video/mp4": "mp4", "video/quicktime": "mov", "video/webm": "webm", "image/bmp": "bmp",
	"image/svg+xml": "svg", "image/tiff": "tiff", "video/x-matroska": "mkv",
}

func extFor(filename, contentType string, head []byte) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	if ext == "jpeg" {
		ext = "jpg"
	}
	if ext != "" && len(ext) <= 5 {
		return ext
	}
	ct := strings.Split(contentType, ";")[0]
	if e, ok := mimeExt[ct]; ok {
		return e
	}
	if e, ok := mimeExt[http.DetectContentType(head)]; ok {
		return e
	}
	return "bin"
}

type Media struct {
	store     *Store
	maxUpload int64
}

// Ingest saves an upload into the library: it sniffs the format, converts
// what ffmpeg cannot read, probes size and animation, and renders the grid
// preview.
func (m *Media) Ingest(ctx context.Context, r io.Reader, filename, contentType string, tags []string) (Item, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	tmpDir, err := os.MkdirTemp(m.store.TmpDir(), "ingest-")
	if err != nil {
		return Item{}, err
	}
	defer os.RemoveAll(tmpDir)

	head := make([]byte, 512)
	n, _ := io.ReadFull(r, head)
	head = head[:n]
	ext := extFor(filename, contentType, head)
	src := filepath.Join(tmpDir, "upload."+ext)
	f, err := os.Create(src)
	if err != nil {
		return Item{}, err
	}
	written, err := io.Copy(f, io.LimitReader(io.MultiReader(bytes.NewReader(head), r), m.maxUpload+1))
	f.Close()
	if err != nil {
		return Item{}, err
	}
	if written > m.maxUpload {
		return Item{}, fmt.Errorf("file is larger than %d MB", m.maxUpload>>20)
	}
	if written == 0 {
		return Item{}, errors.New("empty file")
	}

	info, err := probe(ctx, src)
	if needsMagick[ext] || err != nil {
		png := filepath.Join(tmpDir, "converted.png")
		out, merr := exec.CommandContext(ctx, "magick", src+"[0]", "-auto-orient", png).CombinedOutput()
		if merr != nil {
			if err == nil {
				err = merr
			}
			log.Printf("ingest %s: ffprobe: %v; magick: %v: %s", filename, err, merr, strings.TrimSpace(string(out)))
			return Item{}, errors.New("not an image or video this app can read")
		}
		src, ext = png, "png"
		if info, err = probe(ctx, src); err != nil {
			return Item{}, err
		}
	}

	name := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	if name == "" || name == "." || name == "image" || name == "blob" {
		name = "meme"
	}
	it := Item{
		ID: newID(), Name: name, Tags: normalizeTags(tags), Kind: info.Kind, Ext: ext,
		Width: info.Width, Height: info.Height, Duration: info.Duration, HasAudio: info.HasAudio,
		Created: time.Now().UTC(),
	}
	if err := renderPreview(ctx, src, it, m.store.PreviewPath(it.ID)); err != nil {
		return Item{}, err
	}
	dst := m.store.OriginalPath(&it)
	if err := moveFile(src, dst); err != nil {
		return Item{}, err
	}
	if st, err := os.Stat(dst); err == nil {
		it.Size = st.Size()
	}
	if err := m.store.Add(it); err != nil {
		os.Remove(dst)
		return Item{}, err
	}
	return it, nil
}

// renderPreview makes the small grid thumbnail. Moving items stay moving
// (animated WebP, at most 6 seconds) so the library reads like a GIF picker.
func renderPreview(ctx context.Context, src string, it Item, dst string) error {
	scale := "scale='min(iw,360)':'min(ih,360)':force_original_aspect_ratio=decrease"
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src}
	if it.Kind == KindStill {
		args = append(args, "-vf", scale, "-frames:v", "1", "-c:v", "libwebp", "-q:v", "75")
	} else {
		args = append(args, "-t", "6", "-vf", "fps=12,"+scale, "-an", "-c:v", "libwebp_anim",
			"-q:v", "55", "-loop", "0")
	}
	args = append(args, "-f", "webp", dst)
	return runFFmpeg(ctx, args)
}

func runFFmpeg(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return fmt.Errorf("ffmpeg: %v: %s", err, msg)
	}
	return nil
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
