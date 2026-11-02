package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gomodule/redigo/redis"
	_ "github.com/lib/pq"
)

type message struct {
	ID        int64     `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type app struct {
	db    *sql.DB
	redis *redis.Pool
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func main() {
	// A URL safely quotes database credentials, including spaces and punctuation.
	dsn := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(env("PGHOST", "127.0.0.1"), env("PGPORT", "5432")),
		Path:   "/" + env("PGDATABASE", "common_room"),
		User:   url.UserPassword(env("PGUSER", "common_room"), env("PGPASSWORD", "local-demo-password")),
	}
	query := url.Values{"sslmode": {env("PGSSLMODE", "prefer")}, "connect_timeout": {"3"}}
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("postgres", dsn.String())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(3)
	if err := migrate(db); err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		return
	}

	redisURL := env("REDIS_URL", "redis://127.0.0.1:6379")
	// Keep the PHP demo's tcp:// URL format working too.
	redisURL = strings.Replace(redisURL, "tcp://", "redis://", 1)
	redisURL = strings.Replace(redisURL, "tls://", "rediss://", 1)
	pool := &redis.Pool{
		MaxIdle: 3, MaxActive: 20, Wait: true, IdleTimeout: time.Minute,
		DialContext: func(ctx context.Context) (redis.Conn, error) {
			return redis.DialURLContext(ctx, redisURL,
				redis.DialConnectTimeout(3*time.Second),
				redis.DialReadTimeout(3*time.Second), redis.DialWriteTimeout(3*time.Second))
		},
	}
	defer pool.Close()
	a := &app{db: db, redis: pool}
	server := &http.Server{
		Addr: ":" + env("PORT", "8090"), Handler: a,
		ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: time.Minute,
	}
	log.Printf("The common room is listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func migrate(db *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize schema creation when several app instances start together.
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(7400021)"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS messages (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		author VARCHAR(32) NOT NULL,
		body TEXT NOT NULL CHECK (char_length(body) BETWEEN 1 AND 1000),
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return err
	}
	return tx.Commit()
}

func visitor(r *http.Request) string {
	cookie, err := r.Cookie("room_visitor")
	if err != nil || len(cookie.Value) != 64 || strings.Trim(cookie.Value, "0123456789abcdef") != "" {
		return ""
	}
	return cookie.Value
}

func reply(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func reject(w http.ResponseWriter, status int, text string) {
	reply(w, status, map[string]string{"error": text})
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Second)
	defer cancel()
	if err := a.route(w, r.WithContext(ctx)); err != nil {
		log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
		reject(w, 503, "The room is temporarily unavailable. Please try again shortly.")
	}
}

func (a *app) route(w http.ResponseWriter, r *http.Request) error {
	method := "GET"
	switch r.URL.Path {
	case "/", "/assets/app.css", "/assets/app.js", "/api/state", "/health":
	case "/api/messages":
		method = "POST"
	default:
		reject(w, 404, "This page does not exist.")
		return nil
	}
	if r.Method != method && !(method == "GET" && r.Method == "HEAD") {
		w.Header().Set("Allow", method)
		reject(w, 405, "This method is not allowed.")
		return nil
	}

	token := visitor(r)
	switch r.URL.Path {
	case "/":
		if token == "" {
			var random [32]byte
			if _, err := rand.Read(random[:]); err != nil {
				return err
			}
			token = hex.EncodeToString(random[:])
		}
		http.SetCookie(w, &http.Cookie{Name: "room_visitor", Value: token, Path: "/",
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: os.Getenv("COOKIE_SECURE") == "1" || r.TLS != nil})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The replacement is a validated hex token, never arbitrary HTML.
		io.WriteString(w, strings.Replace(home, "{{TOKEN}}", token, 1))
	case "/assets/app.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		io.WriteString(w, css)
	case "/assets/app.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		io.WriteString(w, js)
	case "/api/state":
		if token == "" {
			reject(w, 401, "Please reload the page to join the room.")
			return nil
		}
		conn, err := a.redis.GetContext(r.Context())
		if err != nil {
			return err
		}
		defer conn.Close()
		viewers, err := redis.Int(conn.Do("EVAL", heartbeat, 1, "common-room:viewers", token, 20))
		if err != nil {
			return err
		}
		rows, err := a.db.QueryContext(r.Context(), "SELECT id, author, body, created_at FROM messages ORDER BY id DESC LIMIT 100")
		if err != nil {
			return err
		}
		defer rows.Close()
		messages := []message{}
		for rows.Next() {
			var m message
			if err := rows.Scan(&m.ID, &m.Author, &m.Body, &m.CreatedAt); err != nil {
				return err
			}
			messages = append(messages, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		reply(w, 200, map[string]any{"viewers": viewers, "messages": messages})
	case "/api/messages":
		return a.post(w, r, token)
	case "/health":
		if _, err := a.db.ExecContext(r.Context(), "SELECT 1 FROM messages LIMIT 1"); err != nil {
			return err
		}
		conn, err := a.redis.GetContext(r.Context())
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.Do("PING"); err != nil {
			return err
		}
		reply(w, 200, map[string]string{"status": "ok"})
	}
	return nil
}

func (a *app) post(w http.ResponseWriter, r *http.Request, token string) error {
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
		reject(w, 403, "Your session has changed. Reload the page and try again.")
		return nil
	}
	contentType := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if !strings.EqualFold(contentType, "application/json") {
		reject(w, 415, "Send the message as JSON.")
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16385))
	if err != nil {
		reject(w, 400, "The message could not be read.")
		return nil
	}
	if len(raw) > 16384 {
		reject(w, 413, "That message is too large.")
		return nil
	}
	var data any
	if !utf8.Valid(raw) || json.Unmarshal(raw, &data) != nil {
		reject(w, 400, "The message could not be read.")
		return nil
	}
	object, _ := data.(map[string]any)
	body, _ := object["body"].(string)
	body = strings.Trim(body, " \t\n\r\x00\x0B")
	if strings.TrimSpace(body) == "" || utf8.RuneCountInString(body) > 1000 || strings.ContainsRune(body, 0) {
		reject(w, 422, "Write a message between 1 and 1,000 characters.")
		return nil
	}
	conn, err := a.redis.GetContext(r.Context())
	if err != nil {
		return err
	}
	defer conn.Close()
	allowed, err := conn.Do("SET", "common-room:post:"+token, "1", "EX", 2, "NX")
	if err != nil {
		return err
	}
	if allowed == nil {
		w.Header().Set("Retry-After", "2")
		reject(w, 429, "Give it a moment before posting again.")
		return nil
	}
	hash := sha256.Sum256([]byte(token))
	author := fmt.Sprintf("Visitor %X", hash[:3])
	var m message
	err = a.db.QueryRowContext(r.Context(),
		"INSERT INTO messages (author, body) VALUES ($1, $2) RETURNING id, author, body, created_at", author, body).
		Scan(&m.ID, &m.Author, &m.Body, &m.CreatedAt)
	if err != nil {
		return err
	}
	reply(w, 201, map[string]any{"message": m})
	return nil
}

// Redis's clock and one atomic operation keep presence consistent across instances.
const heartbeat = `
local now = tonumber(redis.call('TIME')[1])
local ttl = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now - ttl)
redis.call('ZADD', KEYS[1], now, ARGV[1])
redis.call('EXPIRE', KEYS[1], ttl)
return redis.call('ZCARD', KEYS[1])
`

// The original interface is served from these constants; no asset files or build step.
const home = `<!doctype html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <meta name="description" content="A little shared space on the internet. See who's here, leave a thought, and watch the conversation unfold.">
    <meta name="csrf-token" content="{{TOKEN}}">
    <meta name="theme-color" content="#f5f4ef">
    <title>The common room</title>
    <link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 64 64'%3E%3Crect width='64' height='64' rx='18' fill='%232c4edf'/%3E%3Cpath d='M17 19h30v22H30l-9 7v-7h-4z' fill='white'/%3E%3C/svg%3E">
    <link rel="stylesheet" href="/assets/app.css">
    <script src="/assets/app.js" defer></script>
