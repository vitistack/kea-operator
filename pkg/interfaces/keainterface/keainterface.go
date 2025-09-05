package keainterface

import (
	"context"

	"github.com/vitistack/kea-operator/pkg/models/keamodels"
)

type KeaClient interface {
	Send(ctx context.Context, cmd keamodels.Request) (keamodels.Response, error)
}
