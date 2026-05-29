package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"regexp"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//go:embed templates static
var assetsFS embed.FS

type App struct {
	fs        *firestore.Client
	templates *template.Template
	static    http.Handler
	msgCache  messageCache
}

type messageCache struct {
	mu        sync.RWMutex
	messages  []Message
	fetchedAt time.Time
}

type Message struct {
	ID        string    `json:"id" firestore:"-"`
	Author    string    `json:"author" firestore:"author"`
	Device    string    `json:"device" firestore:"device"`
	Text      string    `json:"text" firestore:"text"`
	CreatedAt time.Time `json:"createdAt" firestore:"createdAt"`
}

type chatError struct {
	At      string `json:"at"`
	Source  string `json:"source"`
	Message string `json:"message"`
}

type chatView struct {
	Messages []Message
	Errors   []chatError
}

func newChatError(source string, err error) chatError {
	return chatError{
		At:      time.Now().UTC().Format(time.RFC3339),
		Source:  source,
		Message: err.Error(),
	}
}

func writeChatErrorsHeader(w http.ResponseWriter, errs []chatError) {
	if len(errs) == 0 {
		return
	}
	b, err := json.Marshal(errs)
	if err != nil {
		return
	}
	w.Header().Set("X-Chat-Errors", string(b))
}

var (
	initOnce sync.Once
	appInst  *App
	initErr  error
)

