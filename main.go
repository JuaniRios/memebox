package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func init() {
	// Minimal containers ship no mime.types, and Go's built-in table lacks
	// the video and manifest types this app serves.
	for ext, typ := range map[string]string{
		".webm": "video/webm", ".mp4": "video/mp4", ".mov": "video/quicktime", ".mkv": "video/x-matroska",
		".webp": "image/webp", ".gif": "image/gif", ".png": "image/png", ".heic": "image/heic",
		".webmanifest": "application/manifest+json",
	} {
		mime.AddExtensionType(ext, typ)
	}
}

type App struct {
	store *Store
	media *Media
	conv  *Converter
	auth  *Auth
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func readPassword() (string, error) {
	if f := os.Getenv("MEMEBOX_PASSWORD_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv("MEMEBOX_PASSWORD"), nil
}

func main() {
	addr := env("MEMEBOX_ADDR", "127.0.0.1:8790")
	dataDir := env("MEMEBOX_DATA", "./data")
	maxMB, err := strconv.ParseInt(env("MEMEBOX_MAX_UPLOAD_MB", "200"), 10, 64)
	if err != nil {
		log.Fatalf("MEMEBOX_MAX_UPLOAD_MB: %v", err)
	}
	password, err := readPassword()
	if err != nil {
		log.Fatalf("password: %v", err)
	}
	store, err := OpenStore(dataDir)
	if err != nil {
		log.Fatalf("open data dir: %v", err)
	}
	auth, err := NewAuth(password, dataDir)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	font := os.Getenv("MEMEBOX_FONT")
	if font != "" {
		if _, err := os.Stat(font); err != nil {
			log.Printf("MEMEBOX_FONT %s: %v; captions are off", font, err)
			font = ""
		}
	}
	app := &App{
		store: store,
		media: &Media{store: store, maxUpload: maxMB << 20},
		conv:  NewConverter(store, font, 2),
		auth:  auth,
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           auth.Middleware(app.routes()),
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("memebox on http://%s, data in %s, password %v, captions %v", addr, dataDir, auth.Enabled(), font != "")
	log.Fatal(srv.ListenAndServe())
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sw.js" || r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.loginSubmit)

	mux.HandleFunc("GET /api/config", a.config)
	mux.HandleFunc("GET /api/items", a.listItems)
	mux.HandleFunc("POST /api/items", a.upload)
	mux.HandleFunc("POST /api/import", a.importURL)
	mux.HandleFunc("PATCH /api/items/{id}", a.updateItem)
	mux.HandleFunc("DELETE /api/items/{id}", a.deleteItem)
	mux.HandleFunc("GET /api/items/{id}/export/{preset}", a.export)
	mux.HandleFunc("POST /api/items/{id}/export/{preset}", a.saveExport)
	mux.HandleFunc("GET /media/original/{id}", a.original)
	mux.HandleFunc("GET /media/preview/{id}", a.preview)
	mux.HandleFunc("POST /share-target", a.shareTarget)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (a *App) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"presets": Presets, "captions": a.conv.font != "", "auth": a.auth.Enabled()})
}

func (a *App) listItems(w http.ResponseWriter, r *http.Request) {
	items := a.store.List()
	// Most recently used first, then newest: the memes you send often stay
	// on top without any manual ordering.
	sort.Slice(items, func(i, j int) bool {
		ti, tj := items[i].LastUsed, items[j].LastUsed
		if ti.IsZero() {
			ti = items[i].Created
		}
		if tj.IsZero() {
			tj = items[j].Created
		}
		return ti.After(tj)
	})
	writeJSON(w, 200, items)
}

func splitTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

func (a *App) ingestMultipart(r *http.Request) ([]Item, []string, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, err
	}
	var items []Item
	var failures []string
	var tags []string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return items, failures, err
		}
		if part.FileName() == "" {
			if part.FormName() == "tags" {
				b, _ := io.ReadAll(io.LimitReader(part, 4096))
				tags = splitTags(string(b))
			}
			continue
		}
		it, err := a.media.Ingest(r.Context(), part, part.FileName(), part.Header.Get("Content-Type"), tags)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", part.FileName(), err))
			continue
		}
		items = append(items, it)
	}
	return items, failures, nil
}

