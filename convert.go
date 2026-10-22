package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ExportOptions struct {
	Fit    string  `json:"fit"` // "contain" (pad with transparency) or "cover" (crop)
	Top    string  `json:"top"`
	Bottom string  `json:"bottom"`
	Start  float64 `json:"start"`
	Dur    float64 `json:"dur"`
}

type Preset struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	Hint    string  `json:"hint"`
	Kinds   []Kind  `json:"kinds"`
	UsesFit bool    `json:"usesFit"`
	MaxDur  float64 `json:"maxDuration,omitempty"`
}

var allKinds = []Kind{KindStill, KindAnimated, KindVideo}
var movingKinds = []Kind{KindAnimated, KindVideo}

var Presets = []Preset{
	{ID: "whatsapp", Label: "WhatsApp sticker", UsesFit: true, Kinds: allKinds, MaxDur: 8,
		Hint: "512×512 WebP under WhatsApp's size limits. On WhatsApp Web or Desktop, drop or paste it into a chat to send it as a sticker. On a phone, share it to a sticker-maker app to add it to a pack."},
	{ID: "telegram", Label: "Telegram sticker", UsesFit: true, Kinds: allKinds, MaxDur: 3,
		Hint: "WebP for stills, 3-second WebM for moving ones. Sent as a file it shows as a sticker; send it to @Stickers to add it to a pack."},
	{ID: "gif", Label: "GIF", Kinds: allKinds, MaxDur: 20,
		Hint: "Plays in Zulip, Slack, Discord, iMessage and Telegram. Up to 480 px and 8 MB."},
	{ID: "mp4", Label: "Video (MP4)", Kinds: movingKinds, MaxDur: 120,
		Hint: "Smallest and sharpest for WhatsApp and Telegram GIFs, and keeps sound from videos."},
	{ID: "png", Label: "PNG image", Kinds: allKinds,
		Hint: "A still frame you can copy and paste into any chat."},
	{ID: "emoji", Label: "Custom emoji", UsesFit: true, Kinds: allKinds, MaxDur: 8,
		Hint: "128×128 PNG or GIF under 256 KB for Zulip, Slack and Discord custom emoji."},
	{ID: "discord-sticker", Label: "Discord sticker", UsesFit: true, Kinds: allKinds, MaxDur: 5,
		Hint: "320×320 PNG or APNG under 512 KB for Discord server stickers."},
}

