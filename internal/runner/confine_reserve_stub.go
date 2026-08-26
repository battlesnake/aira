//go:build !linux

package runner

import (
	"context"
	"errors"
)

func confineReserve(context.Context, ConfineReserveRequest) (*ConfineReservation, error) {
	return nil, errors.New("E_CONFINE_UNAVAILABLE: confine reservations require Linux")
}
