package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func run() error {
	flags := flag.NewFlagSet("remarkable-go", flag.ContinueOnError)
	ssh := flags.Bool("ssh", false, "Use SSH transport")
	sshKey := flags.String("ssh-key", "", "SSH private key path")
	usb := flags.Bool("usb", false, "Use USB web transport")
	local := flags.String("local-dir", "", "Read a local xochitl directory, or auto")
	readOnly := flags.Bool("read-only", false, "Disable library writes")
	write := flags.Bool("write", false, "Legacy write flag; writes are enabled by default")
	noFallback := flags.Bool("no-cloud-fallback", false, "Disable cloud fallback")
	serveHTTP := flags.Bool("http", false, "Serve Streamable HTTP")
	host := flags.String("host", os.Getenv("REMARKABLE_MCP_HOST"), "HTTP bind host")
	port := flags.Int("port", 8000, "HTTP port")
	register := flags.String("register", "", "Register a one-time device code")
	args := append([]string(nil), os.Args[1:]...)
	for i, arg := range args {
		if arg == "--local-dir" && (i+1 == len(args) || strings.HasPrefix(args[i+1], "-")) {
			args[i] = "--local-dir=auto"
		}
	}
	if value := os.Getenv("REMARKABLE_MCP_PORT"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid REMARKABLE_MCP_PORT: %w", err)
		}
		*port = parsed
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *readOnly && *write {
		return errors.New("--write and --read-only are mutually exclusive")
	}
	for key, enabled := range map[string]bool{"REMARKABLE_USE_SSH": *ssh, "REMARKABLE_USE_USB_WEB": *usb, "REMARKABLE_READ_ONLY": *readOnly, "REMARKABLE_DISABLE_CLOUD_FALLBACK": *noFallback} {
		if enabled {
			os.Setenv(key, "1")
		}
	}
	if *sshKey != "" {
		os.Setenv("REMARKABLE_SSH_KEY", *sshKey)
	}
	if *local != "" {
		os.Setenv("REMARKABLE_USE_LOCAL_DIR", "1")
		if *local != "auto" {
			os.Setenv("REMARKABLE_LOCAL_DIR", *local)
		}
	}
	if *register != "" {
		return registerDevice(*register)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := newApplication()
	if err != nil {
		return err
	}
	defer app.close()
	if !*serveHTTP {
		setHost := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "host" || f.Name == "port" {
				setHost = true
			}
		})
		if setHost {
			return errors.New("--host and --port require --http")
		}
		return app.server.Run(ctx, &mcp.StdioTransport{})
	}
	if *host == "" {
		*host = "127.0.0.1"
	}
	if *host == "0.0.0.0" || *host == "::" || *host == "[::]" || *port < 1 || *port > 65535 {
		return errors.New("HTTP requires a concrete host and port between 1 and 65535")
	}
	address := net.JoinHostPort(strings.Trim(*host, "[]"), strconv.Itoa(*port))
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return app.server }, nil)
	allowed := map[string]bool{strings.ToLower(address): true}
	if ip := net.ParseIP(strings.Trim(*host, "[]")); (*host == "localhost") || (ip != nil && ip.IsLoopback()) {
		for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
			allowed[net.JoinHostPort(h, strconv.Itoa(*port))] = true
		}
	}
	server := &http.Server{Addr: address, ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !allowed[strings.ToLower(req.Host)] {
			http.Error(w, "invalid Host", http.StatusForbidden)
			return
		}
		if origin := req.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !allowed[strings.ToLower(parsed.Host)] {
				http.Error(w, "invalid Origin", http.StatusForbidden)
				return
			}
		}
		handler.ServeHTTP(w, req)
	})}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