func presetByID(id string) (Preset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

func (p Preset) Supports(k Kind) bool {
	for _, kk := range p.Kinds {
		if kk == k {
			return true
		}
	}
	return false
}

// plan is a list of encoder settings tried from best to smallest until the
// output fits under limit. Each attempt holds the output-side ffmpeg args.
type plan struct {
	ext      string
	limit    int64
	attempts [][]string
}

type Converter struct {
	store *Store
	font  string
	sem   chan struct{}

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewConverter(store *Store, font string, parallel int) *Converter {
	return &Converter{store: store, font: font, sem: make(chan struct{}, parallel), locks: map[string]*sync.Mutex{}}
}

type ExportResult struct {
	Path      string
	Filename  string
	Size      int64
	Limit     int64
	OverLimit bool
}

const exportVersion = "2"

func (o ExportOptions) normalize() (ExportOptions, error) {
	if o.Fit != "cover" {
		o.Fit = "contain"
	}
	o.Top = strings.TrimSpace(o.Top)
	o.Bottom = strings.TrimSpace(o.Bottom)
	if len(o.Top) > 200 || len(o.Bottom) > 200 {
		return o, fmt.Errorf("captions are limited to 200 characters")
	}
	if o.Start < 0 || o.Dur < 0 || o.Start > 36000 || o.Dur > 36000 || math.IsNaN(o.Start) || math.IsNaN(o.Dur) {
		return o, fmt.Errorf("invalid start or duration")
	}
	return o, nil
}

func (c *Converter) keyLock(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[key]
	if !ok {
		l = &sync.Mutex{}
		c.locks[key] = l
	}
	return l
}

// Export converts an item with a preset and caches the result, so sharing
// the same sticker twice does not run ffmpeg twice.
func (c *Converter) Export(ctx context.Context, it Item, p Preset, o ExportOptions) (ExportResult, error) {
	o, err := o.normalize()
	if err != nil {
		return ExportResult{}, err
	}
	if !p.Supports(it.Kind) {
		return ExportResult{}, fmt.Errorf("%s does not apply to a %s item", p.Label, it.Kind)
	}
	if (o.Top != "" || o.Bottom != "") && c.font == "" {
		return ExportResult{}, fmt.Errorf("captions need a font: set MEMEBOX_FONT to a .ttf file")
	}
	if it.Kind == KindStill {
		o.Start, o.Dur = 0, 0
	}
	if !p.UsesFit {
		o.Fit = "contain"
	}

	pl := c.plan(it, p)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%q|%q|%g|%g", exportVersion, p.ID, o.Fit, o.Top, o.Bottom, o.Start, o.Dur)))
	key := hex.EncodeToString(sum[:])[:16]
	dir := c.store.ExportDir(it.ID)
	out := filepath.Join(dir, p.ID+"-"+key+"."+pl.ext)
	res := ExportResult{Path: out, Filename: safeName(it.Name) + "-" + p.ID + "." + pl.ext, Limit: pl.limit}

	lock := c.keyLock(out)
	lock.Lock()
	defer lock.Unlock()

	if st, err := os.Stat(out); err == nil {
		res.Size = st.Size()
		res.OverLimit = pl.limit > 0 && res.Size > pl.limit
		return res, nil
	}

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return res, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	work, err := os.MkdirTemp(c.store.TmpDir(), "export-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(work)

	captions, err := c.captionFilter(work, it, o)
	if err != nil {
		return res, err
	}

	input := []string{"-hide_banner", "-loglevel", "error", "-y"}
	if o.Start > 0 {
		input = append(input, "-ss", ff(o.Start))
	}
	input = append(input, "-i", c.store.OriginalPath(&it))
	dur := 0.0
	if it.Kind != KindStill {
		dur = o.Dur
		if p.MaxDur > 0 && (dur == 0 || dur > p.MaxDur) {
			dur = p.MaxDur
		}
		if dur > 0 {
			input = append(input, "-t", ff(dur))
		}
	}

	tmp := filepath.Join(work, "out."+pl.ext)
	var size int64
	for i, attempt := range pl.attempts {
		args := append(append([]string{}, input...), shorterOnly(withCaptions(attempt, captions, o.Fit), dur)...)
		args = append(args, tmp)
		if err := runFFmpeg(ctx, args); err != nil {
			return res, err
		}
		st, err := os.Stat(tmp)
		if err != nil {
			return res, err
		}
		size = st.Size()
		if pl.limit == 0 || size <= pl.limit || i == len(pl.attempts)-1 {
			break
		}
	}
	if err := os.Rename(tmp, out); err != nil {
		return res, err
	}
	res.Size = size
	res.OverLimit = pl.limit > 0 && size > pl.limit
	return res, nil
}

// withCaptions substitutes the placeholders each attempt's filter uses: the
// caption chain (or nothing) and the fit mode for boxed presets.
func withCaptions(attempt []string, captions, fit string) []string {
	out := make([]string, len(attempt))
	for i, a := range attempt {
		a = strings.ReplaceAll(a, "{cap}", captions)
		a = strings.ReplaceAll(a, "{box512}", box(512, 512, fit, true))
		a = strings.ReplaceAll(a, "{box320}", box(320, 320, fit, true))
		a = strings.ReplaceAll(a, "{box128}", box(128, 128, fit, true))
		a = strings.ReplaceAll(a, "{tg512}", telegramBox(fit))
		out[i] = a
	}
	return out
}

// shorterOnly keeps an attempt's own -t from lengthening a clip the user
// already trimmed shorter.
func shorterOnly(args []string, dur float64) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-t" && dur > 0 {
			if t, err := strconv.ParseFloat(args[i+1], 64); err == nil && t > dur {
				args[i+1] = ff(dur)
			}
		}
	}
	return args
}

// box scales into a W×H canvas. "contain" letterboxes with transparency,
// "cover" fills the canvas and crops the overflow.
func box(w, h int, fit string, pad bool) string {
	if fit == "cover" {
		return fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase:flags=lanczos,crop=%d:%d", w, h, w, h)
	}
	s := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos", w, h)
	if pad {
		s += fmt.Sprintf(",format=rgba,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black@0", w, h)
	}
	return s
}