</head>
<body>
    <div class="shell">
        <header class="site-header">
            <a class="brand" href="/" aria-label="The common room home"><span class="brand-mark" aria-hidden="true">c.</span>the common room<span class="brand-period">.</span></a>
            <div class="connection" id="connection" role="status"><span class="status-dot"></span><span id="connection-label">Connecting</span></div>
        </header>
        <main>
            <section class="intro" aria-labelledby="page-title">
                <div class="intro-copy">
                    <p class="eyebrow">A LITTLE SPACE FOR EVERYONE</p>
                    <h1 id="page-title">You're here.<br>Say something.</h1>
                    <p class="description">A shared corner of the internet. Leave a thought, say hello,<br class="desktop-break"> or just hang out for a while. No account needed.</p>
                </div>
                <aside class="presence-card" aria-label="Live viewer count">
                    <div class="presence-heading"><span class="status-dot"></span> IN THE ROOM</div>
                    <div class="presence-count" id="viewer-count" aria-live="polite" aria-atomic="true">—</div>
                    <p id="viewer-label">Checking who’s here…</p>
                    <div class="presence-footer"><span class="people-icon" aria-hidden="true">◉ ◉ ◉</span><span>Including you. Make yourself at home.</span></div>
                </aside>
            </section>

            <section class="conversation" aria-labelledby="conversation-title">
                <div class="section-heading"><h2 id="conversation-title">The conversation <span id="message-count" class="count-badge">0</span></h2><span class="live-caption"><span class="status-dot"></span> Updates live</span></div>

                <form id="message-form" class="composer">
                    <label for="message">What’s on your mind?</label>
                    <textarea id="message" name="body" rows="3" placeholder="A hello, a big idea, or a very small observation…" aria-describedby="composer-note character-count" required></textarea>
                    <div class="composer-footer">
                        <span id="composer-note">You’ll appear as an anonymous visitor.</span>
                        <div class="composer-actions"><span id="character-count">0 / 1,000</span><button id="send-button" type="submit" disabled>Post message <span aria-hidden="true">↗</span></button></div>
                    </div>
                    <p id="form-feedback" class="form-feedback" role="status" aria-live="polite"></p>
                </form>

                <div class="log-heading"><span>LATEST MESSAGES</span><span>Newest first</span></div>
                <p id="room-error" class="room-error" role="alert" hidden></p>
                <div id="messages" class="messages" aria-label="Message log" aria-busy="true"></div>
                <div id="empty-state" class="empty-state" hidden><span class="empty-icon" aria-hidden="true">↗</span><h3>Every conversation starts somewhere.</h3><p>Be the first to leave a little something.</p></div>
                <noscript><p class="room-error">Enable JavaScript to see live viewers and post messages.</p></noscript>
            </section>
        </main>
        <footer class="site-footer"><span>A small room. An open conversation.</span><span>Be kind. You’re in good company.</span></footer>
    </div>
