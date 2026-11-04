package parserengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/11bronx11/bx_yunpan/backend/internal/ai"
)

const (
	maxEngineResponseBytes = 16 << 20
	workerStartupTimeout   = 3 * time.Minute
	workerShutdownTimeout  = 10 * time.Second
)

type Worker struct {
	baseURL string
	client  *http.Client
	command *exec.Cmd
	done    chan struct{}
	waitErr error
	closeMu sync.Mutex
}

func Start(ctx context.Context, address, workerDir string, _ ...string) (*Worker, error) {
	workerDir, err := findWorkerDir(workerDir)
	if err != nil {
		return nil, err
	}
	command := exec.Command("python3", "-m", "parser_worker.server", "--host", address)
	command.Dir = filepath.Dir(workerDir)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start document parser worker: %w", err)
	}
	worker := &Worker{
		baseURL: "http://" + address,
		client:  &http.Client{},
		command: command,
		done:    make(chan struct{}),
	}
	go func() {
		worker.waitErr = command.Wait()
		close(worker.done)
	}()
	if err := worker.waitReady(ctx); err != nil {
		_ = worker.Close()
		return nil, err
	}
	return worker, nil
}

func findWorkerDir(configured string) (string, error) {
	candidates := []string{configured, "./parser_worker", "../backend/parser_worker"}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if info, err := os.Stat(filepath.Join(absolute, "server.py")); err == nil && !info.IsDir() {
			return absolute, nil
		}
	}
	return "", errors.New("document parser worker directory was not found")
}

func (w *Worker) waitReady(ctx context.Context) error {
	startupCtx, cancel := context.WithTimeout(ctx, workerStartupTimeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, _ := http.NewRequestWithContext(startupCtx, http.MethodGet, w.baseURL+"/health", nil)
		response, err := w.client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-w.done:
			return fmt.Errorf("document parser worker exited before readiness: %w", w.waitErr)
		case <-startupCtx.Done():
			return fmt.Errorf("wait for document parser worker: %w", startupCtx.Err())
		case <-ticker.C:
		}
	}
}

func (w *Worker) Close() error {
	if w == nil || w.command == nil || w.command.Process == nil {
		return nil
	}
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	select {
	case <-w.done:
		return nil
	default:
	}
	if err := w.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-w.done:
		var exitError *exec.ExitError
		if errors.As(w.waitErr, &exitError) && exitError.ExitCode() == -1 {
			return nil
		}
		return w.waitErr
	case <-time.After(workerShutdownTimeout):
		_ = w.command.Process.Kill()
		<-w.done
		return w.waitErr
	}
}

func (w *Worker) Parse(ctx context.Context, request ai.ParseRequest) (ai.ParseResult, error) {
	if request.SizeLimitBytes > 0 && int64(len(request.ObjectAddress.Data)) > request.SizeLimitBytes {
		return ai.ParseResult{}, &ai.ParseError{Code: "ai.parse.resource_limit", ResourceLimit: true, Err: errors.New("object exceeds size limit")}
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+"/parse", bytes.NewReader(request.ObjectAddress.Data))
	if err != nil {
		return ai.ParseResult{}, &ai.ParseError{Code: "ai.parse.invalid_request", Permanent: true, Err: err}
	}
	httpRequest.Header.Set("Content-Type", "application/octet-stream")
	httpRequest.Header.Set("X-Parser-Media-Type", request.MediaType)
	httpRequest.Header.Set("X-Content-Hash", request.ContentHash)
	httpRequest.Header.Set("X-Size-Limit-Bytes", fmt.Sprint(request.SizeLimitBytes))
	httpRequest.Header.Set("X-Page-Limit", fmt.Sprint(request.PageLimit))
	response, err := w.client.Do(httpRequest)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ai.ParseResult{}, &ai.ParseError{Code: "ai.parse.timeout", Retryable: true, Err: err}
		}
		return ai.ParseResult{}, &ai.ParseError{Code: "ai.parse.engine_unavailable", Retryable: true, Err: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxEngineResponseBytes+1))
	if err != nil {
		return ai.ParseResult{}, &ai.ParseError{Code: "ai.parse.engine_read_failed", Retryable: true, Err: err}
	}
	if len(body) > maxEngineResponseBytes {
		return ai.ParseResult{}, &ai.ParseError{Code: "ai.parse.resource_limit", ResourceLimit: true, Err: errors.New("parser response exceeds limit")}
	}
	if response.StatusCode != http.StatusOK {
		return ai.ParseResult{}, workerError(response.StatusCode, body)
	}
	var result ai.ParseResult
	if err := json.Unmarshal(body, &result); err != nil {
		return ai.ParseResult{}, &ai.ParseError{Code: "ai.parse.invalid_response", Retryable: true, Err: err}
	}
	return result, nil
}

type workerErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func workerError(statusCode int, body []byte) error {
	var value workerErrorBody
	_ = json.Unmarshal(body, &value)
	if value.Code == "" {
		value.Code = "ai.parse.failed"
	}
	var cause error
	if value.Message != "" {
		cause = errors.New(value.Message)
	}
	switch statusCode {
	case http.StatusBadRequest:
		return &ai.ParseError{Code: value.Code, Permanent: true, Err: cause}
	case http.StatusUnsupportedMediaType:
		return &ai.ParseError{Code: value.Code, Unsupported: true, Err: cause}
	case http.StatusRequestEntityTooLarge:
		return &ai.ParseError{Code: value.Code, ResourceLimit: true, Err: cause}
	case http.StatusUnprocessableEntity:
		return &ai.ParseError{Code: value.Code, Permanent: true, Err: cause}
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &ai.ParseError{Code: value.Code, Retryable: true, Err: cause}
	default:
		return &ai.ParseError{Code: value.Code, Retryable: true, Err: cause}
	}
}