// telegramBox makes the longest side exactly 512, which is what the
// @Stickers bot asks for. Even dimensions keep VP9's yuva420p happy.
func telegramBox(fit string) string {
	if fit == "cover" {
		return box(512, 512, "cover", false)
	}
	return "scale=512:512:force_original_aspect_ratio=decrease:force_divisible_by=2:flags=lanczos"
}

func maxSide(n int) string {
	return fmt.Sprintf("scale='min(iw,%d)':'min(ih,%d)':force_original_aspect_ratio=decrease:flags=lanczos", n, n)
}

func gifChain(fps int, scale string) string {
	f := ""
	if fps > 0 {
		f = fmt.Sprintf("fps=%d,", fps)
	}
	return "{cap}" + f + scale +
		",split[a][b];[a]palettegen=reserve_transparent=1:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=4:diff_mode=rectangle:alpha_threshold=128"
}

const flattenWhite = "format=rgba,split[bg][fg];[bg]drawbox=c=white@1:replace=1:t=fill[w];[w][fg]overlay=format=auto,format=yuv420p"

func (c *Converter) plan(it Item, p Preset) plan {
	still := it.Kind == KindStill
	webpStill := func(boxed string, qs ...int) [][]string {
		var a [][]string
		for _, q := range qs {
			a = append(a, []string{"-vf", "{cap}" + boxed, "-frames:v", "1", "-c:v", "libwebp", "-q:v", strconv.Itoa(q), "-f", "webp"})
		}
		return a
	}
	pngStill := func(scale string) [][]string {
		return [][]string{{"-vf", "{cap}" + scale, "-frames:v", "1", "-update", "1", "-c:v", "png", "-f", "image2"}}
	}
	gifSteps := func(scale func(int) string, steps ...[2]int) [][]string {
		var a [][]string
		for _, s := range steps {
			a = append(a, []string{"-vf", gifChain(s[0], scale(s[1])), "-an", "-loop", "0", "-f", "gif"})
		}
		return a
	}

	switch p.ID {
	case "whatsapp":
		if still {
			return plan{"webp", 100 << 10, webpStill("{box512}", 90, 75, 60, 45, 30, 15)}
		}
		var a [][]string
		for _, s := range [][2]int{{15, 75}, {15, 55}, {12, 45}, {10, 35}, {10, 25}, {8, 15}} {
			a = append(a, []string{"-vf", fmt.Sprintf("{cap}fps=%d,{box512}", s[0]), "-an",
				"-c:v", "libwebp_anim", "-q:v", strconv.Itoa(s[1]), "-loop", "0", "-f", "webp"})
		}
		return plan{"webp", 500 << 10, a}

	case "telegram":
		if still {
			return plan{"webp", 512 << 10, webpStill("{tg512}", 90, 75, 60)}
		}
		var a [][]string
		for _, s := range [][2]int{{30, 30}, {30, 36}, {24, 42}, {20, 50}, {15, 58}} {
			a = append(a, []string{"-vf", fmt.Sprintf("{cap}fps=%d,{tg512},format=yuva420p", s[0]), "-an",
				"-c:v", "libvpx-vp9", "-b:v", "0", "-crf", strconv.Itoa(s[1]),
				"-deadline", "good", "-cpu-used", "4", "-row-mt", "1", "-f", "webm"})
		}
		return plan{"webm", 256 << 10, a}

	case "gif":
		if still {
			return plan{"gif", 0, gifSteps(maxSide, [2]int{0, 800})}
		}
		return plan{"gif", 8 << 20, gifSteps(maxSide, [2]int{15, 480}, [2]int{12, 400}, [2]int{10, 320}, [2]int{8, 240})}

	case "mp4":
		var a [][]string
		for _, s := range [][2]int{{720, 23}, {540, 28}, {480, 32}} {
			args := []string{"-vf", fmt.Sprintf("{cap}scale='min(iw,%d)':'min(ih,%d)':force_original_aspect_ratio=decrease:force_divisible_by=2:flags=lanczos,%s", s[0], s[0], flattenWhite),
				"-c:v", "libx264", "-preset", "veryfast", "-crf", strconv.Itoa(s[1]), "-movflags", "+faststart"}
			if it.HasAudio {
				args = append(args, "-c:a", "aac", "-b:a", "128k")
			} else {
				args = append(args, "-an")
			}
			a = append(a, append(args, "-f", "mp4"))
		}
		return plan{"mp4", 15 << 20, a}

	case "png":
		return plan{"png", 0, pngStill(maxSide(2048))}

	case "emoji":
		if still {
			return plan{"png", 256 << 10, pngStill("{box128}")}
		}
		box := func(int) string { return "{box128}" }
		return plan{"gif", 256 << 10, gifSteps(box, [2]int{15, 0}, [2]int{12, 0}, [2]int{10, 0}, [2]int{8, 0}, [2]int{6, 0})}

	case "discord-sticker":
		if still {
			return plan{"png", 512 << 10, pngStill("{box320}")}
		}
		// Full-colour APNG first; busy clips only fit in 512 KB as 256-colour
		// frames, and the last resort also drops to 3 seconds.
		var a [][]string
		for _, fps := range []int{15, 10} {
			a = append(a, []string{"-vf", fmt.Sprintf("{cap}fps=%d,{box320}", fps), "-an",
				"-c:v", "apng", "-pred", "mixed", "-plays", "0", "-f", "apng"})
		}
		for _, s := range [][2]int{{12, 5}, {10, 5}, {8, 5}, {8, 3}} {
			chain := fmt.Sprintf("{cap}fps=%d,{box320},split[a][b];[a]palettegen=reserve_transparent=1:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=3:alpha_threshold=128", s[0])
			a = append(a, []string{"-vf", chain, "-an", "-t", strconv.Itoa(s[1]), "-c:v", "apng", "-plays", "0", "-f", "apng"})
		}
		return plan{"png", 512 << 10, a}
	}
	panic("unknown preset " + p.ID)
}

