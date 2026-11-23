package monitor

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Mailer sends an alert email.
type Mailer interface {
	Send(subject, body string) error
}

// SMTP sends mail through an SMTP server (STARTTLS when offered; port 465 uses implicit TLS).
type SMTP struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	To       []string
}

// HTTPMail sends through GopherPost's send-email endpoint (form fields to, subject, body, fromName, brand).
type HTTPMail struct {
	URL      string // e.g. http://127.0.0.1:8787/api55/v65/send-email
	FromName string
	Brand    string // optional GopherPost brand, e.g. kite360
	To       []string
	Client   *http.Client
}

func splitList(s string) []string {
	var out []string
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func (m *HTTPMail) Send(subject, body string) error {
	c := m.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	form := url.Values{"to": {strings.Join(m.To, ",")}, "subject": {subject}, "body": {body}, "fromName": {m.FromName}}
	if m.Brand != "" {
		form.Set("brand", m.Brand)
	}
	resp, err := c.PostForm(m.URL, form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("mail service answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// MailerFromEnv builds a mailer from BERTH_ALERT_TO plus either BERTH_ALERT_URL (GopherPost) or
// BERTH_SMTP_HOST, _PORT, _USER, _PASS, _FROM. It returns nil when email is not configured.
func MailerFromEnv(get func(string) string) Mailer {
	if u, to := strings.TrimSpace(get("BERTH_ALERT_URL")), splitList(get("BERTH_ALERT_TO")); u != "" && len(to) > 0 {
		name := strings.TrimSpace(get("BERTH_ALERT_FROM_NAME"))
		if name == "" {
			name = "Berth"
		}
		return &HTTPMail{URL: u, FromName: name, Brand: strings.TrimSpace(get("BERTH_ALERT_BRAND")), To: to}
	}
	host, to := strings.TrimSpace(get("BERTH_SMTP_HOST")), strings.TrimSpace(get("BERTH_ALERT_TO"))
	if host == "" || to == "" {
		return nil
	}
	port := 587
	if n, err := strconv.Atoi(get("BERTH_SMTP_PORT")); err == nil && n > 0 {
		port = n
	}
	from := strings.TrimSpace(get("BERTH_SMTP_FROM"))
	if from == "" {
		from = strings.TrimSpace(get("BERTH_SMTP_USER"))
	}
	if from == "" {
		from = "berth@localhost"
	}
	m := &SMTP{Host: host, Port: port, User: get("BERTH_SMTP_USER"), Password: get("BERTH_SMTP_PASS"), From: from}
	m.To = splitList(to)
	return m
}

func oneLine(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

func (m *SMTP) Send(subject, body string) error {
	addr := net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
	var conn net.Conn
	var err error
	d := &net.Dialer{Timeout: 10 * time.Second}
	if m.Port == 465 {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: m.Host})
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok && m.Port != 465 {
		if err := c.StartTLS(&tls.Config{ServerName: m.Host}); err != nil {
			return err
		}
	}
	if m.User != "" {
		if err := c.Auth(smtp.PlainAuth("", m.User, m.Password, m.Host)); err != nil { // refuses to send a password without TLS
			return err
		}
	}
	if err := c.Mail(m.From); err != nil {
		return err
	}
	for _, to := range m.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
		oneLine(m.From), oneLine(strings.Join(m.To, ", ")), oneLine(subject), time.Now().Format(time.RFC1123Z), strings.ReplaceAll(body, "\r\n", "\n"))
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
