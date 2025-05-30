package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	golog "log"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/netutil"

	"github.com/imgproxy/imgproxy/v3/config"
	"github.com/imgproxy/imgproxy/v3/errorreport"
	"github.com/imgproxy/imgproxy/v3/ierrors"
	"github.com/imgproxy/imgproxy/v3/metrics"
	"github.com/imgproxy/imgproxy/v3/reuseport"
	"github.com/imgproxy/imgproxy/v3/router"
	"github.com/imgproxy/imgproxy/v3/vips"
)

var imgproxyIsRunningMsg = []byte("imgproxy is running")

func buildRouter() *router.Router {
	r := router.New(config.PathPrefix)

	r.GET("/", handleLanding, true)
	r.GET("", handleLanding, true)

	r.GET("/", withMetrics(withPanicHandler(withCORS(withSecret(handleProcessing)))), false)

	r.HEAD("/", withCORS(handleHead), false)
	r.OPTIONS("/", withCORS(handleHead), false)

	r.HealthHandler = handleHealth

	return r
}

func startServer(cancel context.CancelFunc) (*http.Server, error) {
	l, err := reuseport.Listen(config.Network, config.Bind)
	if err != nil {
		return nil, fmt.Errorf("Can't start server: %s", err)
	}

	if config.MaxClients > 0 {
		l = netutil.LimitListener(l, config.MaxClients)
	}

	errLogger := golog.New(
		log.WithField("source", "http_server").WriterLevel(log.ErrorLevel),
		"", 0,
	)

	s := &http.Server{
		Handler:        buildRouter(),
		ReadTimeout:    time.Duration(config.ReadRequestTimeout) * time.Second,
		MaxHeaderBytes: 1 << 20,
		ErrorLog:       errLogger,
	}

	if config.KeepAliveTimeout > 0 {
		s.IdleTimeout = time.Duration(config.KeepAliveTimeout) * time.Second
	} else {
		s.SetKeepAlivesEnabled(false)
	}

	go func() {
		log.Infof("Starting server at %s", config.Bind)
		if err := s.Serve(l); err != nil && err != http.ErrServerClosed {
			log.Error(err)
		}
		cancel()
	}()

	return s, nil
}

func shutdownServer(s *http.Server) {
	log.Info("Shutting down the server...")

	ctx, close := context.WithTimeout(context.Background(), 5*time.Second)
	defer close()

	s.Shutdown(ctx)
}

func withMetrics(h router.RouteHandler) router.RouteHandler {
	if !metrics.Enabled() {
		return h
	}

	return func(reqID string, rw http.ResponseWriter, r *http.Request) {
		ctx, metricsCancel, rw := metrics.StartRequest(r.Context(), rw, r)
		defer metricsCancel()

		h(reqID, rw, r.WithContext(ctx))
	}
}

func withCORS(h router.RouteHandler) router.RouteHandler {
	return func(reqID string, rw http.ResponseWriter, r *http.Request) {
		if len(config.AllowOrigin) > 0 {
			rw.Header().Set("Access-Control-Allow-Origin", config.AllowOrigin)
			rw.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		}

		h(reqID, rw, r)
	}
}

func withSecret(h router.RouteHandler) router.RouteHandler {
	if len(config.Secret) == 0 {
		return h
	}

	authHeader := []byte(fmt.Sprintf("Bearer %s", config.Secret))

	return func(reqID string, rw http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), authHeader) == 1 {
			h(reqID, rw, r)
		} else {
			panic(newInvalidSecretError())
		}
	}
}

func withPanicHandler(h router.RouteHandler) router.RouteHandler {
	return func(reqID string, rw http.ResponseWriter, r *http.Request) {
		ctx := errorreport.StartRequest(r)
		r = r.WithContext(ctx)

		errorreport.SetMetadata(r, "Request ID", reqID)

		defer func() {
			if rerr := recover(); rerr != nil {
				if rerr == http.ErrAbortHandler {
					panic(rerr)
				}

				err, ok := rerr.(error)
				if !ok {
					panic(rerr)
				}

				ierr := ierrors.Wrap(err, 0)

				if ierr.ShouldReport() {
					errorreport.Report(err, r)
				}

				router.LogResponse(reqID, r, ierr.StatusCode(), ierr)

				rw.Header().Set("Content-Type", "text/plain")
				rw.WriteHeader(ierr.StatusCode())

				if config.DevelopmentErrorsMode {
					rw.Write([]byte(ierr.Error()))
				} else {
					rw.Write([]byte(ierr.PublicMessage()))
				}
			}
		}()

		h(reqID, rw, r)
	}
}

func handleHealth(reqID string, rw http.ResponseWriter, r *http.Request) {
	var (
		status int
		msg    []byte
		ierr   *ierrors.Error
	)

	if err := vips.Health(); err == nil {
		status = http.StatusOK
		msg = imgproxyIsRunningMsg
	} else {
		status = http.StatusInternalServerError
		msg = []byte("Error")
		ierr = ierrors.Wrap(err, 1)
	}

	if len(msg) == 0 {
		msg = []byte{' '}
	}

	// Log response only if something went wrong
	if ierr != nil {
		router.LogResponse(reqID, r, status, ierr)
	}

	rw.Header().Set("Content-Type", "text/plain")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(status)
	rw.Write(msg)
}

func handleHead(reqID string, rw http.ResponseWriter, r *http.Request) {
	router.LogResponse(reqID, r, 200, nil)
	rw.WriteHeader(200)
}
