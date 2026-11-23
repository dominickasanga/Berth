// Command berth runs the Berth server, or creates an admin account.
//
//	berth serve                 start the web server
//	berth admin-create <user>   create an admin (reads password from stdin), prints the 2FA secret
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dominickasanga/berth/config"
	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/deploy"
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: berth serve | berth admin-create <username>")
		os.Exit(2)
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
		srv := &web.Server{
			BasePath:     basePath,
			Expected:     expected,
			Store:        st,
			Box:          box,
			Deploy:       &deploy.Engine{Store: st, Box: box, DataDir: dataDir, ComposeCmd: strings.Fields(env("BERTH_COMPOSE_CMD", "docker compose")), KeepImages: keepImages},
			CookieSecure: env("BERTH_COOKIE_SECURE", "true") != "false",
		}
		h, err := srv.Handler()
		must(err)
		addr := env("BERTH_ADDR", "127.0.0.1:8090")
		log.Printf("berth listening on http://%s%s/ (data dir %s)", addr, basePath, dataDir)
		hs := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		must(hs.ListenAndServe())

	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}
