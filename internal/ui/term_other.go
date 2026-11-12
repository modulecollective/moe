//go:build !linux && !darwin

package ui

import (
	"errors"
	"os"
)

// The console supports Linux and Darwin terminals; elsewhere it refuses
// before touching the terminal.
var errPlatform = errors.New("moe ui supports Linux and macOS terminals only")

type termState struct{}

func openTTY() (*os.File, error)             { return nil, errPlatform }
func getTermios(*os.File) (termState, error) { return termState{}, errPlatform }
func setTermios(*os.File, termState) error   { return errPlatform }
func rawTermios(s termState) termState       { return s }
