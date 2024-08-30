package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/events/v1/eventsv1connect"
	"github.com/tierklinik-dobersberg/apis/gen/go/tkd/idm/v1/idmv1connect"
	"github.com/tierklinik-dobersberg/apis/pkg/cli"
	"github.com/tierklinik-dobersberg/cis-cal/internal/config"
	"github.com/tierklinik-dobersberg/cis-cal/internal/repo"
)

type App struct {
	Config config.Config
	Users  idmv1connect.UserServiceClient
	Roles  idmv1connect.RoleServiceClient
	Events eventsv1connect.EventServiceClient

	repo.Service
}

func New(ctx context.Context, cfg config.Config) (*App, error) {

	service, err := repo.New(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare google calendar backend: %w", err)
	}

	app := &App{
		Service: service,

		Config: cfg,
		Users:  idmv1connect.NewUserServiceClient(http.DefaultClient, cfg.IdmURL),
		Roles:  idmv1connect.NewRoleServiceClient(http.DefaultClient, cfg.IdmURL),
		Events: eventsv1connect.NewEventServiceClient(cli.NewInsecureHttp2Client(), cfg.EventsServiceUrl),
	}

	return app, nil
}