// captionFilter returns classic top/bottom meme text as drawtext filters
// followed by a comma, or "" without captions. Text goes through files so
// no ffmpeg escaping is needed for what the user typed.
func (c *Converter) captionFilter(work string, it Item, o ExportOptions) (string, error) {
	var parts []string
	for i, text := range []string{o.Top, o.Bottom} {
		if text == "" {
			continue
		}
		size, wrapped := fitCaption(text, it.Width, it.Height)
		file := filepath.Join(work, fmt.Sprintf("caption%d.txt", i))
		if err := os.WriteFile(file, []byte(wrapped), 0o644); err != nil {
			return "", err
		}
		y := "h*0.03"
		if i == 1 {
			y = "h-text_h-h*0.03"
		}
		border := max(2, size/14)
		parts = append(parts, fmt.Sprintf(
			"drawtext=fontfile='%s':textfile='%s':fontsize=%d:fontcolor=white:borderw=%d:bordercolor=black:line_spacing=%d:text_align=C:x=(w-text_w)/2:y=%s",
			c.font, file, size, border, size/8, y))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, ",") + ",", nil
}

// fitCaption picks a font size and wraps words so the caption spans the
// width without running off the frame, shrinking long text down to a floor.
func fitCaption(text string, w, h int) (int, string) {
	words := strings.Fields(text)
	size := max(14, h/8)
	floor := max(10, h/22)
	for {
		perLine := int(float64(w) * 0.94 / (float64(size) * 0.62))
		lines := wrapWords(words, max(perLine, 1))
		longest := 0
		for _, l := range lines {
			longest = max(longest, len([]rune(l)))
		}
		if (len(lines) <= 3 && longest <= perLine) || size <= floor {
			return size, strings.Join(lines, "\n")
		}
		size = max(floor, size*9/10)
	}
}

func wrapWords(words []string, perLine int) []string {
	var lines []string
	cur := ""
	for _, w := range words {
		switch {
		case cur == "":
			cur = w
		case len([]rune(cur))+1+len([]rune(w)) <= perLine:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = strings.Trim(unsafeName.ReplaceAllString(s, "-"), "-.")
	if s == "" {
		return "meme"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func ff(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }
