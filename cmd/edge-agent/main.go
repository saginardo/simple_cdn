package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"simple_cdn/internal/edge"
	"simple_cdn/internal/version"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version.Version)
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "upgrade-helper" {
		if err := edge.RunUpgradeHelper(env("EDGE_STATE_DIR", "/opt/cdn-edge/data"), os.Args[2]); err != nil {
			fatal("online upgrade helper: " + err.Error())
		}
		return
	}
	pollSeconds := 30
	if value := os.Getenv("EDGE_POLL_SECONDS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 5 || parsed > 300 {
			fatal("EDGE_POLL_SECONDS must be between 5 and 300")
		}
		pollSeconds = parsed
	}
	agent, err := edge.New(edge.Config{
		ControlURL: os.Getenv("CONTROL_URL"), EnrollmentToken: os.Getenv("ENROLLMENT_TOKEN"),
		StateDir: env("EDGE_STATE_DIR", "/opt/cdn-edge/data"), NginxConfigPath: env("NGINX_CONFIG_PATH", "/opt/cdn-edge/config/nginx/cdn-platform.conf"),
		NginxStreamConfigPath: env("NGINX_STREAM_CONFIG_PATH", "/opt/cdn-edge/config/nginx/cdn-platform-stream.conf"),
		NginxMainConfigPath:   env("NGINX_MAIN_CONFIG_PATH", "/opt/cdn-edge/config/nginx/cdn-platform-main.conf"),
		NginxEventsConfigPath: env("NGINX_EVENTS_CONFIG_PATH", "/opt/cdn-edge/config/nginx/cdn-platform-events.conf"),
		NginxBinaryPath:       env("NGINX_BINARY_PATH", "/opt/cdn-edge/nginx/sbin/nginx"),
		NginxPIDPath:          env("NGINX_PID_PATH", "/opt/cdn-edge/nginx/run/nginx.pid"),
		NginxVersionPath:      env("NGINX_VERSION_PATH", "/opt/cdn-edge/nginx/VERSION"),
		NginxSHA256Path:       env("NGINX_SHA256_PATH", "/opt/cdn-edge/nginx/.bundle-sha256"),
		CertificateDir:        env("EDGE_CERT_DIR", "/opt/cdn-edge/config/certs"), AccessLogPath: env("EDGE_ACCESS_LOG", "/opt/cdn-edge/logs/access.json"), PollInterval: time.Duration(pollSeconds) * time.Second,
		SecurityLogPath: env("EDGE_SECURITY_LOG", "/opt/cdn-edge/logs/security.json"),
		Capabilities:    splitValues(os.Getenv("EDGE_CAPABILITIES")),
	})
	if err != nil {
		fatal(err.Error())
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := agent.Run(ctx); err != nil && err != context.Canceled {
		fatal(err.Error())
	}
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func splitValues(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func fatal(message string) {
	log.Print("cdn-edge-agent: " + message)
	fmt.Fprintln(os.Stderr, message)
	os.Exit(1)
}
