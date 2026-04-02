package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/roots/wp-packages/internal/app"
)

func ListenAndServe(a *app.App) error {
	router := NewRouter(a)

	csrfProtection := http.NewCrossOriginProtection()
	handler := csrfProtection.Handler(router)

	srv := &http.Server{
		Addr:         a.Config.Server.Addr,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ln, err := systemdListener()
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		addr := a.Config.Server.Addr
		if ln != nil {
			addr = ln.Addr().String()
		}
		a.Logger.Info("starting server", "addr", addr, "socket_activation", ln != nil)

		var serveErr error
		if ln != nil {
			serveErr = srv.Serve(ln)
		} else {
			serveErr = srv.ListenAndServe()
		}

		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- fmt.Errorf("server error: %w", serveErr)
		}
		close(errCh)
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-sigCtx.Done():
		a.Logger.Info("shutting down", "cause", context.Cause(sigCtx))
		stop()
	case err := <-errCh:
		if err != nil {
			sentry.CaptureException(err)
			sentry.Flush(2 * time.Second)
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		shutdownErr := fmt.Errorf("shutdown: %w", err)
		sentry.CaptureException(shutdownErr)
		sentry.Flush(2 * time.Second)
		return shutdownErr
	}

	a.Logger.Info("server stopped")
	return nil
}

func systemdListener() (net.Listener, error) {
	pidValue := os.Getenv("LISTEN_PID")
	fdsValue := os.Getenv("LISTEN_FDS")

	if pidValue == "" || fdsValue == "" {
		return nil, nil
	}

	pid, err := strconv.Atoi(pidValue)
	if err != nil {
		return nil, fmt.Errorf("invalid LISTEN_PID %q: %w", pidValue, err)
	}
	if pid != os.Getpid() {
		return nil, nil
	}

	fds, err := strconv.Atoi(fdsValue)
	if err != nil {
		return nil, fmt.Errorf("invalid LISTEN_FDS %q: %w", fdsValue, err)
	}
	if fds == 0 {
		return nil, nil
	}
	if fds > 1 {
		return nil, fmt.Errorf("expected exactly one systemd socket fd, got %d", fds)
	}

	_ = os.Unsetenv("LISTEN_PID")
	_ = os.Unsetenv("LISTEN_FDS")
	_ = os.Unsetenv("LISTEN_FDNAMES")

	file := os.NewFile(uintptr(3), "systemd-listen-fd")
	ln, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("using systemd socket fd: %w", err)
	}

	return ln, nil
}
