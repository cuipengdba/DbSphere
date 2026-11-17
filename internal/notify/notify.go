package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/store"
)

type Config struct {
	WebhookURL *url.URL
	Secret     []byte
	WebhookMin string
	EmailMin   string
	SMTPHost   string
	SMTPPort   int
	SMTPFrom   *mail.Address
	SMTPTo     []*mail.Address
	SMTPUser   string
	SMTPPass   string
	AllowPlain bool
	Client     *http.Client
}

func severityRank(value string) int {
	switch value {
	case "info":
		return 0
	case "warning":
		return 1
	case "critical":
		return 2
	default:
		return -1
	}
}

func Load() (Config, error) {
	base := os.Getenv("DBSPHERE_NOTIFY_MIN_SEVERITY")
	if base == "" {
		base = "warning"
	}
	cfg := Config{WebhookMin: os.Getenv("DBSPHERE_WEBHOOK_MIN_SEVERITY"),
		EmailMin: os.Getenv("DBSPHERE_EMAIL_MIN_SEVERITY"),
		Secret:   []byte(os.Getenv("DBSPHERE_WEBHOOK_SECRET"))}
	if cfg.WebhookMin == "" {
		cfg.WebhookMin = base
	}
	if cfg.EmailMin == "" {
		cfg.EmailMin = base
	}
	if severityRank(base) < 0 || severityRank(cfg.WebhookMin) < 0 || severityRank(cfg.EmailMin) < 0 {
		return Config{}, errors.New("invalid notification severity")
	}
	if raw := os.Getenv("DBSPHERE_WEBHOOK_URL"); raw != "" {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" ||
			(parsed.Scheme != "https" && !(parsed.Scheme == "http" &&
				(os.Getenv("DBSPHERE_WEBHOOK_ALLOW_HTTP") == "true" || loopback(parsed.Hostname())))) {
			return Config{}, errors.New("invalid webhook URL or insecure transport")
		}
		cfg.WebhookURL = parsed
	}
	cfg.SMTPHost = os.Getenv("DBSPHERE_SMTP_HOST")
	from, to := os.Getenv("DBSPHERE_SMTP_FROM"), os.Getenv("DBSPHERE_SMTP_TO")
	if (cfg.SMTPHost != "" || from != "" || to != "") &&
		(cfg.SMTPHost == "" || from == "" || to == "") {
		return Config{}, errors.New("SMTP host, from and to must be set together")
	}
	if cfg.SMTPHost != "" {
		cfg.SMTPPort = 587
		if raw := os.Getenv("DBSPHERE_SMTP_PORT"); raw != "" {
			port, err := strconv.Atoi(raw)
			if err != nil || port < 1 || port > 65535 {
				return Config{}, errors.New("invalid SMTP port")
			}
			cfg.SMTPPort = port
		}
		var err error
		cfg.SMTPFrom, err = mail.ParseAddress(from)
		if err != nil {
			return Config{}, errors.New("invalid SMTP from address")
		}
		cfg.SMTPTo, err = mail.ParseAddressList(to)
		if err != nil || len(cfg.SMTPTo) == 0 {
			return Config{}, errors.New("invalid SMTP recipient addresses")
		}
		cfg.AllowPlain = os.Getenv("DBSPHERE_SMTP_ALLOW_PLAIN") == "true" && loopback(cfg.SMTPHost)
		if os.Getenv("DBSPHERE_SMTP_ALLOW_PLAIN") == "true" && !cfg.AllowPlain {
			return Config{}, errors.New("plain SMTP is allowed only on loopback")
		}
		cfg.SMTPUser, cfg.SMTPPass = os.Getenv("DBSPHERE_SMTP_USER"), os.Getenv("DBSPHERE_SMTP_PASSWORD")
		if (cfg.SMTPUser == "") != (cfg.SMTPPass == "") {
			return Config{}, errors.New("SMTP user and password must be set together")
		}
	}
	cfg.Client = &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return cfg, nil
}

func loopback(host string) bool {
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}

func (cfg Config) Send(ctx context.Context, item store.Notification) (string, error) {
	var payload map[string]any
	if json.Unmarshal(item.Payload, &payload) != nil {
		return "", errors.New("invalid notification evidence")
	}
	severity, _ := payload["severity"].(string)
	minimum := cfg.WebhookMin
	if item.Channel == "email" {
		minimum = cfg.EmailMin
	}
	if severityRank(severity) < severityRank(minimum) {
		return "skipped", nil
	}
	message := map[string]any{"schemaVersion": 1, "deliveryId": item.ID,
		"eventId": item.EventID, "eventTitle": item.EventTitle, "eventStatus": item.EventStatus,
		"assetId": item.AssetID, "assetName": item.AssetName, "kind": item.Kind,
		"observedAt": item.ObservedAt, "ruleId": payload["ruleId"],
		"severity": severity, "value": payload["value"], "source": payload["source"]}
	switch item.Channel {
	case "webhook":
		if cfg.WebhookURL == nil {
			return "skipped", nil
		}
		body, _ := json.Marshal(message)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WebhookURL.String(), bytes.NewReader(body))
		if err != nil {
			return "", errors.New("webhook request failed")
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-DbSphere-Delivery-Id", item.ID)
		if len(cfg.Secret) != 0 {
			mac := hmac.New(sha256.New, cfg.Secret)
			_, _ = mac.Write(body)
			request.Header.Set("X-DbSphere-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		response, err := cfg.Client.Do(request)
		if err != nil {
			return "", errors.New("webhook request failed")
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return "", fmt.Errorf("webhook HTTP %d", response.StatusCode)
		}
		return "delivered", nil
	case "email":
		if cfg.SMTPHost == "" {
			return "skipped", nil
		}
		if err := cfg.sendEmail(ctx, item, severity, payload); err != nil {
			return "", errors.New("SMTP delivery failed")
		}
		return "delivered", nil
	default:
		return "", errors.New("unsupported notification channel")
	}
}

func (cfg Config) sendEmail(ctx context.Context, item store.Notification, severity string, payload map[string]any) error {
	address := net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(cfg.SMTPPort))
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	var err error
	if cfg.SMTPPort == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", address,
			&tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	client, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		return err
	}
	defer client.Close()
	if cfg.SMTPPort != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
				return err
			}
		} else if !cfg.AllowPlain {
			return errors.New("SMTP STARTTLS unavailable")
		}
	}
	if cfg.SMTPUser != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPHost)); err != nil {
			return err
		}
	}
	if err := client.Mail(cfg.SMTPFrom.Address); err != nil {
		return err
	}
	for _, recipient := range cfg.SMTPTo {
		if err := client.Rcpt(recipient.Address); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	subject := mime.QEncoding.Encode("utf-8", "[DbSphere "+severity+"] "+
		strings.ReplaceAll(strings.ReplaceAll(item.AssetName+" · "+item.EventTitle, "\r", " "), "\n", " "))
	to := make([]string, 0, len(cfg.SMTPTo))
	for _, recipient := range cfg.SMTPTo {
		to = append(to, recipient.String())
	}
	message := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n事件 ID: %s\r\n资产: %s (%s)\r\n告警: %v\r\n状态: %s\r\n级别: %s\r\n时间: %s\r\n指标来源: %v\r\n",
		cfg.SMTPFrom.String(), strings.Join(to, ", "), subject, item.EventID,
		item.AssetName, item.AssetID, payload["ruleId"], item.Kind, severity,
		item.ObservedAt.Format(time.RFC3339), payload["source"])
	if _, err := io.WriteString(writer, message); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return client.Quit()
}
