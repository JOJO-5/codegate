//go:build !windows && !linux && !darwin

package terminal

import (
	"context"
	"errors"
)

var errUnixNotImplemented = errors.New("terminal: 此平台 PTY 尚未实现")

func Available() error { return errUnixNotImplemented }
func New() Terminal { return &unsupportedPTY{} }

type unsupportedPTY struct{}

func (*unsupportedPTY) Start(context.Context, StartConfig) error { return errUnixNotImplemented }
func (*unsupportedPTY) Read([]byte) (int, error) { return 0, errUnixNotImplemented }
func (*unsupportedPTY) Write([]byte) (int, error) { return 0, errUnixNotImplemented }
func (*unsupportedPTY) Resize(uint16, uint16) error { return errUnixNotImplemented }
func (*unsupportedPTY) Signal(Signal) error { return errUnixNotImplemented }
func (*unsupportedPTY) Wait() ExitResult { return ExitResult{Err: errUnixNotImplemented} }
func (*unsupportedPTY) Close() error { return nil }
func (*unsupportedPTY) PID() int { return 0 }