func main() {
	addr := envOr("ADDR", envOr("PORT", ":8080"))
	if !strings.HasPrefix(addr, ":") {
		addr = ":" + addr
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)

	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, securityHeaders(mux)); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	app, err := getApp(r.Context())
	if err != nil {
		log.Printf("app init error: %v", err)
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			serveChatFallback(w, []chatError{newChatError("app", err)})
			return
		}
		http.Error(w, "app init: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/static/") {
		app.static.ServeHTTP(w, r)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/":
		app.handleIndex(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/messages":
		app.handleMessagesPartial(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/send":
		app.handleSend(w, r)
	default:
		http.NotFound(w, r)
	}
}

func getApp(ctx context.Context) (*App, error) {
	initOnce.Do(func() {
		appInst, initErr = newApp(ctx)
	})
	return appInst, initErr
}

func newApp(ctx context.Context) (*App, error) {
	projectID := strings.TrimSpace(os.Getenv("FIREBASE_PROJECT_ID"))
	sa := strings.TrimSpace(os.Getenv("FIREBASE_SERVICE_ACCOUNT_JSON"))

	if projectID == "" && sa != "" {
		var meta struct {
			ProjectID string `json:"project_id"`
		}
		if err := json.Unmarshal([]byte(sa), &meta); err == nil {
			projectID = strings.TrimSpace(meta.ProjectID)
		}
	}

	if projectID == "" {
		return nil, errors.New("missing FIREBASE_PROJECT_ID (and could not derive from FIREBASE_SERVICE_ACCOUNT_JSON)")
	}

	var opts []option.ClientOption
	if sa != "" {
		opts = append(opts, option.WithCredentialsJSON([]byte(sa)))
	}

	fs, err := firestore.NewClient(ctx, projectID, opts...)
	if err != nil {
		return nil, err
	}

	tpls, err := template.New("").Funcs(template.FuncMap{
		"since": func(t time.Time) string {
			d := time.Since(t)
			if d < time.Minute {
				return "just now"
			}
			if d < time.Hour {
				return plural(int(d.Minutes()), "minute")
			}
			if d < 24*time.Hour {
				return plural(int(d.Hours()), "hour")
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"linkify": func(s string) template.HTML {
			return linkifyText(s)
		},
		"errorsJSON": func(errs []chatError) template.JS {
			if len(errs) == 0 {
				return template.JS("[]")
			}
			b, err := json.Marshal(errs)
			if err != nil {
				return template.JS("[]")
			}
			return template.JS(b)
		},
	}).ParseFS(assetsFS, "templates/*.html")
	if err != nil {
		fs.Close()
		return nil, err
	}

	staticFS, err := fsSub(assetsFS, ".")
	if err != nil {
		fs.Close()
		return nil, err
	}

	return &App{
		fs:        fs,
		templates: tpls,
		static:    http.FileServer(http.FS(staticFS)),
	}, nil
}

func fsSub(efs embed.FS, dir string) (fs.FS, error) {
	if dir == "." || dir == "" {
		return efs, nil
	}
	return fs.Sub(efs, dir)
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	view := a.loadChatView(ctx)
	writeChatErrorsHeader(w, view.Errors)
	a.render(w, "index.html", view)
}

func (a *App) handleMessagesPartial(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	view := a.loadChatView(ctx)
	writeChatErrorsHeader(w, view.Errors)
	a.render(w, "messages.html", view)
}

func (a *App) loadChatView(ctx context.Context) chatView {
	msgs, stale, err := a.listMessagesCached(ctx, 50)
	if err != nil {
		log.Printf("listMessages error: %v", err)
		return chatView{Errors: []chatError{newChatError("listMessages", err)}}
	}
	var errs []chatError
	if stale {
		errs = append(errs, newChatError("listMessages", errors.New("quota exceeded — showing cached messages")))
		log.Printf("listMessages quota exceeded, serving cached messages")
	}
	return chatView{Messages: msgs, Errors: errs}
}

func (a *App) handleSend(w http.ResponseWriter, r *http.Request) {
	var sendErrs []chatError

	if err := r.ParseForm(); err != nil {
		sendErrs = append(sendErrs, newChatError("send", err))
		a.renderSendResult(w, r.Context(), sendErrs)
		return
	}

	author := strings.TrimSpace(r.FormValue("author"))
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		sendErrs = append(sendErrs, newChatError("send", errors.New("message required")))
		a.renderSendResult(w, r.Context(), sendErrs)
		return
	}
	if len(text) > 2000 {
		sendErrs = append(sendErrs, newChatError("send", errors.New("message too long")))
		a.renderSendResult(w, r.Context(), sendErrs)
		return
	}
	if len(author) > 60 {
		author = author[:60]
	}
	device := detectDevice(r.UserAgent())

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.addMessage(ctx, author, device, text); err != nil {
		log.Printf("addMessage error: %v", err)
		sendErrs = append(sendErrs, newChatError("addMessage", err))
		a.renderSendResult(w, ctx, sendErrs)
		return
	}
	a.invalidateMessageCache()

	view := a.loadChatView(ctx)
	view.Errors = append(sendErrs, view.Errors...)
	writeChatErrorsHeader(w, view.Errors)
	a.render(w, "messages.html", view)
}

func (a *App) renderSendResult(w http.ResponseWriter, ctx context.Context, errs []chatError) {
	view := a.loadChatView(ctx)
	view.Errors = append(errs, view.Errors...)
	writeChatErrorsHeader(w, view.Errors)
	a.render(w, "messages.html", view)
}

func (a *App) addMessage(ctx context.Context, author, device, text string) error {
	m := Message{
		Author:    author,
		Device:    device,
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}
	_, _, err := a.fs.Collection("messages").Add(ctx, m)
	return err
}

func detectDevice(ua string) string {
	u := strings.ToLower(strings.TrimSpace(ua))
	if u == "" {
		return "Web"
	}

	// Mobile OS
	if strings.Contains(u, "iphone") || strings.Contains(u, "ipad") || strings.Contains(u, "ipod") {
		return "iOS"
	}
	if strings.Contains(u, "android") {
		return "Android"
	}

	// Desktop OS
	if strings.Contains(u, "mac os x") || strings.Contains(u, "macintosh") {
		return "macOS"
	}
	if strings.Contains(u, "windows nt") {
		return "Windows"
	}
	if strings.Contains(u, "cros") {
		return "ChromeOS"
	}
	if strings.Contains(u, "linux") {
		return "Linux"
	}

	return "Web"
}

func messageCacheTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("MESSAGES_CACHE_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 8 * time.Second
}

func (a *App) invalidateMessageCache() {
	a.msgCache.mu.Lock()
	a.msgCache.fetchedAt = time.Time{}
	a.msgCache.mu.Unlock()
}

func (a *App) listMessagesCached(ctx context.Context, limit int) ([]Message, bool, error) {
	ttl := messageCacheTTL()

	a.msgCache.mu.RLock()
	if !a.msgCache.fetchedAt.IsZero() && time.Since(a.msgCache.fetchedAt) < ttl {
		msgs := cloneMessages(a.msgCache.messages)
		a.msgCache.mu.RUnlock()
		return msgs, false, nil
	}
	staleMsgs := cloneMessages(a.msgCache.messages)
	a.msgCache.mu.RUnlock()

	msgs, err := a.listMessages(ctx, limit)
	if err != nil {
		if isQuotaExceeded(err) && len(staleMsgs) > 0 {
			return staleMsgs, true, nil
		}
		return nil, false, err
	}

	a.msgCache.mu.Lock()
	a.msgCache.messages = msgs
	a.msgCache.fetchedAt = time.Now()
	a.msgCache.mu.Unlock()

	return cloneMessages(msgs), false, nil
}

func cloneMessages(in []Message) []Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]Message, len(in))
	copy(out, in)
	return out
}