func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	// A raw body (an iOS Shortcut "Get contents of URL" with a file) also
	// works, named by the ?name= query parameter.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "meme"
		}
		it, err := a.media.Ingest(r.Context(), r.Body, name, r.Header.Get("Content-Type"), splitTags(r.URL.Query().Get("tags")))
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"items": []Item{it}})
		return
	}
	items, failures, err := a.ingestMultipart(r)
	if err != nil && len(items) == 0 {
		writeErr(w, 400, err.Error())
		return
	}
	if len(items) == 0 {
		msg := "no files in upload"
		if len(failures) > 0 {
			msg = strings.Join(failures, "; ")
		}
		writeErr(w, 400, msg)
		return
	}
	writeJSON(w, 201, map[string]any{"items": items, "failures": failures})
}

func (a *App) shareTarget(w http.ResponseWriter, r *http.Request) {
	items, failures, _ := a.ingestMultipart(r)
	q := url.Values{}
	if len(items) > 0 {
		q.Set("added", strconv.Itoa(len(items)))
		q.Set("open", items[0].ID)
	}
	if len(failures) > 0 {
		q.Set("failed", strings.Join(failures, "; "))
	}
	http.Redirect(w, r, "/?"+q.Encode(), http.StatusSeeOther)
}

var ogMedia = regexp.MustCompile(`(?i)<meta[^>]+(?:property|name)=["'](?:og:video(?::url|:secure_url)?|og:image(?::url|:secure_url)?|twitter:image)["'][^>]*>`)
var contentAttr = regexp.MustCompile(`(?i)content=["']([^"']+)["']`)

// importURL downloads media from a link. For a web page (a GIPHY or Tenor
// page, a tweet) it follows the page's og:video or og:image instead.
func (a *App) importURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL  string   `json:"url"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
		writeErr(w, 400, "send JSON {\"url\": ...}")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	target, pageName := giphyMedia(strings.TrimSpace(body.URL))
	for hop := 0; hop < 3; hop++ {
		resp, err := fetch(ctx, target)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		ct := resp.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "text/html") {
			page, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			next := pickOGMedia(string(page))
			if next == "" {
				next = metaRefresh(string(page))
			}
			if next == "" {
				writeErr(w, 400, "that page has no image or video link")
				return
			}
			base, _ := url.Parse(target)
			ref, err := url.Parse(html.UnescapeString(next))
			if err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			target = base.ResolveReference(ref).String()
			continue
		}
		u, _ := url.Parse(target)
		name := path.Base(u.Path)
		if pageName != "" {
			name = pageName + path.Ext(name)
		}
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
			name = params["filename"]
		}
		it, err := a.media.Ingest(ctx, resp.Body, name, ct, body.Tags)
		resp.Body.Close()
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		writeJSON(w, 201, map[string]any{"items": []Item{it}})
		return
	}
	writeErr(w, 400, "could not find media at that link")
}

var giphyPage = regexp.MustCompile(`^https?://(?:www\.)?giphy\.com/(?:gifs|stickers)/(?:[^/?#]*-)?([A-Za-z0-9]+)/?(?:[?#].*)?$`)
var refreshTag = regexp.MustCompile(`(?i)<meta[^>]+http-equiv=["']refresh["'][^>]*content=["'][^"']*url=([^"']+)["']`)

// giphyMedia turns a GIPHY page link into its GIF file. The pages are
// rendered by script and carry no usable og:image for a plain fetch.
func giphyMedia(raw string) (string, string) {
	if m := giphyPage.FindStringSubmatch(raw); m != nil {
		slug := strings.TrimSuffix(path.Base(strings.SplitN(raw, "?", 2)[0]), "/")
		name := strings.TrimSuffix(strings.TrimSuffix(slug, m[1]), "-")
		return "https://media.giphy.com/media/" + m[1] + "/giphy.gif", name
	}
	return raw, ""
}

func metaRefresh(page string) string {
	if m := refreshTag.FindStringSubmatch(page); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// pickOGMedia prefers a video (GIPHY and Tenor pages offer an MP4) over the
// still preview image.
func pickOGMedia(page string) string {
	var image string
	for _, tag := range ogMedia.FindAllString(page, -1) {
		m := contentAttr.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		if strings.Contains(strings.ToLower(tag), "og:video") {
			return m[1]
		}
		if image == "" {
			image = m[1]
		}
	}
	return image
}

func fetch(ctx context.Context, raw string) (*http.Response, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("enter an http(s) link")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; memebox)")
	client := &http.Client{Transport: &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: 15 * time.Second, Control: publicOnly}).DialContext,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("the link returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

var cgnat = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// publicOnly stops link imports from reaching the host itself, the LAN or
// the tailnet, so a pasted link cannot read private services.
func publicOnly(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() ||
		ip.IsMulticast() || cgnat.Contains(ip) {
		return fmt.Errorf("links to private addresses are not allowed")
	}
	return nil
}

func (a *App) updateItem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name *string   `json:"name"`
		Tags *[]string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	it, err := a.store.Update(r.PathValue("id"), func(it *Item) {
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			it.Name = strings.TrimSpace(*body.Name)
		}
		if body.Tags != nil {
			it.Tags = normalizeTags(*body.Tags)
		}
	})
	if errors.Is(err, ErrNotFound) {
		writeErr(w, 404, "no such item")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, it)
}

