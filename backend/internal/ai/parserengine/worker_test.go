package parserengine

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
)

func TestWorkerParseMapsParserHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnsupportedMediaType)
		_ = json.NewEncoder(writer).Encode(map[string]string{"code": "ai.parse.unsupported", "message": "unsupported"})
	}))
	defer server.Close()

	worker := &Worker{baseURL: server.URL, client: server.Client()}
	_, err := worker.Parse(context.Background(), ai.ParseRequest{ObjectAddress: ai.ObjectReadAddress{Data: []byte("content")}})
	if !ai.IsParseUnsupported(err) || ai.IsParseRetryable(err) {
		t.Fatalf("unsupported parser response was misclassified: %v", err)
	}
}

func TestWorkerErrorClassifiesBadRequestAsPermanent(t *testing.T) {
	err := workerError(http.StatusBadRequest, []byte(`{"code":"ai.parse.invalid_request"}`))
	if !ai.IsParsePermanent(err) || ai.IsParseRetryable(err) {
		t.Fatalf("bad parser request was misclassified: %v", err)
	}
}

func TestFindWorkerDirRequiresServerEntryPoint(t *testing.T) {
	directory := t.TempDir()
	if _, err := findWorkerDir(directory); err == nil {
		t.Fatal("expected missing server.py to be rejected")
	}
	if err := os.WriteFile(filepath.Join(directory, "server.py"), []byte("pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := findWorkerDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if found != directory {
		t.Fatalf("worker directory = %q, want %q", found, directory)
	}
}

func TestWorkerStartsPythonPackageFromItsParentDirectory(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required to start the parser worker")
	}
	root := t.TempDir()
	packageDir := filepath.Join(root, "parser_worker")
	if err := os.Mkdir(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageDir, "__init__.py"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const worker = `from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import argparse

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
    def log_message(self, *_args):
        pass

args = argparse.ArgumentParser()
args.add_argument("--host", required=True)
host, port = args.parse_args().host.rsplit(":", 1)
ThreadingHTTPServer((host, int(port)), Handler).serve_forever()
`
	if err := os.WriteFile(filepath.Join(packageDir, "server.py"), []byte(worker), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	workerProcess, err := Start(ctx, address, packageDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := workerProcess.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerCloseSignalsAndReapsChild(t *testing.T) {
	command := exec.Command("sleep", "60")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	worker := &Worker{command: command, done: make(chan struct{})}
	go func() {
		worker.waitErr = command.Wait()
		close(worker.done)
	}()

	started := time.Now()
	if err := worker.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > workerShutdownTimeout {
		t.Fatal("worker shutdown exceeded its grace period")
	}
	select {
	case <-worker.done:
	default:
		t.Fatal("child process was not reaped")
	}
}

func TestWaitReadyUsesHealthEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	worker := &Worker{baseURL: server.URL, client: server.Client(), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("health check consumed the startup deadline")
	}
}
