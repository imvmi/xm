package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"xm/internal/archive"
)

func Serve(ctx context.Context, path string, cfg archive.Config, s *archive.Store, address string, secure bool) error {
	l, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer l.Close()
	a, password, err := New(path, cfg, s, os.Getenv("XM_ADMIN_PASSWORD"), secure)
	if err != nil {
		return err
	}
	if password != "" {
		fmt.Fprintln(os.Stderr, "首次启动，管理员密码（仅显示一次，请保存）：", password)
	}
	fmt.Fprintln(os.Stderr, "XM 管理页面 · Local: http://"+l.Addr().String())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return ctx }}
	serverErr := make(chan error, 1)
	workerErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(l) }()
	go func() { workerErr <- a.Run(ctx) }()
	select {
	case <-ctx.Done():
	case err = <-serverErr:
	case err = <-workerErr:
		cancel()
		_ = server.Close()
		return err
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if e := server.Shutdown(shutdown); e != nil {
		_ = server.Close()
	}
	workerResult := <-workerErr
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return workerResult
}
