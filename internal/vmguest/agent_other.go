//go:build !linux

package vmguest

import (
	"errors"
	"log/slog"
)

func Main(*slog.Logger) error {
	return errors.New("the vm-per-job guest agent runs only in a Linux guest")
}