func (a *App) deleteItem(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Delete(r.PathValue("id")); errors.Is(err, ErrNotFound) {
		writeErr(w, 404, "no such item")
		return
	} else if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(204)
}

func exportOptions(r *http.Request) ExportOptions {
	q := r.URL.Query()
	num := func(k string) float64 { f, _ := strconv.ParseFloat(q.Get(k), 64); return f }
	return ExportOptions{Fit: q.Get("fit"), Top: q.Get("top"), Bottom: q.Get("bottom"), Start: num("start"), Dur: num("dur")}
}

func (a *App) runExport(w http.ResponseWriter, r *http.Request) (Item, ExportResult, bool) {
	it, err := a.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "no such item")
		return it, ExportResult{}, false
	}
	p, ok := presetByID(r.PathValue("preset"))
	if !ok {
		writeErr(w, 404, "no such preset")
		return it, ExportResult{}, false
	}
	res, err := a.conv.Export(r.Context(), it, p, exportOptions(r))
	if err != nil {
		log.Printf("export %s %s: %v", it.ID, p.ID, err)
		writeErr(w, 422, err.Error())
		return it, res, false
	}
	return it, res, true
}

func (a *App) export(w http.ResponseWriter, r *http.Request) {
	it, res, ok := a.runExport(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("track") != "0" {
		a.store.Update(it.ID, func(it *Item) { it.LastUsed = time.Now().UTC(); it.Uses++ })
	}
	disp := "inline"
	if r.URL.Query().Get("download") == "1" {
		disp = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": res.Filename}))
	w.Header().Set("X-Memebox-Filename", res.Filename)
	w.Header().Set("X-Memebox-Limit", strconv.FormatInt(res.Limit, 10))
	w.Header().Set("Access-Control-Expose-Headers", "X-Memebox-Filename, X-Memebox-Limit")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, res.Path)
}

// saveExport keeps a converted (for example captioned) version as a new
// library item, so a meme made once is one tap away next time.
func (a *App) saveExport(w http.ResponseWriter, r *http.Request) {
	it, res, ok := a.runExport(w, r)
	if !ok {
		return
	}
	f, err := os.Open(res.Path)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer f.Close()
	name := it.Name
	if o := exportOptions(r); o.Top != "" || o.Bottom != "" {
		name = strings.TrimSpace(o.Top + " " + o.Bottom)
	}
	ext := path.Ext(res.Filename)
	created, err := a.media.Ingest(r.Context(), f, name+ext, "", it.Tags)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	writeJSON(w, 201, created)
}

func (a *App) original(w http.ResponseWriter, r *http.Request) {
	it, err := a.store.Get(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": safeName(it.Name) + "." + it.Ext}))
	}
	http.ServeFile(w, r, a.store.OriginalPath(&it))
}

func (a *App) preview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.store.Get(id); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "image/webp")
	http.ServeFile(w, r, a.store.PreviewPath(id))
}

const loginHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<title>memebox · log in</title><link rel="stylesheet" href="/style.css">
<link rel="manifest" href="/manifest.webmanifest"><meta name="theme-color" content="#16131f"></head>
<body class="login"><form method="post" action="/login" class="login-card">
<img src="/icons/icon-192.png" alt="" width="72" height="72"><h1>memebox</h1>%s
<input type="password" name="password" placeholder="Password" autofocus autocomplete="current-password" required>
<button class="primary" type="submit">Log in</button></form></body></html>`

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if !a.auth.Enabled() || a.auth.Check(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	msg := ""
	if r.URL.Query().Get("bad") == "1" {
		msg = `<p class="error">Wrong password.</p>`
	}
	fmt.Fprintf(w, loginHTML, msg)
}

func (a *App) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !a.auth.Enabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !a.auth.Login(w, r, r.FormValue("password")) {
		http.Redirect(w, r, "/login?bad=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