func isQuotaExceeded(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok {
		return st.Code() == codes.ResourceExhausted
	}
	s := err.Error()
	return strings.Contains(s, "ResourceExhausted") || strings.Contains(s, "Quota exceeded")
}

func (a *App) listMessages(ctx context.Context, limit int) ([]Message, error) {
	iter := a.fs.Collection("messages").
		OrderBy("createdAt", firestore.Desc).
		Limit(limit).
		Documents(ctx)
	defer iter.Stop()

	var out []Message
	for {
		doc, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		var m Message
		if err := doc.DataTo(&m); err != nil {
			return nil, err
		}
		m.ID = doc.Ref.ID
		out = append(out, m)
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error: %v", err)
		serveChatFallback(w, []chatError{newChatError("template", err)})
	}
}

func serveChatFallback(w http.ResponseWriter, errs []chatError) {
	writeChatErrorsHeader(w, errs)
	errsJSON := "[]"
	if len(errs) > 0 {
		if b, err := json.Marshal(errs); err == nil {
			errsJSON = string(b)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Chat</title>
  <link rel="stylesheet" href="/static/app.css" />
  <script type="application/json" id="chat-initial-errors">` + errsJSON + `</script>
</head>
<body>
  <div class="wrap">
    <header class="top">
      <div class="brand"><div class="brandicon" aria-hidden="true"></div></div>
      <div class="toptools">
        <div class="errors-wrap">
          <button class="iconbtn errors-bell" type="button" id="errorsBell" aria-label="Error log" aria-expanded="false">
            <svg viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M12 22a2.5 2.5 0 0 0 2.45-2h-4.9A2.5 2.5 0 0 0 12 22Zm7-6V11a7 7 0 0 0-5.25-6.77V3a1.75 1.75 0 1 0-3.5 0v1.23A7 7 0 0 0 5 11v5l-2 2v1h18v-1l-2-2Z"/></svg>
            <span class="errors-badge" id="errorsBadge" hidden>0</span>
          </button>
          <div class="errors-panel" id="errorsPanel" hidden>
            <div class="errors-panelhead">
              <span class="errors-paneltitle">Error log</span>
              <button class="errors-clear" type="button" id="errorsClear">Clear</button>
            </div>
            <ul class="errors-list" id="errorsList"></ul>
          </div>
        </div>
      </div>
    </header>
    <main class="card">
      <section class="messages" id="messages">
        <div class="msglist"><div class="empty">No messages yet. Say hi.</div></div>
      </section>
      <form class="composer" action="/" method="get">
        <textarea class="text" name="text" placeholder="Type a message…" disabled rows="2"></textarea>
        <button class="send" type="button" onclick="location.reload()">Refresh</button>
      </form>
    </main>
  </div>
  <script src="https://unpkg.com/htmx.org@1.9.12"></script>
  <script src="/static/errors.js"></script>
</body>
</html>`))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		w.Header().Add("Access-Control-Expose-Headers", "X-Chat-Errors, X-Chat-Stale")
		next.ServeHTTP(w, r)
	})
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return strconv.Itoa(n) + " " + unit + "s ago"
}

var urlRe = regexp.MustCompile(`(?i)\b((?:https?://|www\.)[^\s<>"']+[^\s<>"'.,;:!?])`)

func linkifyText(s string) template.HTML {
	if s == "" {
		return template.HTML("")
	}

	matches := urlRe.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return template.HTML(template.HTMLEscapeString(s))
	}

	var b strings.Builder
	b.Grow(len(s) + 32)
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start > last {
			b.WriteString(template.HTMLEscapeString(s[last:start]))
		}
		raw := s[start:end]
		href := raw
		if strings.HasPrefix(strings.ToLower(href), "www.") {
			href = "https://" + href
		}
		b.WriteString(`<a class="autolink" href="`)
		b.WriteString(template.HTMLEscapeString(href))
		b.WriteString(`" target="_blank" rel="noopener noreferrer">`)
		b.WriteString(template.HTMLEscapeString(raw))
		b.WriteString(`</a>`)
		last = end
	}
	if last < len(s) {
		b.WriteString(template.HTMLEscapeString(s[last:]))
	}
	return template.HTML(b.String())
}
