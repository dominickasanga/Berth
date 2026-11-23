// Command berth runs the Berth server, or creates an admin account.
//
//	berth serve                 start the web server
//	berth admin-create <user>   create an admin (reads password from stdin), prints the 2FA secret
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/dominickasanga/berth/config"
	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/deploy"
	"github.com/dominickasanga/berth/internal/logs"
	"github.com/dominickasanga/berth/internal/monitor"
	"github.com/dominickasanga/berth/internal/store"
	"github.com/dominickasanga/berth/internal/web"
	"github.com/pquerna/otp/totp"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// readPassword hides input and asks twice on a terminal; reads one line from a pipe (for scripts).
func readPassword() string {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		pw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(pw, "\r\n")
	}
	fmt.Fprint(os.Stderr, "Password (min 8 chars, hidden): ")
	a, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	must(err)
	fmt.Fprint(os.Stderr, "Repeat password: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	must(err)
	if string(a) != string(b) {
		log.Fatal("passwords do not match")
	}
	return string(a)
}

// version is the git commit this binary was built from (Go embeds it when building in a git checkout).
func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, modified, when := "", false, ""
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			rev = kv.Value
		case "vcs.modified":
			modified = kv.Value == "true"
		case "vcs.time":
			when = kv.Value
		}
	}
	if rev == "" {
		return "dev (built outside a git checkout)"
	}
	if len(rev) > 8 {
		rev = rev[:8]
	}
	v := rev
	if modified {
		v += "+local-changes"
	}
	if when != "" {
		v += " (" + when + ")"
	}
	return v
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: berth serve | berth admin-create <username>")
		os.Exit(2)
	}
	if os.Args[1] == "version" { // needs no settings
		fmt.Println("berth", version())
		return
	}
	ctx := context.Background()
	dsn := os.Getenv("BERTH_DATABASE_URL")
	if dsn == "" {
		log.Fatal("BERTH_DATABASE_URL is required (a dedicated Berth database)")
	}
	box, err := crypt.New(os.Getenv("BERTH_SECRET_KEY"))
	must(err)
	st, err := store.Open(ctx, dsn)
	must(err)

	switch os.Args[1] {
	case "admin-create":
		if len(os.Args) != 3 {
			log.Fatal("usage: berth admin-create <username>  (password on stdin)")
		}
		pw := readPassword()
		if len(pw) < 8 {
			log.Fatalf("password too short (%d characters); it needs at least 8", len(pw))
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		must(err)
		key, err := totp.Generate(totp.GenerateOpts{Issuer: "Berth", AccountName: os.Args[2]})
		must(err)
		sealed, err := box.Seal(key.Secret())
		must(err)
		must(st.CreateAdmin(ctx, os.Args[2], string(hash), sealed))
		fmt.Printf("Admin %q created.\nScan this QR code with your authenticator app:\n\n", os.Args[2])
		if qr, err := qrcode.New(key.URL(), qrcode.Medium); err == nil {
			fmt.Println(qr.ToSmallString(false))
		}
		fmt.Printf("Can't scan? Enter this key manually (time-based, 6 digits): %s\n", key.Secret())

	case "serve":
		dataDir, err := filepath.Abs(env("BERTH_DATA_DIR", "./data"))
		must(err)
		must(os.MkdirAll(dataDir, 0o700))

		// Logging: stderr (journald or a terminal), the Logs page's history, and a rotated file.
		hub := logs.NewHub(1000)
		outputs := []io.Writer{os.Stderr, hub}
		logPath, set := os.LookupEnv("BERTH_LOG_FILE") // set but empty turns the file off
		if !set {
			logPath = filepath.Join(dataDir, "logs", "berth.log")
		}
		if logPath != "" {
			maxMB, keep := 10, 5
			if n, err := strconv.Atoi(os.Getenv("BERTH_LOG_MAX_MB")); err == nil && n > 0 {
				maxMB = n
			}
			if n, err := strconv.Atoi(os.Getenv("BERTH_LOG_KEEP")); err == nil && n >= 0 {
				keep = n
			}
			f, err := logs.OpenFile(logPath, int64(maxMB)<<20, keep)
			if err != nil {
				log.Fatalf("cannot open the log file %s: %v", logPath, err)
			}
			outputs = append(outputs, f)
		}
		log.SetOutput(io.MultiWriter(outputs...))

		composeCmd := strings.Fields(env("BERTH_COMPOSE_CMD", "docker compose"))
		if v, warn, err := deploy.CheckCompose(composeCmd); err != nil {
			log.Fatal(err)
		} else {
			log.Printf("berth %s", version())
			log.Printf("docker compose %s", v)
			if warn != "" {
				log.Print("warning: " + warn)
			}
		}
		must(st.FailStaleDeploys(ctx))
		keepImages := 5
		if n, err := strconv.Atoi(os.Getenv("BERTH_KEEP_IMAGES")); err == nil && n > 0 {
			keepImages = n
		}
		expected, err := config.Expected()
		must(err)
		rawBase, set := os.LookupEnv("BERTH_BASE_PATH") // an empty value is allowed: serve at the site root
		if !set {
			rawBase = "/_berth"
		}
		basePath, err := web.NormalizeBase(rawBase)
		must(err)
		expectedServices, err := config.ExpectedServices()
		must(err)
		requestLog := strings.ToLower(env("BERTH_REQUEST_LOG", "quiet"))
		if requestLog != "all" && requestLog != "off" {
			requestLog = "quiet"
		}
		srv := &web.Server{
			RequestLog:       requestLog,
			Version:          version(),
			ExpectedServices: expectedServices,
			BasePath:         basePath,
			Expected:         expected,
			Store:            st,
			Box:              box,
			Deploy:           &deploy.Engine{Store: st, Box: box, DataDir: dataDir, ComposeCmd: composeCmd, KeepImages: keepImages},
			CookieSecure:     env("BERTH_COOKIE_SECURE", "true") != "false",
			Logs:             hub,
		}
		// Monitoring of the supporting services (PostgreSQL, Redis, MongoDB, RabbitMQ, ...).
		mail := monitor.MailerFromEnv(os.Getenv)
		interval, retention := 30, 7
		if n, err := strconv.Atoi(os.Getenv("BERTH_METRICS_INTERVAL")); err == nil && n >= 5 {
			interval = n
		}
		if n, err := strconv.Atoi(os.Getenv("BERTH_METRICS_RETENTION_DAYS")); err == nil && n > 0 {
			retention = n
		}
		srv.Sampler = &monitor.Sampler{Store: st, Box: box, Mail: mail, Interval: time.Duration(interval) * time.Second,
			Retention: time.Duration(retention) * 24 * time.Hour, PublicURL: strings.TrimRight(os.Getenv("BERTH_PUBLIC_URL"), "/")}
		srv.MailOn = mail != nil
		go srv.Sampler.Run(ctx)
		log.Printf("monitoring services every %ds, keeping %d days; alert email %s", interval, retention, map[bool]string{true: "on", false: "off (set BERTH_SMTP_HOST and BERTH_ALERT_TO)"}[mail != nil])
		h, err := srv.Handler()
		must(err)
		addr := env("BERTH_ADDR", "127.0.0.1:8095")
		if logPath != "" {
			log.Printf("logging to %s", logPath)
		}
		if web.PlainHTTPExposed(addr, srv.CookieSecure) {
			log.Printf("WARNING: listening on %s over plain HTTP: passwords, 2FA codes and sessions are not encrypted, and anyone who can reach this port can try to log in. Restrict it with a firewall (your IP only) or put HTTPS in front and set BERTH_COOKIE_SECURE=true", addr)
		}
		log.Printf("berth listening on http://%s%s/ (data dir %s)", addr, basePath, dataDir)
		hs := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		must(hs.ListenAndServe())

	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}
