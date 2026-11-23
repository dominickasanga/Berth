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

	"github.com/dominickasanga/berth/internal/crypt"
	"github.com/dominickasanga/berth/internal/deploy"
	"github.com/dominickasanga/berth/internal/store"
	"github.com/dominickasanga/berth/internal/web"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
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
		fmt.Fprint(os.Stderr, "Password (min 12 chars): ")
		pw, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(pw, "\r\n")
		if len(pw) < 12 {
			log.Fatal("password too short")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
		must(err)
		key, err := totp.Generate(totp.GenerateOpts{Issuer: "Berth", AccountName: os.Args[2]})
		must(err)
		sealed, err := box.Seal(key.Secret())
		must(err)
		must(st.CreateAdmin(ctx, os.Args[2], string(hash), sealed))
		fmt.Printf("Admin %q created.\nAdd to your authenticator app:\n%s\n", os.Args[2], key.URL())

	case "serve":
		dataDir, err := filepath.Abs(env("BERTH_DATA_DIR", "./data"))
		must(err)
		must(os.MkdirAll(dataDir, 0o700))
		must(st.FailStaleDeploys(ctx))
		keepImages := 5
		if n, err := strconv.Atoi(os.Getenv("BERTH_KEEP_IMAGES")); err == nil && n > 0 {
			keepImages = n
		}
		srv := &web.Server{
			Store:        st,
			Box:          box,
			Deploy:       &deploy.Engine{Store: st, Box: box, DataDir: dataDir, ComposeCmd: strings.Fields(env("BERTH_COMPOSE_CMD", "docker compose")), KeepImages: keepImages},
			CookieSecure: env("BERTH_COOKIE_SECURE", "true") != "false",
		}
		h, err := srv.Handler()
		must(err)
		addr := env("BERTH_ADDR", "127.0.0.1:8090")
		log.Printf("berth listening on %s (data dir %s)", addr, dataDir)
		hs := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		must(hs.ListenAndServe())

	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}
