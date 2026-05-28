package main

import (
	"context"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

type App struct {
	fs        *firestore.Client
	templates *template.Template
}

type Message struct {
	ID        string    `json:"id" firestore:"-"`
	Author    string    `json:"author" firestore:"author"`
	Text      string    `json:"text" firestore:"text"`
	CreatedAt time.Time `json:"createdAt" firestore:"createdAt"`
}

func main() {
	ctx := context.Background()

	app, err := NewApp(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer app.fs.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", app.handleIndex)
	mux.HandleFunc("GET /messages", app.handleMessagesPartial)
	mux.HandleFunc("POST /send", app.handleSend)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))

	addr := envOr("ADDR", ":8080")
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, securityHeaders(mux)); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func NewApp(ctx context.Context) (*App, error) {
	projectID := os.Getenv("FIREBASE_PROJECT_ID")
	if strings.TrimSpace(projectID) == "" {
		return nil, errors.New("missing FIREBASE_PROJECT_ID")
	}

	var opts []option.ClientOption
	if sa := strings.TrimSpace(os.Getenv("FIREBASE_SERVICE_ACCOUNT_JSON")); sa != "" {
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
	}).ParseGlob("./templates/*.html")
	if err != nil {
		fs.Close()
		return nil, err
	}

	return &App{
		fs:        fs,
		templates: tpls,
	}, nil
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	msgs, err := a.listMessages(ctx, 50)
	if err != nil {
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}

	data := struct {
		Messages []Message
	}{
		Messages: msgs,
	}
	a.render(w, "index.html", data)
}

func (a *App) handleMessagesPartial(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	msgs, err := a.listMessages(ctx, 50)
	if err != nil {
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	a.render(w, "messages.html", struct {
		Messages []Message
	}{Messages: msgs})
}

func (a *App) handleSend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	author := strings.TrimSpace(r.FormValue("author"))
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if len(text) > 2000 {
		http.Error(w, "message too long", http.StatusBadRequest)
		return
	}
	if len(author) > 60 {
		author = author[:60]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.addMessage(ctx, author, text); err != nil {
		http.Error(w, "failed to send", http.StatusInternalServerError)
		return
	}

	// HTMX-friendly: respond with updated messages partial and let the client swap it in.
	msgs, err := a.listMessages(ctx, 50)
	if err != nil {
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	a.render(w, "messages.html", struct {
		Messages []Message
	}{Messages: msgs})
}

func (a *App) addMessage(ctx context.Context, author, text string) error {
	m := Message{
		Author:    author,
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}
	_, _, err := a.fs.Collection("messages").Add(ctx, m)
	return err
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

	// Reverse to show oldest -> newest in UI.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
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
	return itoa(n) + " " + unit + "s ago"
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