</body>
</html>
`

const css = `:root { font-family: Inter,ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; color: #24251f; background: #f5f4ef; font-synthesis: none; --muted: #75776e; --line: #e0e1d8; --blue: #2c4edf; --green: #318163; }
* { box-sizing: border-box; }
body { margin: 0; }
button,textarea { font: inherit; }
button,a,textarea { -webkit-tap-highlight-color: transparent; }
a { color: inherit; }
button:focus-visible,a:focus-visible,textarea:focus-visible { outline: 3px solid #91a2ff; outline-offset: 4px; }
[hidden] { display: none!important; }
.shell { max-width: 1100px; padding: 0 44px; margin: auto; }
.site-header { display: flex; justify-content: space-between; align-items: center; min-height: 111px; border-bottom: 1px solid var(--line); gap: 20px; }
.brand { display: flex; align-items: center; text-decoration: none; font-size: 19px; font-weight: 680; letter-spacing: -.65px; }
.brand-mark { display: grid; place-items: center; width: 34px; height: 34px; margin-right: 11px; border-radius: 10px; background: var(--blue); color: white; font-size: 28px; line-height: 1; padding-bottom: 5px; }
.brand-period { color: var(--blue); }
.connection { display: flex; align-items: center; gap: 8px; font-size: 12px; color: var(--muted); }
.status-dot { display: inline-block; width: 7px; height: 7px; flex-shrink: 0; border-radius: 50%; background: #aaa99e; }
.connected .status-dot,.presence-heading .status-dot,.live-caption .status-dot { background: var(--green); box-shadow: 0 0 0 3px #31816312; }
.connected { color: #3c705b; }
.intro { display: grid; grid-template-columns: 1fr 285px; gap: 45px; align-items: center; padding: 68px 0 58px; }
.eyebrow { font-size: 10px; letter-spacing: 2px; font-weight: 750; color: var(--blue); margin: 0 0 21px; }
h1 { font-size: clamp(40px,5vw,59px); line-height: 1.08; letter-spacing: -2.7px; font-weight: 600; margin: 0 0 22px; }
.description { color: var(--muted); font-size: 14px; line-height: 1.8; margin: 0; }
.presence-card { align-self: stretch; display: flex; flex-direction: column; justify-content: center; border: 1px solid #dce1d3; background: #edf0e6; border-radius: 15px; padding: 24px; }
.presence-heading { display: flex; align-items: center; gap: 8px; color: #53715c; font-size: 10px; letter-spacing: 1.4px; font-weight: 700; }
.presence-count { font-size: 68px; line-height: 1.2; letter-spacing: -4px; margin-top: 15px; font-weight: 500; font-variant-numeric: tabular-nums; }
.presence-card>p { font-size: 13px; color: #626c5d; margin: 3px 0 25px; }
.presence-footer { display: flex; align-items: center; gap: 9px; border-top: 1px solid #d9dfd1; padding-top: 18px; font-size: 10px; color: #747e6d; }
.people-icon { letter-spacing: -3px; color: #91a288; white-space: nowrap; margin-right: 4px; }
.section-heading { display: flex; align-items: center; justify-content: space-between; margin-bottom: 20px; }
h2 { font-size: 20px; font-weight: 600; letter-spacing: -.5px; display: flex; align-items: center; gap: 10px; margin: 0; }
.count-badge { font-size: 10px; letter-spacing: 0; background: #e7e8e0; color: #6f7367; padding: 4px 7px; border-radius: 6px; font-weight: 500; }
.live-caption { display: flex; align-items: center; gap: 7px; font-size: 11px; color: var(--muted); }
.live-caption .status-dot { width: 5px; height: 5px; }
.composer { background: #fff; border: 1px solid var(--line); border-radius: 13px; padding: 24px 24px 20px; box-shadow: 0 3px 8px #24251f03; }
.composer label { display: block; font-size: 13px; font-weight: 650; margin-bottom: 14px; }
textarea { width: 100%; min-height: 86px; border: 0; padding: 0; resize: vertical; font-size: 14px; line-height: 1.7; color: #33352d; background: transparent; }
textarea::placeholder { color: #a0a297; }
textarea:focus { outline: none; }
textarea:focus-visible { outline: 2px solid #c7cff9; outline-offset: 5px; border-radius: 3px; }
.composer-footer { display: flex; align-items: center; justify-content: space-between; gap: 16px; border-top: 1px solid #eff0ea; margin-top: 14px; padding-top: 16px; }
#composer-note { font-size: 11px; color: #909387; }
.composer-actions { display: flex; align-items: center; gap: 20px; }
#character-count { font-size: 10px; color: #939789; white-space: nowrap; font-variant-numeric: tabular-nums; }
#character-count.over-limit { color: #ae3e32; }
button { border: 0; background: var(--blue); color: white; padding: 12px 17px; border-radius: 7px; font-size: 12px; font-weight: 600; cursor: pointer; transition: background .15s,transform .15s; white-space: nowrap; }
button span { margin-left: 14px; font-size: 15px; }
button:hover { background: #2340c1; }
button:active { transform: translateY(1px); }
button:disabled { background: #b0bae9; cursor: not-allowed; }
.form-feedback { font-size: 12px; color: var(--green); margin: 13px 0 0; }
.form-feedback:empty { display: none; }
.form-feedback.error { color: #b13d32; }
.log-heading { display: flex; justify-content: space-between; align-items: center; font-size: 9px; letter-spacing: 1.4px; color: #8a8e80; margin: 29px 0 15px; }
.log-heading span:last-child { letter-spacing: 0; font-size: 10px; }
.messages { display: flex; flex-direction: column; gap: 10px; }
.message-card { display: flex; gap: 14px; padding: 21px 22px; background: #fdfdf9; border: 1px solid var(--line); border-radius: 11px; }
.avatar { display: grid; place-items: center; width: 35px; height: 35px; border-radius: 11px; flex-shrink: 0; font-size: 11px; font-weight: 650; background: #e5e9fa; color: #6677b5; }
.tone-1 { background: #f2e7da; color: #ad8554; }
.tone-2 { background: #e6eddf; color: #78905f; }
.tone-3 { background: #ede4ee; color: #96779b; }
.message-content { min-width: 0; flex: 1; }
.message-meta { display: flex; align-items: center; gap: 12px; margin: 2px 0 9px; }
.message-meta h3 { font-size: 12px; font-weight: 600; margin: 0; }
.message-meta time { font-size: 10px; color: #9a9d91; }
.message-content>p { margin: 0; font-size: 13px; color: #62665a; line-height: 1.75; white-space: pre-wrap; overflow-wrap: anywhere; }
.empty-state { text-align: center; border: 1px dashed #d8dcd0; border-radius: 11px; padding: 35px 20px; }
.empty-icon { display: inline-grid; place-items: center; width: 37px; height: 37px; border-radius: 50%; background: #e9ede0; color: #7e9070; font-size: 21px; }
.empty-state h3 { font-size: 14px; font-weight: 500; margin: 14px 0 8px; }
.empty-state p { font-size: 12px; color: var(--muted); margin: 0; }
.room-error { font-size: 13px; background: #fff2db; color: #836125; border-radius: 8px; padding: 14px; line-height: 1.6; }
.site-footer { margin-top: 50px; min-height: 90px; border-top: 1px solid var(--line); display: flex; align-items: center; justify-content: space-between; gap: 15px; font-size: 10px; color: #969a8d; }
@media(min-width:1500px) { .shell { max-width: 1160px; }
.intro { padding-top: 80px; padding-bottom: 70px; }
 }
@media(max-width:760px) { .shell { padding: 0 24px; }
.site-header { min-height: 88px; }
.intro { grid-template-columns: 1fr 230px; gap: 25px; padding: 45px 0; }
.presence-card { padding: 20px; }
.presence-count { font-size: 55px; }
.presence-footer { font-size: 9px; line-height: 1.5; }
.desktop-break { display: none; }
h1 { font-size: 43px; letter-spacing: -2px; }
.description { font-size: 13px; }
.brand { font-size: 16px; }
.composer-footer { align-items: flex-end; }
#composer-note { max-width: 150px; line-height: 1.6; }
.composer-actions { gap: 12px; }
 }
@media(max-width:560px) { .shell { padding: 0 20px; }
.site-header { gap: 10px; }
.brand { font-size: 15px; }
.brand-mark { width: 29px; height: 29px; font-size: 24px; margin-right: 8px; }
.connection { font-size: 10px; gap: 6px; }
.intro { grid-template-columns: 1fr; gap: 28px; padding: 36px 0; }
.eyebrow { font-size: 9px; margin-bottom: 17px; }
h1 { font-size: 48px; margin-bottom: 18px; }
.presence-card { position: relative; padding: 19px 21px; }
.presence-heading { font-size: 9px; }
.presence-count { font-size: 43px; margin: 9px 0 0; letter-spacing: -2px; }
.presence-card>p { margin: 0; font-size: 12px; }
.presence-footer { position: absolute; right: 20px; bottom: 24px; width: 130px; border: 0; padding: 0; font-size: 10px; }
.people-icon { display: none; }
h2 { font-size: 18px; }
.live-caption { font-size: 10px; }
.composer { padding: 20px 18px 17px; }
.composer-footer { flex-direction: column; align-items: stretch; gap: 13px; }
#composer-note { max-width: none; font-size: 10px; }
.composer-actions { justify-content: space-between; }
.message-card { padding: 18px 15px; gap: 11px; }
.message-meta { align-items: flex-start; justify-content: space-between; gap: 8px; }
.message-meta time { font-size: 9px; max-width: 100px; text-align: right; line-height: 1.5; }
.message-meta h3 { font-size: 11px; }
.site-footer { flex-direction: column; align-items: flex-start; justify-content: center; gap: 8px; margin-top: 35px; padding: 20px 0; }
.log-heading { margin-top: 24px; }
 }
`

const js = `'use strict';

const $ = (selector) => document.querySelector(selector);
const form = $('#message-form');
const input = $('#message');
const button = $('#send-button');
const csrf = $('meta[name="csrf-token"]').content;
const messages = new Map();
let posting = false;
let polling = false;
let lastRender = '';

async function request(path, options = {}) {
    const response = await fetch(path, {
        credentials: 'same-origin',
        cache: 'no-store',
        signal: AbortSignal.timeout(8000),
        ...options,
        headers: { Accept: 'application/json', ...options.headers },
    });
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || 'Something went wrong. Please try again.');
    return data;
}

function updateInput() {
    const length = Array.from(input.value).length;
    $('#character-count').textContent = length.toLocaleString() + ' / 1,000';
    $('#character-count').classList.toggle('over-limit', length > 1000);
    button.disabled = posting || input.value.trim().length === 0 || length > 1000;
}

function renderMessages(incoming) {
    // Merge responses so an older in-flight poll cannot hide a just-posted message.
    for (const message of incoming) messages.set(message.id, message);
    const recent = [...messages.values()].sort((a, b) => b.id - a.id).slice(0, 100);
    messages.clear();
    for (const message of recent) messages.set(message.id, message);
    const signature = recent.map((message) => message.id).join(',');
    $('#empty-state').hidden = recent.length !== 0;
    $('#message-count').textContent = recent.length === 100 ? '100 latest' : recent.length;
    if (signature === lastRender) return;
    lastRender = signature;
    const fragment = document.createDocumentFragment();
    for (const message of recent) {
        const article = document.createElement('article');
        article.className = 'message-card';
        const avatar = document.createElement('div');
        avatar.className = 'avatar tone-' + (message.id % 4);
        avatar.textContent = message.author.slice(-2);
        avatar.setAttribute('aria-hidden', 'true');
        const content = document.createElement('div');
        content.className = 'message-content';
        const meta = document.createElement('div');
        meta.className = 'message-meta';
        const author = document.createElement('h3');
        author.textContent = message.author;
        const time = document.createElement('time');
        const date = new Date(message.created_at);
        time.dateTime = message.created_at;
        time.textContent = date.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
        time.title = date.toLocaleString();
        const body = document.createElement('p');
        // Untrusted messages are always text, never HTML.
        body.textContent = message.body;
        meta.append(author, time);
        content.append(meta, body);
        article.append(avatar, content);
        fragment.append(article);
    }
    $('#messages').replaceChildren(fragment);
}

async function poll() {
    if (polling) return;
    polling = true;
    try {
        const data = await request('/api/state');
        $('#viewer-count').textContent = data.viewers;
        $('#viewer-label').textContent = data.viewers === 1 ? 'person here right now' : 'people here right now';
        $('#connection').classList.add('connected');
        $('#connection-label').textContent = 'Live & connected';
        $('#room-error').hidden = true;
        renderMessages(data.messages);
    } catch (error) {
        $('#connection').classList.remove('connected');
        $('#connection-label').textContent = 'Reconnecting';
        $('#viewer-count').textContent = '—';
        $('#viewer-label').textContent = 'Reconnecting to the room…';
        $('#room-error').textContent = 'Connection interrupted. Retrying automatically; your draft is safe.';
        $('#room-error').hidden = false;
    } finally {
        $('#messages').setAttribute('aria-busy', 'false');
        polling = false;
    }
}

form.addEventListener('submit', async (event) => {
    event.preventDefault();
    if (button.disabled) return;
    posting = true;
    input.readOnly = true;
    updateInput();
    button.textContent = 'Posting…';
    $('#form-feedback').textContent = '';
    $('#form-feedback').classList.remove('error');
    try {
        const data = await request('/api/messages', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
            body: JSON.stringify({ body: input.value }),
        });
        renderMessages([data.message]);
        input.value = '';
        $('#form-feedback').textContent = 'Your message is in the room.';
        void poll();
    } catch (error) {
        $('#form-feedback').textContent = error.name === 'TimeoutError' || error instanceof TypeError
            ? 'Could not confirm your post. Check the log before trying again; your draft is saved here.'
            : error.message;
        $('#form-feedback').classList.add('error');
    } finally {
        posting = false;
        input.readOnly = false;
        button.textContent = 'Post message ↗';
        updateInput();
        input.focus();
    }
});

input.addEventListener('input', updateInput);
input.addEventListener('keydown', (event) => {
    if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') form.requestSubmit();
});
window.addEventListener('online', poll);
document.addEventListener('visibilitychange', () => { if (!document.hidden) void poll(); });
updateInput();
void poll();
setInterval(poll, 2000);
`
